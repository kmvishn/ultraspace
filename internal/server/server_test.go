package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ultraspace/internal/config"
)

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *client) do(method, path string, body any, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("X-Ultraspace", "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func setup(t *testing.T) (*client, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h, cleanup, err := New(ctx, cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); cancel(); cleanup() })
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: srv.URL, http: &http.Client{Jar: jar}}, cfg
}

func token(t *testing.T, cfg config.Config) string {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestSetupLoginFlow(t *testing.T) {
	c, cfg := setup(t)

	if code, _ := c.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatalf("me before setup = %d, want 401", code)
	}
	if _, out := c.do("GET", "/api/setup", nil, nil); out["required"] != true {
		t.Fatalf("setup should be required: %v", out)
	}
	if code, _ := c.do("POST", "/api/setup", map[string]string{"token": "wrong", "username": "admin", "password": "correct-horse-1"}, nil); code != 403 {
		t.Fatalf("bad token = %d, want 403", code)
	}
	if code, _ := c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "short"}, nil); code != 400 {
		t.Fatalf("short password = %d, want 400", code)
	}
	if code, out := c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil); code != 200 {
		t.Fatalf("setup = %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("setup token should be deleted after setup")
	}
	if code, out := c.do("GET", "/api/me", nil, nil); code != 200 || out["username"] != "admin" {
		t.Fatalf("me after setup = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/setup", map[string]string{"token": "x", "username": "evil", "password": "correct-horse-2"}, nil); code != 409 {
		t.Fatalf("second setup = %d, want 409", code)
	}

	c.do("POST", "/api/logout", nil, nil)
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatalf("me after logout = %d, want 401", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "nope-nope-nope"}, nil); code != 401 {
		t.Fatalf("bad login = %d, want 401", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "ADMIN", "password": "correct-horse-1"}, nil); code != 200 {
		t.Fatalf("login = %d", code)
	}

	// Password change revokes old sessions and issues a new one.
	if code, _ := c.do("POST", "/api/account/password", map[string]string{"current": "wrong-password", "new": "another-pass-1"}, nil); code != 403 {
		t.Fatalf("wrong current password = %d, want 403", code)
	}
	if code, _ := c.do("POST", "/api/account/password", map[string]string{"current": "correct-horse-1", "new": "another-pass-1"}, nil); code != 200 {
		t.Fatalf("change password = %d", code)
	}
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 200 {
		t.Fatalf("me after password change = %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)
	for range 10 {
		c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "wrong-password"}, nil)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-1"}, nil); code != 429 {
		t.Fatalf("login after 10 failures = %d, want 429", code)
	}
}

func TestCSRF(t *testing.T) {
	c, _ := setup(t)
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "a", "password": "b"}, map[string]string{"X-Ultraspace": ""}); code != 403 {
		t.Fatalf("missing header = %d, want 403", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "a", "password": "b"}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("foreign origin = %d, want 403", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"username": "a", "password": "b"}, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Fatalf("cross-site = %d, want 403", code)
	}
}

func TestFiles(t *testing.T) {
	c, cfg := setup(t)
	if code, _ := c.do("GET", "/api/files/list?path=/", nil, nil); code != 401 {
		t.Fatalf("list unauthenticated = %d, want 401", code)
	}
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	if code, _ := c.do("POST", "/api/files/mkdir", map[string]string{"path": "/docs"}, nil); code != 200 {
		t.Fatalf("mkdir = %d", code)
	}
	if code, _ := c.do("POST", "/api/files/mkdir", map[string]string{"path": "/docs"}, nil); code != 409 {
		t.Fatalf("mkdir existing = %d, want 409", code)
	}

	upload := func(dir, name, content string) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte(content))
		mw.Close()
		req, _ := http.NewRequest("POST", c.base+"/api/files/upload?path="+dir, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Ultraspace", "1")
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code, out := upload("/docs", "hello.txt", "hi there"); code != 200 {
		t.Fatalf("upload = %d %v", code, out)
	}
	// Same name again gets a free name instead of overwriting.
	if _, out := upload("/docs", "hello.txt", "second"); out["saved"].([]any)[0] != "hello (1).txt" {
		t.Fatalf("duplicate upload saved as %v", out["saved"])
	}
	got, _ := os.ReadFile(filepath.Join(cfg.FilesRoot, "docs", "hello.txt"))
	if string(got) != "hi there" {
		t.Fatalf("file content = %q", got)
	}

	if code, out := c.do("GET", "/api/files/list?path=/docs", nil, nil); code != 200 || len(out["entries"].([]any)) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}

	res, err := c.http.Get(c.base + "/api/files/download?path=/docs/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "hi there" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %q, disposition %q", body, res.Header.Get("Content-Disposition"))
	}

	if code, _ := c.do("POST", "/api/files/rename", map[string]string{"from": "/docs/hello.txt", "to": "/docs/hello (1).txt"}, nil); code != 409 {
		t.Fatalf("rename onto existing = %d, want 409", code)
	}
	if code, _ := c.do("POST", "/api/files/rename", map[string]string{"from": "/docs", "to": "/docs/inner"}, nil); code != 400 {
		t.Fatalf("rename into itself = %d, want 400", code)
	}
	if code, _ := c.do("POST", "/api/files/rename", map[string]string{"from": "/docs/hello.txt", "to": "/docs/renamed.txt"}, nil); code != 200 {
		t.Fatalf("rename = %d", code)
	}
	if code, _ := c.do("POST", "/api/files/delete", map[string]any{"paths": []string{"/docs"}}, nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if _, err := os.Stat(filepath.Join(cfg.FilesRoot, "docs")); !os.IsNotExist(err) {
		t.Fatal("docs should be deleted")
	}
	if code, _ := c.do("POST", "/api/files/delete", map[string]any{"paths": []string{"/"}}, nil); code != 400 {
		t.Fatalf("delete root = %d, want 400", code)
	}
}

