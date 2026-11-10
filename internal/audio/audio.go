// Package audio manages server audio output devices (PulseAudio / PipeWire sinks), volume, and routing.
package audio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
)

// Runner abstracts command execution for testability.
type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Sink represents an audio output device (soundcard, HDMI, Bluetooth, etc.).
type Sink struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Volume      int    `json:"volume"` // 0 - 100%
	Mute        bool   `json:"mute"`
	Active      bool   `json:"active"` // true if this is the default sink
	Type        string `json:"type"`   // "analog", "hdmi", "bluetooth", "usb", "virtual", "other"
}

// Status represents the overall audio state on the server.
type Status struct {
	Available    bool     `json:"available"`
	DefaultSink  string   `json:"default_sink"`
	Volume       int      `json:"volume"`
	Mute         bool     `json:"mute"`
	Sinks        []Sink   `json:"sinks"`
	ActiveSinks  []string `json:"active_sinks"`
	CombinedSink bool     `json:"combined_sink"`
}

type SetSinkRequest struct {
	Sink string `json:"sink"`
}

type SetSinksRequest struct {
	Sinks []string `json:"sinks"`
}

type SetVolumeRequest struct {
	Volume int    `json:"volume"` // 0 - 100
	Sink   string `json:"sink"`   // optional; defaults to active sink
}

type SetMuteRequest struct {
	Mute bool   `json:"mute"`
	Sink string `json:"sink"` // optional; defaults to active sink
}

// Audio manages PulseAudio/PipeWire interactions and server playback.
type Audio struct {
	log         *slog.Logger
	runner      Runner
	player      *Player
	mu          sync.RWMutex
	activeSinks []string
}

// New creates an Audio service with execRunner and server Player.
func New(log *slog.Logger, filesRoot ...string) *Audio {
	root := ""
	if len(filesRoot) > 0 {
		root = filesRoot[0]
	}
	return &Audio{
		log:    log,
		runner: execRunner{},
		player: NewPlayer(log, root),
	}
}

// NewWithRunner creates an Audio service with custom Runner for testing.
func NewWithRunner(log *slog.Logger, r Runner, filesRoot ...string) *Audio {
	root := ""
	if len(filesRoot) > 0 {
		root = filesRoot[0]
	}
	return &Audio{
		log:    log,
		runner: r,
		player: NewPlayer(log, root),
	}
}

// SetStreamResolver assigns a resolver for provider stream URLs (e.g. Navidrome).
func (a *Audio) SetStreamResolver(fn StreamResolver) {
	if a.player != nil {
		a.player.SetStreamResolver(fn)
	}
}

// Close releases server player resources.
func (a *Audio) Close() {
	if a.player != nil {
		a.player.Close()
	}
}

// Register wires HTTP endpoints into the router.
func (a *Audio) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/audio/status", auth(http.HandlerFunc(a.handleStatus)))
	mux.Handle("POST /api/audio/sink", auth(http.HandlerFunc(a.handleSetSink)))
	mux.Handle("POST /api/audio/sinks", auth(http.HandlerFunc(a.handleSetSinks)))
	mux.Handle("POST /api/audio/volume", auth(http.HandlerFunc(a.handleSetVolume)))
	mux.Handle("POST /api/audio/mute", auth(http.HandlerFunc(a.handleSetMute)))
	mux.Handle("POST /api/audio/test", auth(http.HandlerFunc(a.handlePlayTestSound)))

	// Server-side audio player endpoints
	mux.Handle("POST /api/audio/player/play", auth(http.HandlerFunc(a.handlePlayerPlay)))
	mux.Handle("POST /api/audio/player/pause", auth(http.HandlerFunc(a.handlePlayerPause)))
	mux.Handle("POST /api/audio/player/resume", auth(http.HandlerFunc(a.handlePlayerResume)))
	mux.Handle("POST /api/audio/player/seek", auth(http.HandlerFunc(a.handlePlayerSeek)))
	mux.Handle("POST /api/audio/player/stop", auth(http.HandlerFunc(a.handlePlayerStop)))
	mux.Handle("GET /api/audio/player/status", auth(http.HandlerFunc(a.handlePlayerStatus)))
}

