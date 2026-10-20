package remote

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// Runs against a real Chrome: ULTRASPACE_TEST_CHROME=/path/to/chrome-headless-shell go test ./internal/remote/
func TestTabsSleepAndWake(t *testing.T) {
	chrome := os.Getenv("ULTRASPACE_TEST_CHROME")
	if chrome == "" {
		t.Skip("set ULTRASPACE_TEST_CHROME to run")
	}
	m := New(Options{ChromePath: chrome, DataDir: t.TempDir(), FilesRoot: t.TempDir(), NoSandbox: true, Profile: "tiny"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	in, err := m.get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()

	in.newTab("https://example.com/")
	in.newTab("https://example.org/")
	wait(t, in, "first page to sleep", func() bool {
		s := tabs(in)
		return len(s) == 3 && s[1].Asleep && s[1].URL == "https://example.com/" && !s[2].Asleep
	})

	// Opening the sleeping tab reloads it in place and puts the other one to sleep.
	sleeping := tabs(in)[1].ID
	in.activate(sleeping)
	wait(t, in, "sleeping tab to wake in place", func() bool {
		s := tabs(in)
		return len(s) == 3 && !s[1].Asleep && s[1].ID != sleeping && s[1].URL == "https://example.com/" && s[2].Asleep && in.active == s[1].ID
	})

	// Closing a sleeping tab just removes it.
	in.closeTab(tabs(in)[2].ID)
	wait(t, in, "sleeping tab to close", func() bool { return len(tabs(in)) == 2 })

	// After a restart only the active tab loads; the rest come back asleep.
	in.newTab("https://example.net/")
	time.Sleep(2 * time.Second)
	in.Stop()
	in, err = m.get(1)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, in, "tabs restored", func() bool {
		s := tabs(in)
		live := 0
		for _, x := range s {
			if !x.Asleep {
				live++
			}
		}
		// Blank tabs are not saved, so the two web pages come back.
		return len(s) == 2 && live == 1 && s[0].URL == "https://example.com/" && s[0].Asleep && s[1].URL == "https://example.net/" && !s[1].Asleep
	})
}

func tabs(in *instance) []Tab {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Tab
	for _, id := range in.order {
		if t := in.tabs[id]; t != nil {
			out = append(out, *t)
		}
	}
	return out
}

func wait(t *testing.T, in *instance, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; tabs: %+v", what, tabs(in))
}
