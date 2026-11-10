package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type mockRunner struct {
	handlers map[string]func(args []string) ([]byte, error)
}

func (m *mockRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmdStr := strings.Join(args, " ")
	for pattern, handler := range m.handlers {
		if strings.Contains(cmdStr, pattern) {
			return handler(args)
		}
	}
	return nil, errors.New("command not mocked: " + cmdStr)
}

const samplePactlOutput = `Sink #0
	State: RUNNING
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
	Driver: module-alsa-card.c
	Sample Specification: s16le 2ch 44100Hz
	Channel Map: front-left,front-right
	Owner Module: 22
	Mute: no
	Volume: front-left: 42598 /  65% / -11.23 dB,   front-right: 42598 /  65% / -11.23 dB
	        balance 0.00
	Base Volume: 65536 / 100% / 0.00 dB
	Monitor Source: alsa_output.pci-0000_00_1f.3.analog-stereo.monitor

Sink #1
	State: SUSPENDED
	Name: alsa_output.pci-0000_00_03.0.hdmi-stereo
	Description: HDMI / DisplayPort Audio
	Driver: module-alsa-card.c
	Mute: yes
	Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB

Sink #2
	State: IDLE
	Name: bluez_output.00_11_22_33_44_55.1
	Description: Sony WH-1000XM4
	Driver: module-bluez5-device.c
	Mute: no
	Volume: front-left: 32768 /  50% / -18.06 dB
`

func TestParseSinks(t *testing.T) {
	defaultSink := "alsa_output.pci-0000_00_1f.3.analog-stereo"
	sinks := ParseSinks([]byte(samplePactlOutput), defaultSink)

	if len(sinks) != 3 {
		t.Fatalf("ParseSinks len = %d, want 3", len(sinks))
	}

	// 1. Analog Stereo
	if sinks[0].Name != defaultSink {
		t.Errorf("sink[0] Name = %q, want %q", sinks[0].Name, defaultSink)
	}
	if !sinks[0].Active {
		t.Error("expected sink[0] to be Active")
	}
	if sinks[0].Volume != 65 {
		t.Errorf("sink[0] Volume = %d, want 65", sinks[0].Volume)
	}
	if sinks[0].Mute {
		t.Error("sink[0] Mute = true, want false")
	}
	if sinks[0].Type != "analog" {
		t.Errorf("sink[0] Type = %q, want analog", sinks[0].Type)
	}

	// 2. HDMI
	if sinks[1].Type != "hdmi" {
		t.Errorf("sink[1] Type = %q, want hdmi", sinks[1].Type)
	}
	if !sinks[1].Mute {
		t.Error("sink[1] Mute = false, want true")
	}
	if sinks[1].Active {
		t.Error("sink[1] Active = true, want false")
	}

	// 3. Bluetooth
	if sinks[2].Type != "bluetooth" {
		t.Errorf("sink[2] Type = %q, want bluetooth", sinks[2].Type)
	}
	if sinks[2].Volume != 50 {
		t.Errorf("sink[2] Volume = %d, want 50", sinks[2].Volume)
	}
}

func TestGetStatus(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"get-default-sink": func(args []string) ([]byte, error) {
				return []byte("alsa_output.pci-0000_00_1f.3.analog-stereo\n"), nil
			},
			"list sinks": func(args []string) ([]byte, error) {
				return []byte(samplePactlOutput), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)
	st, err := audioSvc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.Available {
		t.Fatal("expected Available to be true")
	}
	if st.DefaultSink != "alsa_output.pci-0000_00_1f.3.analog-stereo" {
		t.Errorf("DefaultSink = %q", st.DefaultSink)
	}
	if st.Volume != 65 {
		t.Errorf("Volume = %d, want 65", st.Volume)
	}
	if st.Mute {
		t.Error("Mute = true, want false")
	}
	if len(st.Sinks) != 3 {
		t.Errorf("Sinks len = %d, want 3", len(st.Sinks))
	}
}

func TestSetSinkAndVolume(t *testing.T) {
	var capturedSink string
	var capturedVol string
	var capturedMute string
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"set-default-sink": func(args []string) ([]byte, error) {
				mu.Lock()
				capturedSink = args[len(args)-1]
				mu.Unlock()
				return []byte(""), nil
			},
			"set-sink-volume": func(args []string) ([]byte, error) {
				mu.Lock()
				capturedVol = args[len(args)-1]
				mu.Unlock()
				return []byte(""), nil
			},
			"set-sink-mute": func(args []string) ([]byte, error) {
				mu.Lock()
				capturedMute = args[len(args)-1]
				mu.Unlock()
				return []byte(""), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)

	// Set Default Sink
	if err := audioSvc.SetDefaultSink(context.Background(), "my_sink"); err != nil {
		t.Fatalf("SetDefaultSink err: %v", err)
	}
	mu.Lock()
	if capturedSink != "my_sink" {
		t.Errorf("capturedSink = %q, want my_sink", capturedSink)
	}
	mu.Unlock()

	// Set Volume
	if err := audioSvc.SetVolume(context.Background(), "", 85); err != nil {
		t.Fatalf("SetVolume err: %v", err)
	}
	mu.Lock()
	if capturedVol != "85%" {
		t.Errorf("capturedVol = %q, want 85%%", capturedVol)
	}
	mu.Unlock()

	// Set Mute
	if err := audioSvc.SetMute(context.Background(), "", true); err != nil {
		t.Fatalf("SetMute err: %v", err)
	}
	mu.Lock()
	if capturedMute != "1" {
		t.Errorf("capturedMute = %q, want 1", capturedMute)
	}
	mu.Unlock()

	// Reject invalid sink names
	if err := audioSvc.SetDefaultSink(context.Background(), "bad\nsink"); err == nil {
		t.Errorf("expected error for sink name with newline")
	}
	if err := audioSvc.SetVolume(context.Background(), "bad\nsink", 50); err == nil {
		t.Errorf("expected error for sink name with newline in SetVolume")
	}
}

