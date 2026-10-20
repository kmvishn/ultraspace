// Package files is the file manager API. Every operation goes through os.Root,
// so paths (including symlinks) cannot escape the configured directory.
package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ultraspace/internal/httpx"
)

var errBadPath = errors.New("invalid path")

type Files struct {
	root *os.Root
	log  *slog.Logger
}

func New(dir string, log *slog.Logger) (*Files, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Files{root: root, log: log}, nil
}

func (f *Files) Close() error { return f.root.Close() }

func (f *Files) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, auth(h)) }
	handle("GET /api/files/list", f.handleList)
	handle("GET /api/files/download", f.handleDownload)
	handle("POST /api/files/upload", f.handleUpload)
	handle("POST /api/files/mkdir", f.handleMkdir)
	handle("POST /api/files/rename", f.handleRename)
	handle("POST /api/files/delete", f.handleDelete)
	handle("POST /api/files/write", f.handleWrite)
	handle("GET /api/files/search", f.handleSearch)
}

const maxTextWrite = 5 << 20

func (f *Files) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, maxTextWrite+4096) {
		return
	}
	p, err := rel(req.Path)
	if err != nil || p == "." || !validName(path.Base(p)) {
		f.fail(w, errBadPath)
		return
	}
	if st, err := f.root.Stat(p); err == nil && st.IsDir() {
		httpx.Error(w, http.StatusBadRequest, "cannot write to a folder")
		return
	}
	dir, name := path.Split(p)
	if dir == "" {
		dir = "."
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := path.Join(dir, ".ultraspace-write-"+hex.EncodeToString(rnd[:]))
	out, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		f.fail(w, err)
		return
	}
	_, werr := io.WriteString(out, req.Content)
	if err := errors.Join(werr, out.Close()); err != nil {
		_ = f.root.Remove(tmp)
		f.fail(w, err)
		return
	}
	// Rename over the original so a crash never leaves a half-written file.
	if err := f.root.Rename(tmp, path.Join(dir, name)); err != nil {
		_ = f.root.Remove(tmp)
		f.fail(w, err)
		return
	}
	st, _ := f.root.Stat(p)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "size": st.Size(), "mtime": st.ModTime()})
}

var kindExts = map[string][]string{
	"audio": {".mp3", ".flac", ".ogg", ".oga", ".opus", ".wav", ".m4a", ".aac"},
	"image": {".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp"},
	"video": {".mp4", ".webm", ".mov", ".m4v", ".ogv"},
}

const maxSearchResults = 5000

type found struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// handleSearch walks the whole files root. Symlinked folders are not followed.
func (f *Files) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	kind := r.URL.Query().Get("kind")
	exts, kindOK := kindExts[kind]
	if kind != "" && !kindOK {
		httpx.Error(w, http.StatusBadRequest, "unknown kind")
		return
	}
	if q == "" && kind == "" {
		httpx.Error(w, http.StatusBadRequest, "give a query or a kind")
		return
	}
	limit := maxSearchResults
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < limit {
		limit = n
	}
	results := []found{}
	truncated := false
	err := fs.WalkDir(f.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if p == "." || strings.HasPrefix(d.Name(), ".ultraspace-") {
			return nil
		}
		name := strings.ToLower(d.Name())
		if q != "" && !strings.Contains(name, q) {
			return nil
		}
		if kind != "" && (d.IsDir() || !slices.Contains(exts, path.Ext(name))) {
			return nil
		}
		if len(results) >= limit {
			truncated = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		e := found{Path: "/" + p, Name: d.Name(), Dir: d.IsDir(), ModTime: info.ModTime()}
		if !d.IsDir() {
			e.Size = info.Size()
		}
		results = append(results, e)
		return nil
	})
	if err != nil {
		f.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results, "truncated": truncated})
}

// rel turns a client path like "/a/b" into a root-relative path ("a/b", or "." for the root).
func rel(p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", errBadPath
	}
	c := path.Clean("/" + p)
	if c == "/" {
		return ".", nil
	}
	return c[1:], nil
}

// validName checks a single path element supplied by the client.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 &&
		!strings.ContainsAny(name, "/\\\x00")
}

type entry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Link    bool      `json:"link,omitempty"`
}

func (f *Files) handleList(w http.ResponseWriter, r *http.Request) {
	p, err := rel(r.URL.Query().Get("path"))
	if err != nil {
		f.fail(w, err)
		return
	}
	d, err := f.root.Open(p)
	if err != nil {
		f.fail(w, err)
		return
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		f.fail(w, err)
		return
	}
	out := make([]entry, 0, len(des))
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".ultraspace-") {
			continue // in-progress uploads and saves
		}
		e := entry{Name: de.Name(), Dir: de.IsDir()}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if de.Type()&fs.ModeSymlink != 0 {
			e.Link = true
			// Follow the link inside the root; links pointing outside are listed but unusable.
			if target, err := f.root.Stat(path.Join(p, de.Name())); err == nil {
				info = target
				e.Dir = target.IsDir()
			}
		}
		e.Size, e.ModTime = info.Size(), info.ModTime()
		if e.Dir {
			e.Size = 0
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"path": "/" + strings.TrimPrefix(p, "."), "entries": out})
}

