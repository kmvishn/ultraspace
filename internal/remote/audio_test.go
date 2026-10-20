package remote

import (
	"io"
	"log/slog"
	"os/exec"
	"testing"
	"time"
)

// ffmpeg's tone generator stands in for PulseAudio, to check the fMP4 splitting.
func TestAudioStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &Manager{log: log, ffmpegBin: ffmpeg, opts: Options{AudioInput: []string{"-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000"}}}
	a := &audio{m: m, log: log, enabled: true, listeners: map[*client]bool{}}
	defer a.close()

	first := &client{audio: make(chan []byte, 64), closed: make(chan struct{})}
	a.setListening(first, true)
	init := next(t, first)
	if init[0] != msgAudioInit || string(init[5:9]) != "ftyp" {
		t.Fatalf("first message is not the init segment: % x", init[:12])
	}
	frag := next(t, first)
	if frag[0] != msgAudioFrag || string(frag[5:9]) != "moof" {
		t.Fatalf("second message is not a fragment: % x", frag[:12])
	}

	// A window that switches sound on later gets the init segment first.
	late := &client{audio: make(chan []byte, 64), closed: make(chan struct{})}
	a.setListening(late, true)
	if m := next(t, late); m[0] != msgAudioInit {
		t.Fatalf("late listener started with type %d", m[0])
	}
	if m := next(t, late); m[0] != msgAudioFrag {
		t.Fatalf("late listener then got type %d", m[0])
	}
}

func next(t *testing.T, c *client) []byte {
	t.Helper()
	select {
	case b := <-c.audio:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("no audio message")
		return nil
	}
}
