package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// Backup and the release check both live behind the account, because the
// backup holds every account and the check needs the running version.

func registerAccountExtra(mux *http.ServeMux, st *store.Store, a *auth.Auth, version string) {
	checker := &releaseChecker{version: version}
	mux.Handle("POST /api/account/backup", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleBackup(w, r, st)
	})))
	mux.Handle("GET /api/system/version-check", a.Require(http.HandlerFunc(checker.handle)))
}

func handleBackup(w http.ResponseWriter, r *http.Request, st *store.Store) {
	var req struct {
		Current string `json:"current"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Current)) != nil {
		httpx.Error(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	dir, err := os.MkdirTemp("", "ultraspace-backup-*")
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "backup failed")
		return
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "ultraspace.db")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := st.BackupTo(ctx, dbPath); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "backup failed")
		return
	}
	f, err := os.Open(dbPath)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "backup failed")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "backup failed")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="ultraspace-backup.zip"`)
	zw := zip.NewWriter(w)
	hdr := &zip.FileHeader{Name: "ultraspace.db", Method: zip.Deflate}
	hdr.SetModTime(info.ModTime())
	out, err := zw.CreateHeader(hdr)
	if err == nil {
		_, err = io.Copy(out, f)
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// The headers may already be sent; nothing useful left to report.
		return
	}
}

// ---- release check (notify only; updating stays a command on the server) ----

type versionInfo struct {
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	Update  bool   `json:"update_available"`
	URL     string `json:"url,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Error   string `json:"error,omitempty"`
}

type releaseChecker struct {
	version string
	mu      sync.Mutex
	at      time.Time
	info    versionInfo
	fetch   func(ctx context.Context) (versionInfo, error)
}

func (c *releaseChecker) handle(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info.Current == "" || time.Since(c.at) > time.Hour {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		info, err := c.fetchLatest(ctx)
		cancel()
		if err != nil {
			info = versionInfo{Current: c.version, Error: "could not check for a newer release"}
		}
		c.info, c.at = info, time.Now()
	}
	httpx.WriteJSON(w, http.StatusOK, c.info)
}

func (c *releaseChecker) fetchLatest(ctx context.Context) (versionInfo, error) {
	if c.fetch != nil {
		return c.fetch(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/kmvishn/ultraspace/releases/latest", nil)
	if err != nil {
		return versionInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ultraspace")
	res, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return versionInfo{}, err
	}
	defer res.Body.Close()
	var rel struct {
		Tag  string `json:"tag_name"`
		URL  string `json:"html_url"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rel); err != nil {
		return versionInfo{}, err
	}
	notes := strings.TrimSpace(rel.Body)
	if len(notes) > 2000 {
		notes = notes[:2000]
	}
	return versionInfo{
		Current: c.version, Latest: rel.Tag,
		Update: updateAvailable(c.version, rel.Tag),
		URL:    rel.URL, Notes: notes,
	}, nil
}

// updateAvailable compares the vX.Y.Z part of both versions. Anything it
// cannot read (a dev build, a dirty tree) simply reports no update.
func updateAvailable(current, latest string) bool {
	cur, ok1 := parseRelease(current)
	lat, ok2 := parseRelease(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range cur {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

func parseRelease(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