var volRegexp = regexp.MustCompile(`/ +([0-9]+)%`)

func detectSinkType(name, driver, desc string) string {
	lower := strings.ToLower(name + " " + driver + " " + desc)
	switch {
	case strings.Contains(lower, "bluez"):
		return "bluetooth"
	case strings.Contains(lower, "hdmi"):
		return "hdmi"
	case strings.Contains(lower, "usb"):
		return "usb"
	case strings.Contains(lower, "null") || strings.Contains(lower, "dummy") || strings.Contains(lower, "auto_null"):
		return "virtual"
	case strings.Contains(lower, "analog") || strings.Contains(lower, "speaker") || strings.Contains(lower, "headphone") || strings.Contains(lower, "pci"):
		return "analog"
	default:
		return "other"
	}
}

// ParseSinks parses the detailed output of `pactl list sinks`.
func ParseSinks(out []byte, defaultSink string) []Sink {
	var sinks []Sink
	scanner := bufio.NewScanner(bytes.NewReader(out))

	var current *Sink
	driver := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "Sink #") {
			if current != nil {
				current.Type = detectSinkType(current.Name, driver, current.Description)
				current.Active = (current.Name == defaultSink)
				sinks = append(sinks, *current)
			}
			idStr := strings.TrimPrefix(line, "Sink #")
			id, _ := strconv.Atoi(idStr)
			current = &Sink{ID: id}
			driver = ""
			continue
		}

		if current == nil {
			continue
		}

		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "Name":
			current.Name = v
		case "Description":
			current.Description = v
		case "Driver":
			driver = v
		case "Mute":
			current.Mute = (strings.ToLower(v) == "yes")
		case "Volume":
			matches := volRegexp.FindStringSubmatch(v)
			if len(matches) >= 2 {
				vol, _ := strconv.Atoi(matches[1])
				current.Volume = vol
			}
		}
	}

	if current != nil {
		current.Type = detectSinkType(current.Name, driver, current.Description)
		current.Active = (current.Name == defaultSink)
		sinks = append(sinks, *current)
	}

	return sinks
}

// GetStatus queries the system for active PulseAudio / PipeWire audio output devices and volume.
func (a *Audio) GetStatus(ctx context.Context) (*Status, error) {
	// 1. Get default sink
	defOut, err := a.runner.CombinedOutput(ctx, "pactl", "get-default-sink")
	if err != nil {
		a.log.Debug("pactl get-default-sink failed", "err", err)
		return &Status{Available: false, Sinks: []Sink{}}, nil
	}
	defaultSink := strings.TrimSpace(string(defOut))

	// 2. Get all sinks
	sinksOut, err := a.runner.CombinedOutput(ctx, "pactl", "list", "sinks")
	if err != nil {
		return &Status{Available: false, Sinks: []Sink{}}, nil
	}
	allSinks := ParseSinks(sinksOut, defaultSink)

	a.mu.Lock()
	if len(a.activeSinks) == 0 && defaultSink != "" && defaultSink != "combined" {
		a.activeSinks = []string{defaultSink}
	}
	activeMap := make(map[string]bool)
	for _, s := range a.activeSinks {
		activeMap[s] = true
	}
	a.mu.Unlock()

	var sinks []Sink
	activeVol := 100
	activeMute := false

	for _, s := range allSinks {
		// Hide virtual and combine sinks from the device list,
		// so users see their physical/bluetooth hardware devices.
		if strings.HasPrefix(s.Name, "combined") || s.Type == "virtual" {
			if s.Name == defaultSink || (defaultSink == "combined" && s.Name == "combined") {
				activeVol = s.Volume
				activeMute = s.Mute
			}
			continue
		}
		if activeMap[s.Name] || (len(activeMap) == 0 && s.Active) {
			s.Active = true
			if !strings.HasPrefix(defaultSink, "combined") {
				activeVol = s.Volume
				activeMute = s.Mute
			}
		} else {
			s.Active = false
		}
		sinks = append(sinks, s)
	}

	a.mu.RLock()
	currentActive := append([]string{}, a.activeSinks...)
	a.mu.RUnlock()

	return &Status{
		Available:    true,
		DefaultSink:  defaultSink,
		Volume:       activeVol,
		Mute:         activeMute,
		Sinks:        sinks,
		ActiveSinks:  currentActive,
		CombinedSink: defaultSink == "combined",
	}, nil
}

