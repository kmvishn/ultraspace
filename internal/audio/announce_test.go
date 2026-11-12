package audio

import (
	"bytes"
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func announceAudio(t *testing.T) (*Audio, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	a := NewWithRunner(slog.Default(), &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"get-default-sink": func(args []string) ([]byte, error) {
			return []byte("alsa_output.pci-0000_00_1f.3.analog-stereo\n"), nil
		},
		"list sinks": func(args []string) ([]byte, error) {
			return []byte(samplePactlOutput), nil
		},
		"-s 160": func(args []string) ([]byte, error) {
			mu.Lock()
			calls = append(calls, "espeak "+strings.Join(args, " "))
			mu.Unlock()
			return nil, nil
		},
		"--audio-device": func(args []string) ([]byte, error) {
			mu.Lock()
			calls = append(calls, "mpv "+strings.Join(args, " "))
			mu.Unlock()
			return nil, nil
		},
	}})
	a.findSpeech = func() (string, error) { return "espeak-ng", nil }
	return a, &calls
}

func TestAnnounceTextSpeaksOnChosenSpeaker(t *testing.T) {
	a, calls := announceAudio(t)
	err := a.AnnounceText(context.Background(), "  dinner is ready  ", "", []string{"alsa_output.pci-0000_00_1f.3.analog-stereo"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "espeak") || !strings.Contains(joined, "dinner is ready") {
		t.Fatalf("speech = %s", joined)
	}
	if !strings.Contains(joined, "--audio-device=pulse/alsa_output.pci-0000_00_1f.3.analog-stereo") {
		t.Fatalf("playback = %s", joined)
	}
}

func TestAnnounceRejectsUnknownSpeaker(t *testing.T) {
	a, calls := announceAudio(t)
	err := a.AnnounceText(context.Background(), "hello", "", []string{"no-such-speaker"}, 0)
	if err == nil || !strings.Contains(err.Error(), "connected") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("played anyway: %v", *calls)
	}
}

func TestAnnounceBusy(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	a := NewWithRunner(slog.Default(), &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"get-default-sink": func(args []string) ([]byte, error) {
			return []byte("alsa_output.pci-0000_00_1f.3.analog-stereo\n"), nil
		},
		"list sinks": func(args []string) ([]byte, error) { return []byte(samplePactlOutput), nil },
		"-s 160":     func(args []string) ([]byte, error) { return nil, nil },
		"--audio-device": func(args []string) ([]byte, error) {
			once.Do(func() { close(started) })
			<-release
			return nil, nil
		},
	}})
	a.findSpeech = func() (string, error) { return "espeak-ng", nil }
	sink := []string{"alsa_output.pci-0000_00_1f.3.analog-stereo"}
	done := make(chan error, 1)
	go func() { done <- a.AnnounceText(context.Background(), "first", "", sink, 0) }()
	<-started
	err := a.AnnounceText(context.Background(), "second", "", sink, 0)
	if err == nil || !strings.Contains(err.Error(), "already playing") {
		t.Fatalf("err = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAnnounceVoice(t *testing.T) {
	a, calls := announceAudio(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="audio"; filename="note.webm"`},
		"Content-Type":        {"audio/webm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("webm")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("sink", "alsa_output.pci-0000_00_1f.3.analog-stereo"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/audio/announce/voice", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleAnnounceVoice(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "mpv ") {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestAnnounceCapabilities(t *testing.T) {
	a, _ := announceAudio(t)
	rec := httptest.NewRecorder()
	a.handleAnnounceCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/audio/announce/capabilities", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"available":true`) || !strings.Contains(rec.Body.String(), `"speakers":`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestParseVoicesKeepsNamedVariants(t *testing.T) {
	got := parseVoices([]byte("Pty Language Age/Gender VoiceName File\n" +
		" 5  en --/M English gmw/en\n" +
		" 5  variant --/M male2 !v/m2\n" +
		" 5  variant --/M fast_test !v/fast\n" +
		" 5  variant --/M Gene2 !v/Gene2\n" +
		" 5  variant --/F Alicia !v/Alicia\n" +
		" 5  variant --/M Adam !v/adam\n" +
		" 5  variant --/F grandma !v/grandma\n" +
		" 5  variant --/M Half-LifeAnnouncementSystem !v/announcer\n" +
		" 5  variant --/M Mr_Serious !v/Mr serious\n" +
		" 5  variant --/M Storm !v/Storm             (en-us 5)\n"))
	if len(got) != 6 || got[0].Name != "Alicia" || got[1].Name != "Grandma" || got[2].Name != "Adam" || got[3].Name != "Announcer" || got[4].Name != "Mr Serious" || got[5].Name != "Storm" {
		t.Fatalf("%+v", got)
	}
	if got[0].ID != "en+Alicia" || got[0].Gender != "female" || got[2].ID != "en+adam" || got[3].ID != "en+announcer" || got[4].ID != "en+Mr serious" || got[5].ID != "en+Storm" {
		t.Fatalf("%+v", got)
	}
}

func TestParseMicsSkipsSpeakerMonitors(t *testing.T) {
	got := parseMics([]byte(`Source #1
	Name: alsa_output.pci.monitor
	Description: Monitor of Built-in Audio
Source #2
	Name: alsa_input.pci-analog
	Description: Built-in Microphone
`))
	if len(got) != 1 || got[0].Name != "alsa_input.pci-analog" || got[0].Description != "Built-in Microphone" {
		t.Fatalf("%+v", got)
	}
}

func TestSpeakingRate(t *testing.T) {
	if got, err := speakingRate(0); err != nil || got != 160 {
		t.Fatalf("default = %d %v", got, err)
	}
	if got, err := speakingRate(80); err != nil || got != 80 {
		t.Fatalf("slow = %d %v", got, err)
	}
	if _, err := speakingRate(50); err == nil {
		t.Fatal("accepted a speed outside the range")
	}
}

func TestAnnounceUsesChosenVoice(t *testing.T) {
	a, calls := announceAudio(t)
	a.runner = &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"get-default-sink": func(args []string) ([]byte, error) {
			return []byte("alsa_output.pci-0000_00_1f.3.analog-stereo\n"), nil
		},
		"list sinks": func(args []string) ([]byte, error) { return []byte(samplePactlOutput), nil },
		"--voices": func(args []string) ([]byte, error) {
			return []byte("Pty Language Age/Gender VoiceName File\n 5  variant --/M Adam !v/adam\n"), nil
		},
		"-s 120": func(args []string) ([]byte, error) {
			*calls = append(*calls, "espeak "+strings.Join(args, " "))
			return nil, nil
		},
		"--audio-device": func(args []string) ([]byte, error) { return nil, nil },
	}}
	err := a.AnnounceText(context.Background(), "hello", "en+adam", []string{"alsa_output.pci-0000_00_1f.3.analog-stereo"}, 120)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, " ")
	if !strings.Contains(joined, "-v en+adam") || !strings.Contains(joined, "-s 120") {
		t.Fatalf("calls = %v", *calls)
	}
	if err := a.AnnounceText(context.Background(), "hello", "nope", []string{"alsa_output.pci-0000_00_1f.3.analog-stereo"}, 0); err == nil {
		t.Fatal("unknown voice was accepted")
	}
}
