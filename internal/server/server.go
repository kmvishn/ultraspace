// Package server wires the modules into one HTTP server.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ultraspace/internal/agent"
	"ultraspace/internal/auth"
	"ultraspace/internal/browser"
	"ultraspace/internal/config"
	"ultraspace/internal/files"
	"ultraspace/internal/httpx"
	"ultraspace/internal/monitor"
	"ultraspace/internal/navidrome"
	"ultraspace/internal/remote"
	"ultraspace/internal/services"
	"ultraspace/internal/state"
	"ultraspace/internal/store"
	"ultraspace/web"
)

// New builds the full HTTP handler. The returned cleanup closes the store and file root.
func New(ctx context.Context, cfg config.Config, version string, log *slog.Logger) (http.Handler, func(), error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ultraspace.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	fm, err := files.New(cfg.FilesRoot, log)
	if err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("open files root: %w", err)
	}
	rb := remote.New(remote.Options{
		ChromePath: cfg.Browser.Chrome, DataDir: cfg.DataDir, FilesRoot: cfg.FilesRoot,
		Idle: cfg.Browser.Idle.Duration, NoSandbox: cfg.Browser.NoSandbox, Quality: cfg.Browser.Quality,
	}, log)
	cleanup := func() { rb.Shutdown(); fm.Close(); st.Close() }

	proxy := httpx.Proxy{Trust: cfg.TrustProxy}
	a, err := auth.New(st, auth.Options{
		TTL:            cfg.SessionTTL.Duration,
		SetupTokenPath: filepath.Join(cfg.DataDir, "setup-token"),
		Proxy:          proxy,
		Log:            log,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	mon := monitor.New(version, []monitor.Disk{{Label: "System", Path: "/"}, {Label: "Files", Path: cfg.FilesRoot}})
	go mon.Run(ctx)
	go a.PurgeLoop(ctx)

	mux := http.NewServeMux()
	a.Register(mux)
	fm.Register(mux, a.Require)
	mon.Register(mux, a.Require)
	services.New(log).Register(mux, a.Require)
	state.New(st, log).Register(mux, a.Require)
	browser.New(log).Register(mux, a.Require)
	rb.Register(mux, a.Require)
	navidrome.New(st, log).Register(mux, a.Require)
	agent.NewClient(cfg.AgentSocket, proxy, cfg.PublicOrigin).Register(mux, a.Require)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, http.StatusNotFound, "not found")
	})
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	mux.Handle("/", readOnly(noCache(http.FileServerFS(static))))

	h := securityHeaders(csrf(cfg.PublicOrigin, proxy, logRequests(log, mux)))
	return h, cleanup, nil
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg config.Config, version string, log *slog.Logger) error {
	h, cleanup, err := New(ctx, cfg, version, log)
	if err != nil {
		return err
	}
	defer cleanup()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No read/write timeouts: uploads and downloads can be large and slow.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "version", version, "files_root", cfg.FilesRoot)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// style-src allows inline styles because the terminal (xterm.js) themes itself with a
// <style> element; scripts stay strictly 'self'.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; " +
	"media-src 'self'; connect-src 'self'; frame-src 'self' https:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if strings.HasPrefix(r.URL.Path, "/code/") {
			// VS Code brings its own CSP; the agent proxy adds frame-ancestors 'self'.
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// csrf rejects cross-site state changes. The app sends X-Ultraspace on every
// request; browsers cannot add that header cross-origin without CORS, which we never allow.
func csrf(public string, proxy httpx.Proxy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				httpx.Error(w, http.StatusForbidden, "cross-site request rejected")
				return
			}
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		}
		// VS Code makes its own requests without our header; for it, require proof the
		// request came from this site (the agent proxy checks Origin as well).
		if strings.HasPrefix(r.URL.Path, "/code/") {
			if r.Header.Get("Sec-Fetch-Site") == "same-origin" || r.Header.Get("Origin") == proxy.Origin(r) || (public != "" && r.Header.Get("Origin") == public) {
				next.ServeHTTP(w, r)
			} else {
				httpx.Error(w, http.StatusForbidden, "cross-site request rejected")
			}
			return
		}
		if r.Header.Get("X-Ultraspace") != "1" {
			httpx.Error(w, http.StatusForbidden, "missing request header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != proxy.Origin(r) && (public == "" || o != public) {
			httpx.Error(w, http.StatusForbidden, "origin not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/system") && sw.status == http.StatusOK {
			return // polled every few seconds; too noisy to log
		}
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
	})
}