// unloadCombineSinks unloads all existing module-combine-sink instances by numeric module ID.
// PulseAudio requires integer IDs for module unloading (unloading by name fails).
func (a *Audio) unloadCombineSinks(ctx context.Context) {
	out, err := a.runner.CombinedOutput(ctx, "pactl", "list", "modules", "short")
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "module-combine-sink") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				modID := fields[0]
				if _, err := strconv.Atoi(modID); err == nil {
					_, _ = a.runner.CombinedOutput(ctx, "pactl", "unload-module", modID)
				}
			}
		}
	}
}

// moveSinkInputs safely moves active client audio playback streams (e.g. mpv, browser) to the specified sink.
// It explicitly skips internal filter modules, combine-sink slave streams, and non-client streams.
func (a *Audio) moveSinkInputs(ctx context.Context, targetSink string) {
	inputsOut, err := a.runner.CombinedOutput(ctx, "pactl", "list", "sink-inputs", "short")
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(inputsOut))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			// fields[0]=inputID, fields[1]=sinkID, fields[2]=clientID, fields[3]=driver
			// Internal filter/combine-sink slave streams have client ID "-" or module-combine-sink driver.
			// NEVER move them, as doing so breaks combine-sink output slaves!
			if fields[2] == "-" || strings.Contains(line, "module-combine-sink") || strings.Contains(line, "filter") {
				continue
			}
			inputID := fields[0]
			currentSink := fields[1]
			if currentSink == targetSink {
				continue
			}
			if _, err := strconv.Atoi(inputID); err == nil {
				_, moveErr := a.runner.CombinedOutput(ctx, "pactl", "move-sink-input", inputID, targetSink)
				if moveErr != nil {
					a.log.Debug("move-sink-input failed", "id", inputID, "sink", targetSink, "err", moveErr)
				}
			}
		}
	}
}