// previewable lists types the browser may render inline. Everything else downloads.
func previewable(ctype string) bool {
	switch {
	case ctype == "image/svg+xml":
		return false
	case strings.HasPrefix(ctype, "image/"), strings.HasPrefix(ctype, "video/"), strings.HasPrefix(ctype, "audio/"):
		return true
	}
	return false
}

func (f *Files) handleDownload(w http.ResponseWriter, r *http.Request) {
	p, err := rel(r.URL.Query().Get("path"))
	if err != nil || p == "." {
		f.fail(w, errBadPath)
		return
	}
	file, err := f.root.Open(p)
	if err != nil {
		f.fail(w, err)
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		f.fail(w, err)
		return
	}
	if st.IsDir() {
		httpx.Error(w, http.StatusBadRequest, "cannot download a folder")
		return
	}
	name := path.Base(p)
	ctype := mime.TypeByExtension(path.Ext(name))
	inline := r.URL.Query().Get("inline") == "1"
	switch {
	case inline && previewable(ctype):
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", "inline")
	case inline && r.URL.Query().Get("text") == "1":
		// Text previews are fetched by the app and shown with textContent, never rendered as HTML.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, name, st.ModTime(), file)
}

func (f *Files) handleUpload(w http.ResponseWriter, r *http.Request) {
	dir, err := rel(r.URL.Query().Get("path"))
	if err != nil {
		f.fail(w, err)
		return
	}
	if st, err := f.root.Stat(dir); err != nil || !st.IsDir() {
		f.fail(w, cmpErr(err, fs.ErrNotExist))
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	var saved []string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "upload interrupted")
			return
		}
		name := part.FileName()
		if name == "" {
			part.Close()
			continue
		}
		if !validName(name) {
			part.Close()
			httpx.Error(w, http.StatusBadRequest, "invalid file name")
			return
		}
		final, err := f.save(dir, name, part)
		part.Close()
		if err != nil {
			f.fail(w, err)
			return
		}
		saved = append(saved, final)
	}
	f.log.Info("upload", "dir", dir, "files", len(saved))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"saved": saved})
}

// save streams src into a temporary file, then renames it to a free name in dir.
func (f *Files) save(dir, name string, src io.Reader) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := path.Join(dir, ".ultraspace-upload-"+hex.EncodeToString(rnd[:]))
	out, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = f.root.Remove(tmp)
		return "", err
	}
	final := f.freeName(dir, name)
	if err := f.root.Rename(tmp, path.Join(dir, final)); err != nil {
		_ = f.root.Remove(tmp)
		return "", err
	}
	return final, nil
}

// freeName returns name, or "name (n).ext" if name is taken.
func (f *Files) freeName(dir, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for i := 1; ; i++ {
		if _, err := f.root.Lstat(path.Join(dir, candidate)); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
}

func (f *Files) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	p, err := rel(req.Path)
	if err != nil || p == "." || !validName(path.Base(p)) {
		f.fail(w, errBadPath)
		return
	}
	if err := f.root.Mkdir(p, 0o750); err != nil {
		f.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (f *Files) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	from, err1 := rel(req.From)
	to, err2 := rel(req.To)
	if err1 != nil || err2 != nil || from == "." || to == "." || !validName(path.Base(to)) {
		f.fail(w, errBadPath)
		return
	}
	if to == from || strings.HasPrefix(to, from+"/") {
		httpx.Error(w, http.StatusBadRequest, "cannot move a folder into itself")
		return
	}
	if _, err := f.root.Lstat(to); err == nil {
		f.fail(w, fs.ErrExist)
		return
	}
	if err := f.root.Rename(from, to); err != nil {
		f.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (f *Files) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	for _, raw := range req.Paths {
		p, err := rel(raw)
		if err != nil || p == "." {
			f.fail(w, errBadPath)
			return
		}
		if _, err := f.root.Lstat(p); err != nil {
			f.fail(w, err)
			return
		}
		if err := f.root.RemoveAll(p); err != nil {
			f.fail(w, err)
			return
		}
	}
	f.log.Info("delete", "count", len(req.Paths))
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (f *Files) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadPath):
		httpx.Error(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, fs.ErrNotExist):
		httpx.Error(w, http.StatusNotFound, "not found")
	case errors.Is(err, fs.ErrExist):
		httpx.Error(w, http.StatusConflict, "a file or folder with that name already exists")
	case errors.Is(err, fs.ErrPermission):
		httpx.Error(w, http.StatusForbidden, "permission denied")
	case strings.Contains(err.Error(), "escapes from parent"), strings.Contains(err.Error(), "path escapes"):
		httpx.Error(w, http.StatusBadRequest, "path is outside the files folder")
	case strings.Contains(err.Error(), "not a directory"):
		httpx.Error(w, http.StatusBadRequest, "not a folder")
	case strings.Contains(err.Error(), "directory not empty"):
		httpx.Error(w, http.StatusConflict, "folder is not empty")
	default:
		f.log.Error("files", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