func TestPlayTestSound(t *testing.T) {
	var paplayCalled bool
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"complete.oga": func(args []string) ([]byte, error) {
				mu.Lock()
				paplayCalled = true
				mu.Unlock()
				return []byte(""), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)
	if err := audioSvc.PlayTestSound(context.Background()); err != nil {
		t.Fatalf("PlayTestSound err: %v", err)
	}

	mu.Lock()
	called := paplayCalled
	mu.Unlock()

	if !called {
		t.Error("expected paplay to be called")
	}
}

func TestAudioHTTPHandlers(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"get-default-sink": func(args []string) ([]byte, error) {
				return []byte("alsa_output.pci-0000_00_1f.3.analog-stereo\n"), nil
			},
			"list sinks": func(args []string) ([]byte, error) {
				return []byte(samplePactlOutput), nil
			},
			"set-default-sink": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
			"set-sink-volume": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
			"set-sink-mute": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
			"complete.oga": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)
	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	audioSvc.Register(mux, passAuth)

	// 1. GET /api/audio/status
	req := httptest.NewRequest("GET", "/api/audio/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/audio/status code = %d", w.Code)
	}
	var st Status
	if err := json.NewDecoder(w.Body).Decode(&st); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !st.Available || len(st.Sinks) != 3 {
		t.Errorf("unexpected status: %+v", st)
	}

	// 2. POST /api/audio/sink
	body, _ := json.Marshal(SetSinkRequest{Sink: "bluez_output.00_11_22_33_44_55.1"})
	req = httptest.NewRequest("POST", "/api/audio/sink", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("POST /api/audio/sink code = %d, body = %s", w.Code, string(b))
	}

	// 3. POST /api/audio/volume
	body, _ = json.Marshal(SetVolumeRequest{Volume: 80})
	req = httptest.NewRequest("POST", "/api/audio/volume", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/audio/volume code = %d", w.Code)
	}

	// 4. POST /api/audio/mute
	body, _ = json.Marshal(SetMuteRequest{Mute: true})
	req = httptest.NewRequest("POST", "/api/audio/mute", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/audio/mute code = %d", w.Code)
	}

	// 5. POST /api/audio/test
	req = httptest.NewRequest("POST", "/api/audio/test", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/audio/test code = %d", w.Code)
	}

	// 6. GET /api/audio/player/status
	req = httptest.NewRequest("GET", "/api/audio/player/status", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/audio/player/status code = %d", w.Code)
	}

	// 7. POST /api/audio/player/stop
	req = httptest.NewRequest("POST", "/api/audio/player/stop", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/audio/player/stop code = %d", w.Code)
	}
}

func TestSinkInputMigration(t *testing.T) {
	var movedInputID, movedSink string
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"set-default-sink": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
			"sink-inputs short": func(args []string) ([]byte, error) {
				return []byte("15\t1\t242\tprotocol-native.c\ts16le 1ch 48000Hz\n"), nil
			},
			"move-sink-input": func(args []string) ([]byte, error) {
				mu.Lock()
				if len(args) >= 3 {
					movedInputID = args[len(args)-2]
					movedSink = args[len(args)-1]
				}
				mu.Unlock()
				return []byte(""), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)
	if err := audioSvc.SetDefaultSink(context.Background(), "bluez_sink.test"); err != nil {
		t.Fatalf("SetDefaultSink err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if movedInputID != "15" {
		t.Errorf("movedInputID = %q, want 15", movedInputID)
	}
	if movedSink != "bluez_sink.test" {
		t.Errorf("movedSink = %q, want bluez_sink.test", movedSink)
	}
}

func TestSetMultipleSinksCombined(t *testing.T) {
	var loadedModule string
	var defaultSink string
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"unload-module": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
			"load-module module-combine-sink": func(args []string) ([]byte, error) {
				mu.Lock()
				loadedModule = strings.Join(args, " ")
				mu.Unlock()
				return []byte("42\n"), nil
			},
			"set-default-sink": func(args []string) ([]byte, error) {
				mu.Lock()
				if len(args) > 0 {
					defaultSink = args[len(args)-1]
				}
				mu.Unlock()
				return []byte(""), nil
			},
			"sink-inputs short": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	audioSvc := NewWithRunner(slog.Default(), runner)
	sinks := []string{"bluez_sink.1", "alsa_output.speaker"}
	if err := audioSvc.SetSinks(context.Background(), sinks); err != nil {
		t.Fatalf("SetSinks err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(loadedModule, "slaves=bluez_sink.1,alsa_output.speaker") {
		t.Errorf("loadedModule = %q, want slaves=bluez_sink.1,alsa_output.speaker", loadedModule)
	}
	if defaultSink != "combined" {
		t.Errorf("defaultSink = %q, want combined", defaultSink)
	}
}