func TestFilesCannotEscapeRoot(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	secret := filepath.Join(cfg.DataDir, "secret.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)
	// A symlink inside the root that points outside it.
	os.Symlink(cfg.DataDir, filepath.Join(cfg.FilesRoot, "escape"))

	for _, p := range []string{"/../secret.txt", "../secret.txt", "/escape/secret.txt", "/escape/ultraspace.db"} {
		res, err := c.http.Get(c.base + "/api/files/download?path=" + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == 200 || strings.Contains(string(body), "top secret") {
			t.Fatalf("download %q escaped the root: %d %q", p, res.StatusCode, body)
		}
	}
	if code, _ := c.do("POST", "/api/files/delete", map[string]any{"paths": []string{"/escape/secret.txt"}}, nil); code == 200 {
		t.Fatal("delete through symlink escaped the root")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("secret file was touched")
	}
	if code, _ := c.do("POST", "/api/files/mkdir", map[string]string{"path": "/escape/newdir"}, nil); code == 200 {
		t.Fatal("mkdir through symlink escaped the root")
	}
}

func TestSystemAndStatic(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)
	code, out := c.do("GET", "/api/system", nil, nil)
	if code != 200 || out["memory"].(map[string]any)["total"].(float64) <= 0 {
		t.Fatalf("system = %d %v", code, out)
	}
	res, err := c.http.Get(c.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "js/main.js") || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("index = %d csp=%q", res.StatusCode, res.Header.Get("Content-Security-Policy"))
	}
}

