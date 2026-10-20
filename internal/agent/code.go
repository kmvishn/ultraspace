package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"ultraspace/internal/sysmem"
)

// codeServer runs VS Code (code-server) on demand and stops it when nobody has used it for a while.
type codeServer struct {
	bin      string
	sock     string
	stateDir string
	idle     time.Duration
	profile  string
	log      *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	starting chan struct{}
	active   int
	last     time.Time

	proxy *httputil.ReverseProxy
}

func newCodeServer(bin, runtimeDir, stateDir string, idle time.Duration, log *slog.Logger) *codeServer {
	c := &codeServer{bin: bin, sock: filepath.Join(runtimeDir, "code-server.sock"), stateDir: stateDir, idle: idle, log: log}
	target, _ := url.Parse("http://code-server")
	c.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // code-server compares Origin with Host for its sockets
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/code")
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.sock)
			},
			MaxIdleConns: 16, IdleConnTimeout: 60 * time.Second,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			c.log.Warn("code-server proxy", "err", err)
			http.Error(w, "VS Code is not responding: "+err.Error(), http.StatusBadGateway)
		},
	}
	return c
}

func (c *codeServer) available() bool {
	st, err := os.Stat(c.bin)
	return err == nil && st.Mode()&0o111 != 0
}

func (c *codeServer) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil
}

// ensure starts code-server if needed and waits until it accepts connections.
func (c *codeServer) ensure(ctx context.Context) error {
	c.mu.Lock()
	if c.cmd != nil {
		c.mu.Unlock()
		return nil
	}
	if c.starting != nil {
		wait := c.starting
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !c.running() {
			return fmt.Errorf("VS Code failed to start")
		}
		return nil
	}
	if !c.available() {
		c.mu.Unlock()
		return fmt.Errorf("VS Code (code-server) is not installed at %s", c.bin)
	}
	c.starting = make(chan struct{})
	c.mu.Unlock()

	err := c.start()
	c.mu.Lock()
	close(c.starting)
	c.starting = nil
	c.mu.Unlock()
	return err
}

// lightSettings make VS Code use noticeably less memory (measured: about 80 MB less
// with a small project) without changing how editing works.
var lightSettings = map[string]any{
	"chat.disableAIFeatures":      true, // no AI agent host process
	"workbench.enableExperiments": false,
	"extensions.autoCheckUpdates": false,
	"telemetry.telemetryLevel":    "off",
	// code-server runs with a small Node heap setting; programs in VS Code's own terminal should not inherit it.
	"terminal.integrated.env.linux": map[string]any{"NODE_OPTIONS": nil},
}

// seedSettings gives VS Code a dark theme and the memory-saving settings above. A new
// settings file gets everything; an existing one gets only the keys it lacks, once
// (a marker file records that), so later changes by the user are never overridden.
func seedSettings(stateDir string, light bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".local", "share", "code-server", "User")
	path := filepath.Join(dir, "settings.json")
	marker := filepath.Join(stateDir, "vscode-defaults-1")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &settings) != nil {
			return // has comments or is hand-formatted JSONC: leave it alone
		}
	} else {
		settings["workbench.colorTheme"] = "Default Dark Modern"
	}
	if light {
		for k, v := range lightSettings {
			if _, ok := settings[k]; !ok {
				settings[k] = v
			}
		}
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	if os.WriteFile(path+".tmp", append(b, '\n'), 0o600) == nil && os.Rename(path+".tmp", path) == nil {
		_ = os.WriteFile(marker, nil, 0o600)
	}
}

func (c *codeServer) start() error {
	seedSettings(c.stateDir, true)
	_ = os.Remove(c.sock)
	cmd := exec.Command(c.bin,
		"--auth", "none", // ultraspace already checked the login
		"--socket", c.sock, "--socket-mode", "600",
		"--disable-telemetry", "--disable-update-check", "--disable-workspace-trust",
		"--app-name", "ultraspace",
	)
	home, _ := os.UserHomeDir()
	cmd.Dir = home
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PASSWORD=") && !strings.HasPrefix(kv, "HASHED_PASSWORD=") && !strings.HasPrefix(kv, "NODE_OPTIONS=") {
			env = append(env, kv)
		}
	}
	// A small young generation trims every Node process of code-server (measured: ~60 MB).
	env = append(env, "NODE_OPTIONS=--max-semi-space-size=1")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start code-server: %w", err)
	}
	sysmem.PreferOOMKill(cmd.Process.Pid, 500)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		c.log.Info("code-server exited", "err", err)
		c.mu.Lock()
		if c.cmd == cmd {
			c.cmd = nil
		}
		c.mu.Unlock()
		close(exited)
	}()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("code-server exited while starting")
		case <-time.After(250 * time.Millisecond):
		}
		if conn, err := net.Dial("unix", c.sock); err == nil {
			conn.Close()
			c.mu.Lock()
			c.cmd, c.exited, c.last = cmd, exited, time.Now()
			c.mu.Unlock()
			c.log.Info("code-server started", "pid", cmd.Process.Pid)
			return nil
		}
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return fmt.Errorf("code-server did not start in time")
}

func (c *codeServer) stop() {
	c.mu.Lock()
	cmd, exited := c.cmd, c.exited
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// idleLoop stops code-server once no window has used it for the idle period.
func (c *codeServer) idleLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stop()
			return
		case <-t.C:
		}
		c.mu.Lock()
		quiet := c.cmd != nil && c.active == 0 && time.Since(c.last) > c.idle
		c.mu.Unlock()
		if quiet {
			c.log.Info("code-server idle; stopping")
			c.stop()
		}
	}
}

func (c *codeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 70*time.Second)
	err := c.ensure(ctx)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	c.mu.Lock()
	c.last = time.Now()
	if upgrade {
		c.active++
	}
	c.mu.Unlock()
	if upgrade {
		defer func() {
			c.mu.Lock()
			c.active--
			c.last = time.Now()
			c.mu.Unlock()
		}()
	}
	c.proxy.ServeHTTP(w, r)
}
