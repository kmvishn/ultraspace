package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestShellSwitchAndBackup(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	if code, out := c.do("GET", "/api/account/shell", nil, nil); code != 200 || out["enabled"] != true {
		t.Fatalf("shell = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/account/shell", map[string]any{"enabled": false, "current": "wrong-password"}, nil); code != 403 {
		t.Fatalf("shell with wrong password = %d", code)
	}
	if code, out := c.do("POST", "/api/account/shell", map[string]any{"enabled": false, "current": "correct-horse-1"}, nil); code != 200 || out["enabled"] != false {
		t.Fatalf("turn shell off = %d %v", code, out)
	}
	if code, out := c.do("GET", "/api/agent/status", nil, nil); code != 200 || out["shell_enabled"] != false {
		t.Fatalf("agent status = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/term/sessions", map[string]any{"cols": 80, "rows": 24}, nil); code != 403 {
		t.Fatalf("new terminal while off = %d", code)
	}
	if code, _ := c.do("POST", "/api/account/shell", map[string]any{"enabled": true, "current": "correct-horse-1"}, nil); code != 200 {
		t.Fatalf("turn shell on = %d", code)
	}

	// Backup: wrong password refused, right password returns a zip.
	backup := func(pw string) (int, []byte) {
		b, _ := json.Marshal(map[string]string{"current": pw})
		req, _ := http.NewRequest("POST", c.base+"/api/account/backup", bytes.NewReader(b))
		req.Header.Set("X-Ultraspace", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, data
	}
	if code, _ := backup("wrong-password"); code != 403 {
		t.Fatalf("backup with wrong password = %d", code)
	}
	code, data := backup("correct-horse-1")
	if code != 200 || len(data) < 4 || string(data[:2]) != "PK" || !strings.Contains(string(data), "ultraspace.db") {
		t.Fatalf("backup = %d, %d bytes", code, len(data))
	}
}

func TestNewDeviceIsFlagged(t *testing.T) {
	c, cfg := setup(t)
	c.do("POST", "/api/setup", map[string]string{"token": token(t, cfg), "username": "admin", "password": "correct-horse-1"}, nil)

	jar, _ := cookiejar.New(nil)
	other := &client{t: t, base: c.base, http: &http.Client{Jar: jar}}
	code, _ := other.do("POST", "/api/login",
		map[string]string{"username": "admin", "password": "correct-horse-1"},
		map[string]string{"User-Agent": "Mozilla/5.0 (Linux; Android 14) TestPhone"})
	if code != 200 {
		t.Fatalf("second login = %d", code)
	}
	_, out := c.do("GET", "/api/account/sessions", nil, nil)
	sessions, _ := out["sessions"].([]any)
	var newID float64
	for _, raw := range sessions {
		s, _ := raw.(map[string]any)
		if s["is_new"] == true {
			newID, _ = s["id"].(float64)
		}
	}
	if newID == 0 {
		t.Fatalf("no session flagged new: %v", out)
	}
	if code, _ := c.do("POST", "/api/account/sessions/seen", map[string]any{"id": newID}, nil); code != 200 {
		t.Fatalf("seen = %d", code)
	}
	_, out = c.do("GET", "/api/account/sessions", nil, nil)
	for _, raw := range out["sessions"].([]any) {
		if raw.(map[string]any)["is_new"] == true {
			t.Fatalf("still flagged new: %v", out)
		}
	}
}
