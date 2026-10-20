package webdl

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":  "_.._etc_passwd",
		"Film: Part 2?.mkv": "Film_ Part 2_.mkv",
		"  ..  ":            "download",
		"a\x00b/c\\d":       "a_b_c_d",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrivateAddressesRefused(t *testing.T) {
	m, err := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer target.Close()
	mux := http.NewServeMux()
	m.Register(mux, func(h http.Handler) http.Handler { return h })
	post := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/downloads", bytes.NewBufferString(body)))
		return rec.Code
	}
	if code := post(`{"url":"file:///etc/passwd","name":"x"}`); code != 400 {
		t.Fatalf("file URL = %d, want 400", code)
	}
	if code := post(`{"url":"` + target.URL + `/a","name":"x"}`); code != 200 {
		t.Fatalf("add = %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/downloads", nil))
		var out struct{ Downloads []job }
		json.Unmarshal(rec.Body.Bytes(), &out)
		if s := out.Downloads[0].State; s == "failed" {
			break
		} else if s == "done" || time.Now().After(deadline) {
			t.Fatalf("loopback download state = %q, want failed", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
