package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// scrollback is how much recent output a session keeps, replayed when a window
// (re)attaches, so you can pick a terminal up on another device.
const scrollback = 512 << 10

type session struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Cols    int       `json:"cols"`
	Rows    int       `json:"rows"`
	Exited  bool      `json:"exited"`

	cmd  *exec.Cmd
	tty  *os.File
	mu   sync.Mutex
	buf  []byte // ring of the newest output
	subs map[chan []byte]struct{}
	done chan struct{}
}

type terms struct {
	mu       sync.Mutex
	sessions map[string]*session
	shell    string
	home     string
}

func newTerms() *terms {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	home, _ := os.UserHomeDir()
	return &terms{sessions: map[string]*session{}, shell: shell, home: home}
}

func (t *terms) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /term/sessions", t.list)
	mux.HandleFunc("POST /term/sessions", t.create)
	mux.HandleFunc("DELETE /term/sessions/{id}", t.kill)
	mux.HandleFunc("GET /term/sessions/{id}/ws", t.attach)
}

func (t *terms) list(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	out := make([]session, 0, len(t.sessions))
	for _, s := range t.sessions {
		s.mu.Lock()
		out = append(out, session{ID: s.ID, Created: s.Created, Cols: s.Cols, Rows: s.Rows, Exited: s.Exited})
		s.mu.Unlock()
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (t *terms) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	req.Cols, req.Rows = clamp(req.Cols, 20, 500, 100), clamp(req.Rows, 5, 300, 30)

	t.mu.Lock()
	if len(t.sessions) >= 20 {
		t.mu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many terminal sessions; close some first"})
		return
	}
	t.mu.Unlock()

	cmd := exec.Command(t.shell, "-l")
	cmd.Dir = t.home
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=ultraspace")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(req.Cols), Rows: uint16(req.Rows)})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s := &session{ID: randomID(), Created: time.Now(), Cols: req.Cols, Rows: req.Rows, cmd: cmd, tty: tty,
		subs: map[chan []byte]struct{}{}, done: make(chan struct{})}
	t.mu.Lock()
	t.sessions[s.ID] = s
	t.mu.Unlock()
	go s.pump()
	go func() {
		_ = cmd.Wait()
		<-s.done
		t.mu.Lock()
		delete(t.sessions, s.ID)
		t.mu.Unlock()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"id": s.ID, "created": s.Created, "cols": s.Cols, "rows": s.Rows})
}

func (t *terms) kill(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	s := t.sessions[r.PathValue("id")]
	t.mu.Unlock()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	s.terminate()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (t *terms) attach(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	s := t.sessions[r.PathValue("id")]
	t.mu.Unlock()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	// The socket is only reachable by the ultraspace service, which checked the login
	// and the page origin before proxying here.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ch := make(chan []byte, 256)
	s.mu.Lock()
	replay := append([]byte(nil), s.buf...)
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	go func() {
		defer cancel()
		if len(replay) > 0 && ws.Write(ctx, websocket.MessageBinary, replay) != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				_ = ws.Write(ctx, websocket.MessageText, []byte(`{"t":"exit"}`))
				ws.Close(websocket.StatusNormalClosure, "session ended")
				return
			case b := <-ch:
				if ws.Write(ctx, websocket.MessageBinary, b) != nil {
					return
				}
			}
		}
	}()

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if _, err := s.tty.Write(data); err != nil {
				return
			}
			continue
		}
		var m struct {
			T    string `json:"t"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &m) == nil && m.T == "resize" {
			s.resize(clamp(m.Cols, 20, 500, s.Cols), clamp(m.Rows, 5, 300, s.Rows))
		}
	}
}

// pump copies shell output to the scrollback and every attached window.
func (s *session) pump() {
	defer close(s.done)
	b := make([]byte, 32<<10)
	for {
		n, err := s.tty.Read(b)
		if n > 0 {
			chunk := append([]byte(nil), b[:n]...)
			s.mu.Lock()
			s.buf = append(s.buf, chunk...)
			if len(s.buf) > scrollback {
				s.buf = append([]byte(nil), s.buf[len(s.buf)-scrollback:]...)
			}
			for ch := range s.subs {
				select {
				case ch <- chunk:
				default: // a stuck viewer misses output rather than blocking the shell
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			s.Exited = true
			s.mu.Unlock()
			s.tty.Close()
			return
		}
	}
}

func (s *session) resize(cols, rows int) {
	s.mu.Lock()
	s.Cols, s.Rows = cols, rows
	s.mu.Unlock()
	_ = pty.Setsize(s.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (s *session) terminate() {
	if s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGHUP)
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
	}
}

func (t *terms) closeAll() {
	t.mu.Lock()
	list := make([]*session, 0, len(t.sessions))
	for _, s := range t.sessions {
		list = append(list, s)
	}
	t.mu.Unlock()
	for _, s := range list {
		s.terminate()
	}
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(hi, v))
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

