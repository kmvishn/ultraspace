package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Output survives detaching: a second viewer gets the earlier output replayed.
func TestTerminalSessionReplay(t *testing.T) {
	terms := newTerms()
	terms.shell = "/bin/sh"
	mux := http.NewServeMux()
	terms.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer terms.closeAll()

	res, err := http.Post(srv.URL+"/term/sessions", "application/json", strings.NewReader(`{"cols":80,"rows":24}`))
	if err != nil {
		t.Fatal(err)
	}
	var s struct{ ID string }
	json.NewDecoder(res.Body).Decode(&s)
	res.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/term/sessions/" + s.ID + "/ws"
	first, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Write(ctx, websocket.MessageBinary, []byte("echo marker-$((40+2))\n"))
	waitFor(t, ctx, first, "marker-42")
	first.Close(websocket.StatusNormalClosure, "")

	second, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(websocket.StatusNormalClosure, "")
	waitFor(t, ctx, second, "marker-42")
}

func waitFor(t *testing.T, ctx context.Context, c *websocket.Conn, want string) {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q, got %q: %v", want, got.String(), err)
		}
		got.Write(b)
	}
}

func TestSeedSettingsMergesOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	dir := home + "/.local/share/code-server/User"
	os.MkdirAll(dir, 0o700)
	os.WriteFile(dir+"/settings.json", []byte(`{"editor.fontSize": 15, "telemetry.telemetryLevel": "all"}`), 0o600)

	seedSettings(state, true)
	var got map[string]any
	b, _ := os.ReadFile(dir + "/settings.json")
	json.Unmarshal(b, &got)
	if got["editor.fontSize"] != float64(15) || got["telemetry.telemetryLevel"] != "all" || got["chat.disableAIFeatures"] != true {
		t.Fatalf("merged settings = %v", got)
	}
	if _, ok := got["workbench.colorTheme"]; ok {
		t.Fatalf("theme forced onto an existing settings file: %v", got)
	}
	// Once merged, later edits by the user win.
	os.WriteFile(dir+"/settings.json", []byte(`{"chat.disableAIFeatures": false}`), 0o600)
	seedSettings(state, true)
	b, _ = os.ReadFile(dir + "/settings.json")
	if string(b) != `{"chat.disableAIFeatures": false}` {
		t.Fatalf("settings rewritten after the first merge: %s", b)
	}
}
