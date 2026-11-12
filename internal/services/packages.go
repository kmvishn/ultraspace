package services

import (
	"net/http"
	"os"
	"os/exec"
	"strings"

	"ultraspace/internal/httpx"
)

// Package is one program a release expects on the machine.
type Package struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail"`
}

type pkgSpec struct {
	id, name, missing string
	bins              []string
	paths             []string
	homeOnly          bool // a VPS has no speakers, Bluetooth or Wi-Fi to manage
}

// releasePackages is what Settings lists. Paths cover the Browser app's own Chrome
// when it is not on PATH.
var releasePackages = []pkgSpec{
	{id: "mpv", name: "mpv", missing: "Music and video playback on the server speakers.", bins: []string{"mpv"}, homeOnly: true},
	{id: "pulse", name: "PulseAudio", missing: "Server volume and speaker output.", bins: []string{"pactl", "pulseaudio"}, homeOnly: true},
	{id: "networkmanager", name: "NetworkManager", missing: "Wi-Fi, ethernet, and the setup hotspot.", bins: []string{"nmcli"}, homeOnly: true},
	{id: "bluetooth", name: "Bluetooth", missing: "Pairing speakers and other devices.", bins: []string{"bluetoothctl"}, homeOnly: true},
	{id: "chrome", name: "Chrome", missing: "The Browser app.", bins: []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome-headless-shell"}, paths: []string{"/opt/ultraspace/chrome/chrome-headless-shell"}},
	{id: "ffmpeg", name: "ffmpeg", missing: "Video thumbnails and playback.", bins: []string{"ffmpeg"}},
	{id: "espeak", name: "eSpeak", missing: "Spoken announcements. Voice recordings still play without it.", bins: []string{"espeak-ng", "espeak"}, homeOnly: true},
}

// isVPS reports whether this machine is a virtual cloud server.
func isVPS() bool {
	out, err := exec.Command("systemd-detect-virt").Output()
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(out))
	return v != "" && v != "none"
}

func inspectPackages(look func(string) (string, error), exists func(string) bool, vps bool) []Package {
	out := make([]Package, 0, len(releasePackages))
	for _, spec := range releasePackages {
		if vps && spec.homeOnly {
			continue
		}
		p := Package{ID: spec.id, Name: spec.name, Detail: spec.missing}
		for _, bin := range spec.bins {
			if path, err := look(bin); err == nil && path != "" {
				p.Present = true
				p.Path = path
				p.Detail = path
				break
			}
		}
		if !p.Present {
			for _, path := range spec.paths {
				if exists(path) {
					p.Present = true
					p.Path = path
					p.Detail = path
					break
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func (s *Services) handlePackages(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"packages": inspectPackages(exec.LookPath, func(path string) bool {
			st, err := os.Stat(path)
			return err == nil && !st.IsDir()
		}, isVPS()),
	})
}