func TestWriteSearchSettings(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	if code, out := c.do("POST", "/api/files/mkdir", map[string]string{"path": "/Music"}, nil); code != 200 {
		t.Fatalf("mkdir = %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/files/write", map[string]string{"path": "/Music/notes.txt", "content": "first"}, nil); code != 200 {
		t.Fatalf("write new = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/files/write", map[string]string{"path": "/Music/notes.txt", "content": "second"}, nil); code != 200 {
		t.Fatalf("overwrite = %d", code)
	}
	got, _ := os.ReadFile(filepath.Join(cfg.FilesRoot, "Music", "notes.txt"))
	if string(got) != "second" {
		t.Fatalf("content = %q", got)
	}
	if code, _ := c.do("POST", "/api/files/write", map[string]string{"path": "/Music", "content": "x"}, nil); code != 400 {
		t.Fatalf("write onto folder = %d, want 400", code)
	}
	if code, _ := c.do("POST", "/api/files/write", map[string]string{"path": "/missing/dir/a.txt", "content": "x"}, nil); code != 404 {
		t.Fatalf("write into missing folder = %d, want 404", code)
	}
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Music", "01 - Song.mp3"), []byte("id3"), 0o600)
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Music", "cover.jpg"), []byte("jpg"), 0o600)

	if _, out := c.do("GET", "/api/files/search?kind=audio", nil, nil); len(out["results"].([]any)) != 1 {
		t.Fatalf("audio search = %v", out)
	}
	if _, out := c.do("GET", "/api/files/search?q=NOTES", nil, nil); len(out["results"].([]any)) != 1 {
		t.Fatalf("name search = %v", out)
	}
	if code, _ := c.do("GET", "/api/files/search?kind=bogus", nil, nil); code != 400 {
		t.Fatalf("bad kind = %d, want 400", code)
	}
	// Temporary files from in-flight saves never show up.
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Music", ".ultraspace-write-abc"), []byte("x"), 0o600)
	if _, out := c.do("GET", "/api/files/list?path=/Music", nil, nil); len(out["entries"].([]any)) != 3 {
		t.Fatalf("list should hide temp files: %v", out["entries"])
	}

	if _, out := c.do("GET", "/api/account/settings", nil, nil); len(out) != 0 {
		t.Fatalf("default settings = %v", out)
	}
	if code, _ := c.do("PUT", "/api/account/settings", map[string]any{"theme": "light", "accent": "#3b82f6"}, nil); code != 200 {
		t.Fatalf("put settings = %d", code)
	}
	if _, out := c.do("GET", "/api/account/settings", nil, nil); out["theme"] != "light" {
		t.Fatalf("settings after put = %v", out)
	}
	if code, _ := c.do("PUT", "/api/account/settings", map[string]any{"big": strings.Repeat("x", 20000)}, nil); code != 413 {
		t.Fatalf("oversized settings = %d, want 413", code)
	}
}

func TestSystemEndpoints(t *testing.T) {
	c, cfg := setup(t)
	if code, _ := c.do("GET", "/api/system/processes", nil, nil); code != 401 {
		t.Fatalf("processes unauthenticated = %d, want 401", code)
	}
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)
	if code, out := c.do("GET", "/api/system/history", nil, nil); code != 200 || out["points"] == nil {
		t.Fatalf("history = %d %v", code, out)
	}
	code, out := c.do("GET", "/api/system/processes", nil, nil)
	if code != 200 {
		t.Fatalf("processes = %d", code)
	}
	self := false
	for _, p := range out["processes"].([]any) {
		if int(p.(map[string]any)["pid"].(float64)) == os.Getpid() {
			self = true
		}
	}
	if !self {
		t.Fatal("process list does not include the test process")
	}
	if code, _ := c.do("GET", "/api/logs?unit=../../etc/passwd", nil, nil); code != 400 {
		t.Fatalf("bad unit = %d, want 400", code)
	}
	if code, _ := c.do("GET", "/api/logs?priority=9", nil, nil); code != 400 {
		t.Fatalf("bad priority = %d, want 400", code)
	}
}

func TestWorkspaceState(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	if code, out := c.do("GET", "/api/state/workspace", nil, nil); code != 200 || out["data"] != nil {
		t.Fatalf("empty state = %d %v", code, out)
	}
	doc := map[string]any{"device": "laptop-1", "device_name": "Chrome on Linux", "data": map[string]any{"windows": []any{map[string]any{"app": "files"}}}}
	code, out := c.do("PUT", "/api/state/workspace", doc, nil)
	if code != 200 || out["updated"] == nil {
		t.Fatalf("put state = %d %v", code, out)
	}
	_, out = c.do("GET", "/api/state/workspace", nil, nil)
	if out["device"] != "laptop-1" || out["device_name"] != "Chrome on Linux" {
		t.Fatalf("state device = %v", out)
	}
	if wins := out["data"].(map[string]any)["windows"].([]any); len(wins) != 1 {
		t.Fatalf("state windows = %v", wins)
	}
	for name, body := range map[string]any{
		"not an object": map[string]any{"device": "d", "data": []int{1}},
		"no device":     map[string]any{"data": map[string]any{}},
	} {
		if code, _ := c.do("PUT", "/api/state/workspace", body, nil); code != 400 {
			t.Errorf("%s: code = %d, want 400", name, code)
		}
	}
	// A device that has not seen the latest save must not overwrite it.
	_, out = c.do("GET", "/api/state/workspace", nil, nil)
	seen := out["updated"].(float64)
	stale := map[string]any{"device": "phone-2", "base": seen - 1, "data": map[string]any{}}
	if code, out := c.do("PUT", "/api/state/workspace", stale, nil); code != 409 || out["device_name"] != "Chrome on Linux" {
		t.Errorf("stale write = %d %v, want 409", code, out)
	}
	fresh := map[string]any{"device": "phone-2", "base": seen, "data": map[string]any{}}
	if code, _ := c.do("PUT", "/api/state/workspace", fresh, nil); code != 200 {
		t.Errorf("up-to-date write = %d, want 200", code)
	}
	if code, _ := c.do("PUT", "/api/state/workspace", stale, nil); code != 200 {
		t.Errorf("same device overwriting its own save = %d, want 200", code)
	}
	if code, _ := c.do("PUT", "/api/state/secrets", doc, nil); code != 404 {
		t.Errorf("unknown key = %d, want 404", code)
	}
	big := map[string]any{"device": "d", "data": map[string]any{"x": strings.Repeat("a", 600<<10)}}
	if code, _ := c.do("PUT", "/api/state/workspace", big, nil); code != 413 {
		t.Errorf("oversized state = %d, want 413", code)
	}
}

func TestSessionsAndRevoke(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	// A second device signs in.
	jar, _ := cookiejar.New(nil)
	phone := &client{t: t, base: c.base, http: &http.Client{Jar: jar}}
	if code, _ := phone.do("POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-1"}, map[string]string{"User-Agent": "Phone/1.0"}); code != 200 {
		t.Fatalf("phone login = %d", code)
	}
	_, out := c.do("GET", "/api/account/sessions", nil, nil)
	list := out["sessions"].([]any)
	if len(list) != 2 {
		t.Fatalf("sessions = %v", list)
	}
	var phoneID float64
	for _, s := range list {
		m := s.(map[string]any)
		if m["user_agent"] == "Phone/1.0" {
			phoneID = m["id"].(float64)
			if m["current"] != false {
				t.Errorf("phone session marked current")
			}
		}
	}
	if phoneID == 0 {
		t.Fatalf("phone session not listed: %v", list)
	}
	if code, _ := c.do("POST", "/api/account/sessions/revoke", map[string]any{"id": phoneID}, nil); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ := phone.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatalf("revoked phone still signed in: %d", code)
	}
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 200 {
		t.Fatalf("current device signed out by revoke: %d", code)
	}
	if code, _ := c.do("POST", "/api/account/sessions/revoke", map[string]any{"id": phoneID}, nil); code != 404 {
		t.Fatalf("revoke twice = %d, want 404", code)
	}

	phone.do("POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-1"}, nil)
	if code, _ := c.do("POST", "/api/account/sessions/revoke", map[string]any{"others": true}, nil); code != 200 {
		t.Fatalf("revoke others = %d", code)
	}
	if code, _ := phone.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatalf("phone survived sign-out-others: %d", code)
	}
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 200 {
		t.Fatalf("this device signed out by sign-out-others: %d", code)
	}
}

