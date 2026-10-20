package remote

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
)

// Files going from the user's device (or the Files library) into web pages in the
// server browser: file choosers, drag and drop, and pasted files. Each file is
// staged in the browser's own directory and referred to by an opaque id, so the
// page side never names a path on the server.

const (
	stageTTL     = 6 * time.Hour
	maxPasteSize = 10 << 20 // pasted files travel inside a DevTools message
)

type staged struct {
	path    string
	name    string
	userID  int64
	created time.Time
}

type stage struct {
	mu    sync.Mutex
	files map[string]staged
}

func (m *Manager) stageDir(userID int64) string {
	return filepath.Join(m.opts.DataDir, "browser", strconv.FormatInt(userID, 10), "uploads")
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sweep removes staged files older than stageTTL.
func (m *Manager) sweep() {
	m.stage.mu.Lock()
	defer m.stage.mu.Unlock()
	for id, f := range m.stage.files {
		if time.Since(f.created) > stageTTL {
			os.RemoveAll(filepath.Dir(f.path))
			delete(m.stage.files, id)
		}
	}
}

func (m *Manager) put(userID int64, name string, r io.Reader) (string, int64, error) {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == "" || strings.ContainsRune(name, 0) {
		name = "file"
	}
	id := newID()
	dir := filepath.Join(m.stageDir(userID), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return "", 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", 0, err
	}
	m.stage.mu.Lock()
	if m.stage.files == nil {
		m.stage.files = map[string]staged{}
	}
	m.stage.files[id] = staged{path: p, name: name, userID: userID, created: time.Now()}
	m.stage.mu.Unlock()
	return id, n, nil
}

// resolve turns the client's ids into staged paths that belong to this user.
func (m *Manager) resolve(userID int64, ids []string) []staged {
	m.stage.mu.Lock()
	defer m.stage.mu.Unlock()
	var out []staged
	for _, id := range ids {
		if f, ok := m.stage.files[id]; ok && f.userID == userID {
			out = append(out, f)
		}
	}
	return out
}

type stagedJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// handleUpload stages files sent from the user's device (multipart, streamed to disk).
func (m *Manager) handleUpload(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m.sweep()
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	out := []stagedJSON{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "upload interrupted")
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		id, n, err := m.put(u.ID, part.FileName(), part)
		part.Close()
		if err != nil {
			m.log.Error("stage upload", "err", err)
			httpx.Error(w, http.StatusInternalServerError, "could not store the file")
			return
		}
		out = append(out, stagedJSON{ID: id, Name: part.FileName(), Size: n})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"files": out})
}

// handleImport stages copies of files from the Files library. Copies (through
// os.Root) mean a symlink in the library can never point the browser elsewhere.
func (m *Manager) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !httpx.DecodeJSON(w, r, &req) || len(req.Paths) == 0 || len(req.Paths) > 50 {
		httpx.Error(w, http.StatusBadRequest, "give up to 50 file paths")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	m.sweep()
	root, err := os.OpenRoot(m.opts.FilesRoot)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "files are not available")
		return
	}
	defer root.Close()
	out := []stagedJSON{}
	for _, p := range req.Paths {
		rel := strings.TrimPrefix(path.Clean("/"+p), "/")
		f, err := root.Open(rel)
		if err != nil {
			httpx.Error(w, http.StatusNotFound, "not found: "+p)
			return
		}
		if st, err := f.Stat(); err != nil || st.IsDir() {
			f.Close()
			httpx.Error(w, http.StatusBadRequest, "not a file: "+p)
			return
		}
		id, n, err := m.put(u.ID, path.Base(rel), f)
		f.Close()
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not copy the file")
			return
		}
		out = append(out, stagedJSON{ID: id, Name: path.Base(rel), Size: n})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"files": out})
}

// ---------- handing staged files to the page ----------

func paths(files []staged) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// fillChooser answers a page's file dialog (input type=file) with staged files.
func (in *instance) fillChooser(sess string, node int, files []staged) {
	if sess == "" || node == 0 || len(files) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.conn.Call(ctx, sess, "DOM.setFileInputFiles", map[string]any{"files": paths(files), "backendNodeId": node}, nil); err != nil {
		in.log.Debug("set file input", "err", err)
	}
}

// dropFiles performs a real drag-and-drop of staged files at a point in the page.
func (in *instance) dropFiles(x, y float64, files []staged) {
	sess := in.activeSession()
	if sess == "" || len(files) == 0 {
		return
	}
	data := map[string]any{"items": []any{}, "files": paths(files), "dragOperationsMask": 1}
	for _, typ := range []string{"dragEnter", "dragOver", "drop"} {
		in.conn.Send(sess, "Input.dispatchDragEvent", map[string]any{"type": typ, "x": x, "y": y, "data": data})
	}
}

// pasteFiles fires a paste event carrying the files at the focused element, the way
// pasting an image into a chat or mail page works.
func (in *instance) pasteFiles(files []staged) (bool, error) {
	sess := in.activeSession()
	if sess == "" || len(files) == 0 {
		return false, errors.New("no page")
	}
	type item struct {
		Name string `json:"name"`
		Type string `json:"type"`
		B64  string `json:"b64"`
	}
	var list []item
	total := 0
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			return false, err
		}
		if total += len(b); total > maxPasteSize {
			return false, errors.New("pasted files are limited to 10 MB; drag larger files onto the page instead")
		}
		typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.name)))
		list = append(list, item{Name: f.name, Type: typ, B64: base64.StdEncoding.EncodeToString(b)})
	}
	arg, _ := json.Marshal(list)
	js := `((files) => {
		const dt = new DataTransfer();
		for (const f of files) {
			const bin = Uint8Array.from(atob(f.b64), (c) => c.charCodeAt(0));
			dt.items.add(new File([bin], f.name, { type: f.type || 'application/octet-stream' }));
		}
		const a = document.activeElement;
		const target = a && a !== document.body ? a : document;
		const ev = new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true });
		target.dispatchEvent(ev);
		return ev.defaultPrevented;
	})(` + string(arg) + `)`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res struct {
		Result struct {
			Value bool `json:"value"`
		} `json:"result"`
	}
	err := in.conn.Call(ctx, sess, "Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "userGesture": true}, &res)
	return res.Result.Value, err
}
