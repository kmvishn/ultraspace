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
	"time"

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
	Available   bool   `json:"available"`
	DefaultSink string `json:"default_sink"`
	Volume      int    `json:"volume"`
	Mute        bool   `json:"mute"`
	Sinks       []Sink `json:"sinks"`
}

type SetSinkRequest struct {
	Sink string `json:"sink"`
}

type SetVolumeRequest struct {
	Volume int    `json:"volume"` // 0 - 100
	Sink   string `json:"sink"`   // optional; defaults to active sink
}

type SetMuteRequest struct {
	Mute bool   `json:"mute"`
	Sink string `json:"sink"` // optional; defaults to active sink
}

// Audio manages PulseAudio/PipeWire interactions.
type Audio struct {
	log    *slog.Logger
	runner Runner
}

// New creates an Audio service with execRunner.
func New(log *slog.Logger) *Audio {
	return &Audio{log: log, runner: execRunner{}}
}

// NewWithRunner creates an Audio service with custom Runner for testing.
func NewWithRunner(log *slog.Logger, r Runner) *Audio {
	return &Audio{log: log, runner: r}
}

// Register wires HTTP endpoints into the router.
func (a *Audio) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/audio/status", auth(http.HandlerFunc(a.handleStatus)))
	mux.Handle("POST /api/audio/sink", auth(http.HandlerFunc(a.handleSetSink)))
	mux.Handle("POST /api/audio/volume", auth(http.HandlerFunc(a.handleSetVolume)))
	mux.Handle("POST /api/audio/mute", auth(http.HandlerFunc(a.handleSetMute)))
	mux.Handle("POST /api/audio/test", auth(http.HandlerFunc(a.handlePlayTestSound)))
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
	sinks := ParseSinks(sinksOut, defaultSink)

	// If default sink was not set or empty, pick the first sink
	if defaultSink == "" && len(sinks) > 0 {
		defaultSink = sinks[0].Name
		sinks[0].Active = true
	}

	activeVol := 100
	activeMute := false
	for _, s := range sinks {
		if s.Active {
			activeVol = s.Volume
			activeMute = s.Mute
			break
		}
	}

	return &Status{
		Available:   true,
		DefaultSink: defaultSink,
		Volume:      activeVol,
		Mute:        activeMute,
		Sinks:       sinks,
	}, nil
}

// SetDefaultSink changes the system-wide default audio output sink.
func (a *Audio) SetDefaultSink(ctx context.Context, sinkName string) error {
	if sinkName == "" {
		return errors.New("sink name required")
	}
	out, err := a.runner.CombinedOutput(ctx, "pactl", "set-default-sink", sinkName)
	if err != nil {
		return fmt.Errorf("set-default-sink: %w (%s)", err, strings.TrimSpace(string(out)))
	}
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

	if err := a.SetDefaultSink(ctx, req.Sink); err != nil {
		a.log.Error("set default sink", "sink", req.Sink, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "sink": req.Sink})
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