func TestBrowserProbe(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	for _, u := range []string{"", "javascript:alert(1)", "file:///etc/passwd", "ftp://example.com/"} {
		if code, _ := c.do("GET", "/api/browser/probe?url="+url.QueryEscape(u), nil, nil); code != 400 {
			t.Errorf("probe %q = %d, want 400", u, code)
		}
	}
	// The test server itself is on loopback, which the prober must refuse to contact.
	_, out := c.do("GET", "/api/browser/probe?url="+url.QueryEscape(c.base+"/api/health"), nil, nil)
	if out["error"] != "private address; not checked" {
		t.Fatalf("loopback probe = %v", out)
	}
}

func TestStaticCSPAllowsFrames(t *testing.T) {
	c, _ := setup(t)
	res, err := c.http.Get(c.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-src 'self' https:") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp = %q", csp)
	}
}

func TestNavidromeRelay(t *testing.T) {
	var lastQuery url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lastQuery = q
		sum := md5.Sum([]byte("music-pass" + q.Get("s")))
		if q.Get("u") != "alice" || q.Get("t") != hex.EncodeToString(sum[:]) {
			io.WriteString(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":"Wrong username or password"}}}`)
			return
		}
		if r.URL.Path == "/rest/stream" {
			http.ServeContent(w, r, "a.mp3", time.Time{}, strings.NewReader("0123456789"))
			return
		}
		io.WriteString(w, `{"subsonic-response":{"status":"ok","type":"navidrome","serverVersion":"test"}}`)
	}))
	defer fake.Close()

	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	if code, _ := c.do("POST", "/api/navidrome/connect", map[string]string{"url": fake.URL, "username": "alice", "password": "wrong"}, nil); code != 502 {
		t.Fatalf("bad password connect = %d, want 502", code)
	}
	if code, out := c.do("POST", "/api/navidrome/connect", map[string]string{"url": fake.URL + "/app/", "username": "alice", "password": "music-pass"}, nil); code != 200 {
		t.Fatalf("connect = %d %v", code, out)
	}
	_, out := c.do("GET", "/api/navidrome", nil, nil)
	if out["connected"] != true || out["url"] != fake.URL || out["token"] != nil || out["salt"] != nil {
		t.Fatalf("status leaks or wrong: %v", out)
	}
	if code, _ := c.do("GET", "/api/state/navidrome", nil, nil); code != 404 {
		t.Fatalf("credentials readable through the state API: %d", code)
	}
	// The browser cannot swap in other credentials.
	if code, out := c.do("GET", "/api/navidrome/rest/ping?u=bob&t=x&s=y&id=7", nil, nil); code != 200 || lastQuery.Get("u") != "alice" || lastQuery.Get("id") != "7" {
		t.Fatalf("relay = %d %v, upstream query %v", code, out, lastQuery)
	}
	if code, _ := c.do("GET", "/api/navidrome/rest/createUser?username=x", nil, nil); code != 403 {
		t.Fatalf("admin call allowed: %d", code)
	}
	req, _ := http.NewRequest("GET", c.base+"/api/navidrome/rest/stream?id=1", nil)
	req.Header.Set("Range", "bytes=2-4")
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || string(body) != "234" {
		t.Fatalf("ranged stream = %d %q", res.StatusCode, body)
	}
	c.do("POST", "/api/navidrome/disconnect", nil, nil)
	if _, out := c.do("GET", "/api/navidrome", nil, nil); out["connected"] != false {
		t.Fatalf("still connected after disconnect: %v", out)
	}
}