// SetSinks sets one or multiple simultaneous audio output sinks using PulseAudio's module-combine-sink.
func (a *Audio) SetSinks(ctx context.Context, sinks []string) error {
	var cleanSinks []string
	for _, s := range sinks {
		s = strings.TrimSpace(s)
		if s == "" || len(s) > 128 || strings.ContainsAny(s, "\r\n\x00, ") || strings.HasPrefix(s, "combined") || strings.HasPrefix(s, "null") {
			continue
		}
		cleanSinks = append(cleanSinks, s)
	}

	if len(cleanSinks) == 0 {
		a.mu.Lock()
		a.activeSinks = nil
		a.mu.Unlock()
		if a.player != nil {
			_ = a.player.Stop(ctx)
			_ = a.player.SetAudioDevice("")
		}
		a.unloadCombineSinks(ctx)
		return nil
	}

	a.mu.Lock()
	same := len(cleanSinks) == len(a.activeSinks)
	if same {
		for i := range cleanSinks {
			if cleanSinks[i] != a.activeSinks[i] {
				same = false
				break
			}
		}
	}
	a.mu.Unlock()
	if same {
		if a.player != nil {
			if len(cleanSinks) == 1 {
				_ = a.player.SetAudioDevice(cleanSinks[0])
			} else {
				_ = a.player.SetAudioDevice("combined")
			}
		}
		return nil
	}

	if len(cleanSinks) == 1 {
		// Single sink: First migrate active streams to target BEFORE unloading combine sinks.
		// This prevents PulseAudio from abruptly terminating streams when combined unloads.
		target := cleanSinks[0]
		if err := a.SetDefaultSink(ctx, target); err != nil {
			return err
		}
		if a.player != nil {
			_ = a.player.SetAudioDevice(target)
		}
		a.moveSinkInputs(ctx, target)
		a.unloadCombineSinks(ctx)

		a.mu.Lock()
		a.activeSinks = cleanSinks
		a.mu.Unlock()
		return nil
	}

	// Multiple sinks:
	// Move active streams to the primary sink temporarily so playback does not stall during unload
	a.moveSinkInputs(ctx, cleanSinks[0])
	a.unloadCombineSinks(ctx)

	// Load module-combine-sink with resample_method and adjust_time for drift-free multi-rate sync
	slaves := strings.Join(cleanSinks, ",")
	out, err := a.runner.CombinedOutput(ctx, "pactl", "load-module", "module-combine-sink",
		"sink_name=combined",
		"slaves="+slaves,
		"resample_method=speex-float-1",
		"adjust_time=3",
		"sink_properties=device.description=Combined_Output",
	)
	if err != nil {
		return fmt.Errorf("load combine-sink: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Set combined as default and move all active streams to it
	if err := a.SetDefaultSink(ctx, "combined"); err != nil {
		return err
	}
	if a.player != nil {
		_ = a.player.SetAudioDevice("combined")
	}
	a.moveSinkInputs(ctx, "combined")

	a.mu.Lock()
	a.activeSinks = cleanSinks
	a.mu.Unlock()
	return nil
}

// SetDefaultSink changes the system-wide default audio output sink and immediately moves all active streams.
func (a *Audio) SetDefaultSink(ctx context.Context, sinkName string) error {
	sinkName = strings.TrimSpace(sinkName)
	if sinkName == "" {
		return errors.New("sink name required")
	}
	if len(sinkName) > 128 || strings.ContainsAny(sinkName, "\r\n\x00") {
		return errors.New("invalid sink name")
	}
	out, err := a.runner.CombinedOutput(ctx, "pactl", "set-default-sink", sinkName)
	if err != nil {
		return fmt.Errorf("set-default-sink: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Move all currently active sink inputs to the new sink so ongoing audio switches immediately.
	a.moveSinkInputs(ctx, sinkName)
	return nil
}

// SetVolume sets the volume of a sink (or default sink if empty) as a percentage (0-100%).
func (a *Audio) SetVolume(ctx context.Context, sinkName string, percent int) error {
	if percent < 0 {
		percent = 0
	} else if percent > 150 { // allow up to 150% audio boost
		percent = 150
	}

	target := sinkName
	if target == "" {
		target = "@DEFAULT_SINK@"
	} else if len(target) > 128 || strings.ContainsAny(target, "\r\n\x00") {
		return errors.New("invalid sink name")
	}

	volArg := fmt.Sprintf("%d%%", percent)
	out, err := a.runner.CombinedOutput(ctx, "pactl", "set-sink-volume", target, volArg)
	if err != nil {
		return fmt.Errorf("set-sink-volume: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetMute sets or unsets the mute state for a sink (or default sink).
func (a *Audio) SetMute(ctx context.Context, sinkName string, mute bool) error {
	target := sinkName
	if target == "" {
		target = "@DEFAULT_SINK@"
	} else if len(target) > 128 || strings.ContainsAny(target, "\r\n\x00") {
		return errors.New("invalid sink name")
	}

	muteArg := "0"
	if mute {
		muteArg = "1"
	}

	out, err := a.runner.CombinedOutput(ctx, "pactl", "set-sink-mute", target, muteArg)
	if err != nil {
		return fmt.Errorf("set-sink-mute: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PlayTestSound plays a brief alert sound on the server to verify speaker output.
func (a *Audio) PlayTestSound(ctx context.Context) error {
	// Try standard Linux audio notification bells
	testPaths := []string{
		"/usr/share/sounds/freedesktop/stereo/complete.oga",
		"/usr/share/sounds/freedesktop/stereo/bell.oga",
		"/usr/share/sounds/alsa/Front_Center.wav",
	}

	for _, p := range testPaths {
		out, err := a.runner.CombinedOutput(ctx, "paplay", p)
		if err == nil {
			return nil
		}
		a.log.Debug("paplay attempt", "path", p, "err", err, "out", string(out))
	}

	// Fallback to speaker-test single beep if installed
	out, err := a.runner.CombinedOutput(ctx, "speaker-test", "-t", "sine", "-f", "440", "-l", "1")
	if err != nil {
		return fmt.Errorf("play test sound: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HTTP Handlers

func (a *Audio) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status, err := a.GetStatus(ctx)
	if err != nil {
		a.log.Error("get audio status", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to get audio status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (a *Audio) handleSetSink(w http.ResponseWriter, r *http.Request) {
	var req SetSinkRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.SetSinks(ctx, []string{req.Sink}); err != nil {
		a.log.Error("set default sink", "sink", req.Sink, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "sink": req.Sink})
}

func (a *Audio) handleSetSinks(w http.ResponseWriter, r *http.Request) {
	var req SetSinksRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.SetSinks(ctx, req.Sinks); err != nil {
		a.log.Error("set sinks", "sinks", req.Sinks, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "sinks": req.Sinks})
}

func (a *Audio) handleSetVolume(w http.ResponseWriter, r *http.Request) {
	var req SetVolumeRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.SetVolume(ctx, req.Sink, req.Volume); err != nil {
		a.log.Error("set volume", "vol", req.Volume, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "volume": req.Volume})
}

func (a *Audio) handleSetMute(w http.ResponseWriter, r *http.Request) {
	var req SetMuteRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.SetMute(ctx, req.Sink, req.Mute); err != nil {
		a.log.Error("set mute", "mute", req.Mute, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "mute": req.Mute})
}

func (a *Audio) handlePlayTestSound(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.PlayTestSound(ctx); err != nil {
		a.log.Warn("play test sound failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "playing"})
}

func (a *Audio) handlePlayerPlay(w http.ResponseWriter, r *http.Request) {
	var req PlayRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if len(req.Sinks) > 0 {
		if err := a.SetSinks(ctx, req.Sinks); err != nil {
			a.log.Warn("player set sinks failed", "sinks", req.Sinks, "err", err)
		}
	} else {
		// No server sinks enabled; ensure server player is stopped
		if a.player != nil {
			_ = a.player.Stop(ctx)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
		return
	}

	var userID int64
	if u, ok := auth.UserFrom(r.Context()); ok {
		userID = u.ID
	}

	if err := a.player.Play(ctx, req, userID); err != nil {
		a.log.Error("player play failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	targetSink := "combined"
	if len(req.Sinks) == 1 {
		targetSink = req.Sinks[0]
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		a.moveSinkInputs(context.Background(), targetSink)
	}()

	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "playing"})
}

func (a *Audio) handlePlayerPause(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.player.Pause(ctx); err != nil {
		a.log.Error("player pause failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (a *Audio) handlePlayerResume(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.player.Resume(ctx); err != nil {
		a.log.Error("player resume failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "playing"})
}

func (a *Audio) handlePlayerSeek(w http.ResponseWriter, r *http.Request) {
	var req SeekRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.player.Seek(ctx, req.Time); err != nil {
		a.log.Error("player seek failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": req.Time})
}

func (a *Audio) handlePlayerStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.player.Stop(ctx); err != nil {
		a.log.Error("player stop failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (a *Audio) handlePlayerStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	st, err := a.player.GetStatus(ctx)
	if err != nil {
		a.log.Error("player status failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}


