// Package webdl downloads files from the web straight into Files on the server, for
// example a Stremio web stream saved for later. Only public addresses are fetched, and
// files are written through os.Root, so a name can never place them outside Files.
//
// The list lives in memory: downloads in progress stop when ultraspace restarts (the
// partial ".part" file stays and is replaced by the next try).
package webdl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/httpx"
	"ultraspace/internal/mediasrc"
)

const (
	folder    = "Downloads"
	maxActive = 3
	keepDone  = 50
)

type job struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Path     string  `json:"path"` // in Files
	Host     string  `json:"host"`
	Size     int64   `json:"size"` // -1 when unknown
	Have     int64   `json:"have"`
	Rate     float64 `json:"rate"`
	State    string  `json:"state"` // queued, downloading, done, failed, cancelled
	Error    string  `json:"error,omitempty"`
	Added    int64   `json:"added"`
	url      string
	cancel   context.CancelFunc
	lastHave int64
}

type Manager struct {
	root *os.Root
	log  *slog.Logger
	mu   sync.Mutex
	jobs []*job
}

func New(filesRoot string, log *slog.Logger) (*Manager, error) {
	root, err := os.OpenRoot(filesRoot)
	if err != nil {
		return nil, err
	}
	return &Manager{root: root, log: log.With("component", "webdl")}, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	m.mu.Unlock()
	m.root.Close()
}

func (m *Manager) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/downloads", require(http.HandlerFunc(m.handleList)))
	mux.Handle("POST /api/downloads", require(http.HandlerFunc(m.handleAdd)))
	mux.Handle("POST /api/downloads/{id}/{action}", require(http.HandlerFunc(m.handleAction)))
}

// cleanName makes a safe single file name.
func cleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.Trim(name, ". ")
	if len(name) > 180 {
		ext := path.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	if name == "" {
		name = "download"
	}
	return name
}

func (m *Manager) handleAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 16<<10) {
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		httpx.Error(w, http.StatusBadRequest, "only http and https addresses can be downloaded")
		return
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	j := &job{ID: hex.EncodeToString(b[:]), Name: cleanName(req.Name), Host: u.Hostname(), Size: -1, State: "queued", Added: time.Now().UnixMilli(), url: u.String()}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.trimLocked()
	m.mu.Unlock()
	go m.next()
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": j.ID})
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	out := make([]job, 0, len(m.jobs))
	for i := len(m.jobs) - 1; i >= 0; i-- {
		out = append(out, *m.jobs[i])
	}
	m.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"downloads": out})
}

func (m *Manager) handleAction(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.jobs, func(j *job) bool { return j.ID == r.PathValue("id") })
	if i < 0 {
		httpx.Error(w, http.StatusNotFound, "no such download")
		return
	}
	j := m.jobs[i]
	switch r.PathValue("action") {
	case "cancel":
		if j.cancel != nil {
			j.cancel()
		}
		if j.State == "queued" {
			j.State = "cancelled"
		}
	case "forget":
		if j.State == "downloading" || j.State == "queued" {
			httpx.Error(w, http.StatusConflict, "cancel it first")
			return
		}
		m.jobs = slices.Delete(m.jobs, i, i+1)
	default:
		httpx.Error(w, http.StatusBadRequest, "unknown action")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// trimLocked forgets the oldest finished downloads beyond keepDone.
func (m *Manager) trimLocked() {
	for len(m.jobs) > keepDone {
		i := slices.IndexFunc(m.jobs, func(j *job) bool { return j.State != "downloading" && j.State != "queued" })
		if i < 0 {
			return
		}
		m.jobs = slices.Delete(m.jobs, i, i+1)
	}
}

// next starts queued downloads while fewer than maxActive run.
func (m *Manager) next() {
	m.mu.Lock()
	active := 0
	var start *job
	for _, j := range m.jobs {
		switch {
		case j.State == "downloading":
			active++
		case j.State == "queued" && start == nil:
			start = j
		}
	}
	if start == nil || active >= maxActive {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	start.State, start.cancel = "downloading", cancel
	m.mu.Unlock()
	go func() {
		err := m.run(ctx, start)
		cancelled := ctx.Err() != nil
		cancel()
		m.mu.Lock()
		switch {
		case err == nil:
			start.State, start.Rate = "done", 0
		case cancelled:
			start.State, start.Rate = "cancelled", 0
		default:
			start.State, start.Error, start.Rate = "failed", err.Error(), 0
		}
		start.cancel = nil
		m.mu.Unlock()
		if err != nil && !cancelled {
			m.log.Warn("download failed", "name", start.Name, "err", err)
		}
		m.next()
	}()
	go m.next() // there may be room for more
}

func (m *Manager) run(ctx context.Context, j *job) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) ultraspace")
	res, err := mediasrc.PublicClient().Do(req)
	if err != nil {
		return errors.New("could not reach " + j.Host)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", j.Host, res.Status)
	}
	name := j.Name
	if path.Ext(name) == "" {
		// Take the extension from the server's file name or media type.
		if _, p, err := mime.ParseMediaType(res.Header.Get("Content-Disposition")); err == nil && path.Ext(p["filename"]) != "" {
			name += path.Ext(p["filename"])
		} else if ext := path.Ext(res.Request.URL.Path); ext != "" && len(ext) <= 6 {
			name += ext
		} else if exts, _ := mime.ExtensionsByType(res.Header.Get("Content-Type")); len(exts) > 0 {
			name += exts[0]
		}
		name = cleanName(name)
	}
	if err := m.root.MkdirAll(folder, 0o750); err != nil {
		return err
	}
	final := m.unusedName(name)
	part := final + ".part"
	m.mu.Lock()
	j.Name, j.Path, j.Size = path.Base(final), "/"+final, res.ContentLength
	m.mu.Unlock()
	out, err := m.root.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go m.measure(j, done)
	_, err = io.Copy(out, &counter{r: res.Body, m: m, j: j})
	close(done)
	if err = errors.Join(err, out.Close()); err != nil {
		_ = m.root.Remove(part)
		return err
	}
	return m.root.Rename(part, final)
}

// unusedName returns Downloads/name, or "name (2)" etc. when that exists.
func (m *Manager) unusedName(name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	free := func(p string) bool {
		_, e1 := m.root.Stat(p)
		_, e2 := m.root.Stat(p + ".part")
		return errors.Is(e1, os.ErrNotExist) && errors.Is(e2, os.ErrNotExist)
	}
	for i := 1; i <= 100; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		if p := path.Join(folder, n); free(p) {
			return p
		}
	}
	return path.Join(folder, fmt.Sprintf("%s (%d)%s", base, time.Now().UnixNano(), ext))
}

func (m *Manager) measure(j *job, done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			m.mu.Lock()
			j.Rate = 0.6*j.Rate + 0.4*float64(j.Have-j.lastHave)
			j.lastHave = j.Have
			m.mu.Unlock()
		}
	}
}

type counter struct {
	r io.Reader
	m *Manager
	j *job
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.m.mu.Lock()
	c.j.Have += int64(n)
	c.m.mu.Unlock()
	return n, err
}
