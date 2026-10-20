// Package agent gives the ultraspace web desktop a terminal and VS Code that run as a
// real Linux user. "ultraspace agent" runs as that user (a separate systemd service)
// and serves on a Unix socket that only the ultraspace service may open; the main
// server checks the login and proxies Terminal and VS Code traffic to it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Options struct {
	// Socket is used when systemd does not pass a listening socket.
	Socket     string
	CodeServer string
	CodeIdle   time.Duration
	Profile    string
}

// Run serves the agent API until ctx is cancelled.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	ln, err := listener(opts.Socket)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".local", "share", "ultraspace")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if opts.CodeIdle <= 0 {
		opts.CodeIdle = 20 * time.Minute
	}
	t := newTerms()
	// systemd's RuntimeDirectory keeps the socket path short (Unix sockets allow ~107 bytes).
	runtime := os.Getenv("RUNTIME_DIRECTORY")
	if runtime == "" {
		runtime = state
	}
	code := newCodeServer(opts.CodeServer, runtime, state, opts.CodeIdle, log)
	code.profile = opts.Profile
	go code.idleLoop(ctx)

	mux := http.NewServeMux()
	t.register(mux)
	mux.Handle("/code/", code)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		n := len(t.sessions)
		t.mu.Unlock()
		user := os.Getenv("USER")
		writeJSON(w, http.StatusOK, map[string]any{
			"user": user, "home": home, "sessions": n,
			"code_available": code.available(), "code_running": code.running(),
		})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		t.closeAll()
	}()
	log.Info("agent listening", "addr", ln.Addr().String(), "user", os.Getenv("USER"))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listener takes the socket systemd passed (socket activation), or creates one.
func listener(path string) (net.Listener, error) {
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid == os.Getpid() && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		ln, err := net.FileListener(f)
		f.Close()
		return ln, err
	}
	if path == "" {
		return nil, fmt.Errorf("no socket: run under systemd socket activation or pass -socket")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o600)
}
