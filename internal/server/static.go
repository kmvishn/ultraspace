package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// staticFile is one embedded asset, prepared once at startup.
type staticFile struct {
	body  []byte
	gz    []byte // nil when compressing does not help
	etag  string
	ctype string
}

// staticFiles serves the embedded frontend with ETags (repeat visits download nothing
// that has not changed) and precompressed gzip (about a quarter of the bytes for JS/CSS).
type staticFiles map[string]*staticFile

func loadStatic(fsys fs.FS) (staticFiles, error) {
	files := staticFiles{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		f := &staticFile{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: contentType(p)}
		if compressible(f.ctype) && len(body) > 512 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(body)
			zw.Close()
			if buf.Len() < len(body)*9/10 {
				f.gz = buf.Bytes()
			}
		}
		files["/"+p] = f
		return nil
	})
	return files, err
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "svg") || strings.Contains(ctype, "json")
}

func (s staticFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if strings.HasSuffix(p, "/") {
		p += "index.html"
	}
	f := s[p]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Vary", "Accept-Encoding")
	if strings.HasPrefix(p, "/vendor/") {
		h.Set("Cache-Control", "public, max-age=86400") // third-party files change only with a release
	} else {
		h.Set("Cache-Control", "no-cache") // always revalidate; the ETag makes that a 304
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.body
	if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
