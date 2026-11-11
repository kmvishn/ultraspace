package files

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchStaysInsideChosenFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skip"), 0o750); err != nil {
		t.Fatal(err)
	}
	payload := id3Text("TIT2", "Night Drive") + id3Text("TPE1", "Ada")
	if err := os.WriteFile(filepath.Join(dir, "keep", "a.mp3"), []byte(id3Header(payload)+payload), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip", "b.mp3"), []byte("not music"), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := New(dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	mux := http.NewServeMux()
	f.Register(mux, func(h http.Handler) http.Handler { return h })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/files/search?kind=audio&under=/keep", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Night Drive") || strings.Contains(w.Body.String(), "b.mp3") {
		t.Fatalf("search = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/files/search?kind=audio&under=/missing", nil))
	if w.Code != 400 {
		t.Fatalf("missing folder = %d", w.Code)
	}
}