func TestStaticCachingAndGzip(t *testing.T) {
	c, _ := setup(t)
	req, _ := http.NewRequest("GET", c.base+"/js/core.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req) // no transparent decompression
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := io.ReadAll(res.Body)
	res.Body.Close()
	etag := res.Header.Get("ETag")
	if res.Header.Get("Content-Encoding") != "gzip" || etag == "" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("headers = %v", res.Header)
	}
	plain, _ := http.Get(c.base + "/js/core.js")
	body, _ := io.ReadAll(plain.Body)
	plain.Body.Close()
	if len(gz) >= len(body)/2 {
		t.Errorf("gzip %d bytes vs plain %d: not compressed", len(gz), len(body))
	}
	req.Header.Set("If-None-Match", etag)
	res, _ = http.DefaultTransport.RoundTrip(req)
	res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", res.StatusCode)
	}
	if res, _ := http.Get(c.base + "/vendor/xterm/xterm.mjs"); res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("mjs type = %q", res.Header.Get("Content-Type"))
	}
	if res, _ := http.Post(c.base+"/js/core.js", "text/plain", nil); res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a static file = %d", res.StatusCode)
	}
}

func TestCoverSearch(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)
	for _, f := range []string{"A/photo1.jpg", "A/cover.jpg", "A/zz.png", "B/front.png", "B/a.jpg", "C/only.webp"} {
		p := filepath.Join(cfg.FilesRoot, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	_, out := c.do("GET", "/api/files/search?kind=cover", nil, nil)
	got := map[string]bool{}
	for _, r := range out["results"].([]any) {
		got[r.(map[string]any)["path"].(string)] = true
	}
	want := map[string]bool{"/A/cover.jpg": true, "/B/front.png": true, "/C/only.webp": true}
	if len(got) != len(want) {
		t.Fatalf("covers = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("covers = %v, want %v", got, want)
		}
	}
}

func TestClipboardAndEvents(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	// Another device listens for live events.
	req, _ := http.NewRequest("GET", c.base+"/api/events", nil)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("events content type = %q", ct)
	}
	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := res.Body.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "event: clipboard\n") {
				got <- acc
				return
			}
			if err != nil {
				return
			}
		}
	}()

	if code, out := c.do("POST", "/api/clipboard", map[string]string{"text": "hello from the phone", "device": "Phone", "origin": "dev-1"}, nil); code != 200 || out["text"] != "hello from the phone" {
		t.Fatalf("add = %d %v", code, out)
	}
	select {
	case ev := <-got:
		if !strings.Contains(ev, "hello from the phone") || !strings.Contains(ev, `"origin":"dev-1"`) {
			t.Fatalf("event = %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no clipboard event")
	}
	c.do("POST", "/api/clipboard", map[string]string{"text": "second", "device": "Laptop"}, nil)
	c.do("POST", "/api/clipboard", map[string]string{"text": "hello from the phone", "device": "Laptop"}, nil) // re-sent: moves to the top, no duplicate
	_, out := c.do("GET", "/api/clipboard", nil, nil)
	items := out["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["text"] != "hello from the phone" {
		t.Fatalf("history = %v", items)
	}
	id := items[1].(map[string]any)["id"].(string)
	c.do("DELETE", "/api/clipboard/"+id, nil, nil)
	if _, out := c.do("GET", "/api/clipboard", nil, nil); len(out["items"].([]any)) != 1 {
		t.Fatalf("after delete = %v", out)
	}
	if code, _ := c.do("GET", "/api/state/clipboard", nil, nil); code != 404 {
		t.Fatalf("clipboard readable through the state API: %d", code)
	}
}

