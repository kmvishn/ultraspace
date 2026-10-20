package remote

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const transferPage = `<!doctype html><body style="margin:0;height:100vh">
<input type="file" id="f">
<script>
document.body.addEventListener('dragover', (e) => e.preventDefault());
document.body.addEventListener('drop', (e) => { e.preventDefault(); window.dropped = [...e.dataTransfer.files].map((f) => f.name + ':' + f.size).join(); });
document.addEventListener('paste', (e) => { e.preventDefault(); window.pasted = [...e.clipboardData.files].map((f) => f.name + ':' + f.size).join(); });
</script>`

// Runs against a real Chrome: ULTRASPACE_TEST_CHROME=/path/to/chrome-headless-shell go test ./internal/remote/
func TestFilesIntoPages(t *testing.T) {
	chrome := os.Getenv("ULTRASPACE_TEST_CHROME")
	if chrome == "" {
		t.Skip("set ULTRASPACE_TEST_CHROME to run")
	}
	m := New(Options{ChromePath: chrome, DataDir: t.TempDir(), FilesRoot: t.TempDir(), NoSandbox: true, Profile: "normal"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	in, err := m.get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	c := &client{text: make(chan []byte, 256), frame: make(chan []byte, 1), audio: make(chan []byte, 8), closed: make(chan struct{})}
	in.addClient(c)

	id, _, err := m.put(1, "hello.txt", strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if other := m.resolve(2, []string{id}); len(other) != 0 {
		t.Fatal("another user's id resolved")
	}
	files := m.resolve(1, []string{id})

	wait(t, in, "a page session", func() bool { return in.activeSession() != "" })
	sess := in.activeSession()
	in.conn.Send(sess, "Page.navigate", map[string]string{"url": "data:text/html," + url.PathEscape(transferPage)})
	time.Sleep(1500 * time.Millisecond)
	eval := func(js string) string {
		var res struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		in.conn.Call(ctx, sess, "Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "userGesture": true}, &res)
		b, _ := json.Marshal(res.Result.Value)
		return string(b)
	}

	// 1. A file dialog opened by the page reaches the window, which answers it.
	eval(`document.getElementById('f').click()`)
	var chooser struct {
		Session string `json:"session"`
		Node    int    `json:"node"`
	}
	deadline := time.After(10 * time.Second)
	for chooser.Node == 0 {
		select {
		case b := <-c.text:
			var msg map[string]any
			json.Unmarshal(b, &msg)
			if msg["t"] == "filechooser" {
				json.Unmarshal(b, &chooser)
			}
		case <-deadline:
			t.Fatal("no filechooser message")
		}
	}
	in.fillChooser(chooser.Session, chooser.Node, files)
	if got := eval(`[...document.getElementById('f').files].map((f) => f.name + ':' + f.size).join()`); got != `"hello.txt:11"` {
		t.Fatalf("file input holds %s", got)
	}

	// 2. Drag and drop onto the page.
	in.dropFiles(200, 200, files)
	wait(t, in, "the drop", func() bool { return eval(`window.dropped || ''`) == `"hello.txt:11"` })

	// 3. Pasting a file.
	taken, err := in.pasteFiles(files)
	if err != nil || !taken {
		t.Fatalf("paste taken=%v err=%v", taken, err)
	}
	if got := eval(`window.pasted`); got != `"hello.txt:11"` {
		t.Fatalf("pasted = %s", got)
	}
}