// exifJPEG makes a w×h JPEG carrying EXIF orientation and a date taken.
func exifJPEG(t *testing.T, w, h, orientation int, taken string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var body bytes.Buffer
	if err := jpeg.Encode(&body, img, nil); err != nil {
		t.Fatal(err)
	}
	// TIFF (little-endian): IFD0 with Orientation and an Exif IFD pointer; Exif IFD with DateTimeOriginal.
	le := binary.LittleEndian
	tiff := make([]byte, 0, 128)
	tiff = append(tiff, 'I', 'I', 42, 0, 8, 0, 0, 0)
	entry := func(tag, typ uint16, count, val uint32) {
		tiff = le.AppendUint16(tiff, tag)
		tiff = le.AppendUint16(tiff, typ)
		tiff = le.AppendUint32(tiff, count)
		tiff = le.AppendUint32(tiff, val)
	}
	tiff = le.AppendUint16(tiff, 2)
	entry(0x0112, 3, 1, uint32(orientation))
	exifOff := uint32(8 + 2 + 2*12 + 4)
	entry(0x8769, 4, 1, exifOff)
	tiff = le.AppendUint32(tiff, 0)
	tiff = le.AppendUint16(tiff, 1)
	dateOff := exifOff + 2 + 12 + 4
	entry(0x9003, 2, uint32(len(taken)+1), dateOff)
	tiff = le.AppendUint32(tiff, 0)
	tiff = append(tiff, append([]byte(taken), 0)...)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}
	b := body.Bytes()
	return append(append(append([]byte{}, b[:2]...), append(seg, app1...)...), b[2:]...)
}

func TestPhotosAndThumbnails(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)
	os.MkdirAll(filepath.Join(cfg.FilesRoot, "Photos", ".hidden"), 0o755)
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Photos", "phone.jpg"), exifJPEG(t, 800, 600, 6, "2021:07:04 18:30:00"), 0o644)
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Photos", "plain.jpg"), exifJPEG(t, 300, 200, 1, "2019:01:02 03:04:05"), 0o644)
	os.WriteFile(filepath.Join(cfg.FilesRoot, "Photos", ".hidden", "skip.jpg"), exifJPEG(t, 10, 10, 1, "2020:01:01 00:00:00"), 0o644)

	_, out := c.do("GET", "/api/files/photos", nil, nil)
	ph := out["photos"].([]any)
	if len(ph) != 2 {
		t.Fatalf("photos = %v", ph)
	}
	first := ph[0].(map[string]any)
	if first["path"] != "/Photos/phone.jpg" || !strings.HasPrefix(first["taken"].(string), "2021-07-04T18:30:00") {
		t.Fatalf("newest photo = %v", first)
	}
	if first["w"] != float64(600) || first["h"] != float64(800) {
		t.Fatalf("rotated photo should report 600x800, got %v x %v", first["w"], first["h"])
	}

	res, err := c.http.Get(c.base + "/api/files/thumb?path=/Photos/phone.jpg&size=256")
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := jpeg.Decode(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("thumb: %v (status %d)", err, res.StatusCode)
	}
	if b := thumb.Bounds(); b.Dx() != 192 || b.Dy() != 256 {
		t.Fatalf("thumb size = %v, want upright 192x256", b)
	}
	etag := res.Header.Get("ETag")
	req, _ := http.NewRequest("GET", c.base+"/api/files/thumb?path=/Photos/phone.jpg&size=256", nil)
	req.Header.Set("If-None-Match", etag)
	if res, _ := c.http.Do(req); res.StatusCode != http.StatusNotModified {
		t.Fatalf("cached thumb = %d, want 304", res.StatusCode)
	}
	if code, _ := c.do("GET", "/api/files/thumb?path=/notes.txt", nil, nil); code != 404 && code != 415 {
		t.Fatalf("thumb of a non-image = %d", code)
	}
}
