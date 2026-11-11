// Package network manages system network interfaces, Wi-Fi scanning, and connections via NetworkManager (nmcli).
package network

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
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

// Interface represents a physical or virtual network interface.
type Interface struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`       // "wifi", "ethernet", "loopback", etc.
	State      string   `json:"state"`      // "connected", "disconnected", "connecting", "unavailable", etc.
	Connection string   `json:"connection"` // active network connection or SSID
	IPv4       string   `json:"ipv4"`       // e.g. "192.168.1.50/24"
	Gateway    string   `json:"gateway"`    // default gateway
	DNS        []string `json:"dns"`
	MAC        string   `json:"mac"`
}

// WiFiAccessPoint represents a scanned wireless network.
type WiFiAccessPoint struct {
	SSID     string `json:"ssid"`
	BSSID    string `json:"bssid"`
	Signal   int    `json:"signal"` // 0 - 100
	Channel  int    `json:"channel"`
	Freq     string `json:"freq"`     // e.g. "2.4 GHz" or "5 GHz"
	Security string `json:"security"` // e.g. "WPA2", "WPA3", "Open"
	InUse    bool   `json:"in_use"`
}

// Status summarizes overall network connectivity.
type Status struct {
	Connected     bool        `json:"connected"`
	Primary       string      `json:"primary"` // name of primary interface (e.g. "eth0", "wlan0")
	Type          string      `json:"type"`    // "wifi" or "ethernet"
	IP            string      `json:"ip"`
	SSID          string      `json:"ssid"`
	Available     bool        `json:"available"` // whether NetworkManager is installed and available
	HotspotActive bool        `json:"hotspot_active"`
	HotspotSSID   string      `json:"hotspot_ssid"`
	Interfaces    []Interface `json:"interfaces"`
}

// ConnectRequest is the JSON payload for connecting to a Wi-Fi network.
type ConnectRequest struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
	Hidden   bool   `json:"hidden"`
	Device   string `json:"device"` // optional specific wireless interface
}

// DisconnectRequest is the payload for disconnecting an interface.
type DisconnectRequest struct {
	Device string `json:"device"`
}

// Network coordinates network management.
type Network struct {
	log           *slog.Logger
	runner        Runner
	hotspot       *HotspotManager
	desktop       http.Handler // the signed-in app, published on the setup Wi-Fi
	desktopListen string       // where that app already listens, for example 127.0.0.1:4600
}

// New creates a Network service using the system's execRunner.
func New(log *slog.Logger) *Network {
	net := &Network{log: log, runner: execRunner{}}
	net.hotspot = NewHotspotManager(net, DefaultHotspotConfig())
	return net
}

// NewWithRunner creates a Network service with a custom Runner (useful for tests).
func NewWithRunner(log *slog.Logger, r Runner) *Network {
	net := &Network{log: log, runner: r}
	net.hotspot = NewHotspotManager(net, DefaultHotspotConfig())
	return net
}

// EnableHotspot configures and returns the HotspotManager.
func (n *Network) EnableHotspot(cfg HotspotConfig) *HotspotManager {
	n.hotspot = NewHotspotManager(n, cfg)
	return n.hotspot
}

// Hotspot returns the active HotspotManager.
func (n *Network) Hotspot() *HotspotManager {
	return n.hotspot
}

// PublishDesktop remembers the app so the setup page can open it from a phone.
// listen is the address the app already serves, such as 127.0.0.1:4600.
func (n *Network) PublishDesktop(h http.Handler, listen string) {
	if n == nil {
		return
	}
	n.desktop = h
	n.desktopListen = listen
}

// Register wires HTTP endpoints into the router.
func (n *Network) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/network/status", auth(http.HandlerFunc(n.handleStatus)))
	mux.Handle("GET /api/network/wifi/scan", auth(http.HandlerFunc(n.handleWiFiScan)))
	mux.Handle("POST /api/network/wifi/connect", auth(http.HandlerFunc(n.handleWiFiConnect)))
	mux.Handle("POST /api/network/wifi/disconnect", auth(http.HandlerFunc(n.handleWiFiDisconnect)))
	mux.Handle("GET /api/network/hotspot", auth(http.HandlerFunc(n.handleHotspotStatus)))
	mux.Handle("POST /api/network/hotspot/start", auth(http.HandlerFunc(n.handleHotspotStart)))
	mux.Handle("POST /api/network/hotspot/stop", auth(http.HandlerFunc(n.handleHotspotStop)))
}

// SplitNMFields splits a colon-delimited line from `nmcli -t`, respecting backslash-escaped colons.
func SplitNMFields(line string) []string {
	var fields []string
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
		} else if ch == '\\' {
			escaped = true
		} else if ch == ':' {
			fields = append(fields, cur.String())
			cur.Reset()
		} else {
			cur.WriteByte(ch)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// GetStatus queries NetworkManager for all devices and their configuration.
func (n *Network) GetStatus(ctx context.Context) (*Status, error) {
	out, err := n.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "dev", "status")
	if err != nil {
		n.log.Debug("nmcli dev status not available", "err", err)
		return &Status{Available: false, Interfaces: []Interface{}}, nil
	}

	status := &Status{Available: true, Interfaces: []Interface{}}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := SplitNMFields(line)
		if len(fields) < 3 {
			continue
		}
		devName := fields[0]
		devType := fields[1]
		devState := fields[2]
		connName := ""
		if len(fields) >= 4 {
			connName = fields[3]
		}

		if devType == "loopback" {
			continue
		}

		if devType == "802-11-wireless" {
			devType = "wifi"
		} else if devType == "802-3-ethernet" {
			devType = "ethernet"
		}

		iface := Interface{
			Name:       devName,
			Type:       devType,
			State:      devState,
			Connection: connName,
			DNS:        []string{},
		}

		// Query detailed device info
		devOut, devErr := n.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "GENERAL.HWADDR,IP4.ADDRESS,IP4.GATEWAY,IP4.DNS", "dev", "show", devName)
		if devErr == nil {
			devScanner := bufio.NewScanner(bytes.NewReader(devOut))
			for devScanner.Scan() {
				dLine := strings.TrimSpace(devScanner.Text())
				key, val, ok := strings.Cut(dLine, ":")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				val = strings.TrimSpace(val)
				switch {
				case key == "GENERAL.HWADDR":
					iface.MAC = val
				case strings.HasPrefix(key, "IP4.ADDRESS"):
					if iface.IPv4 == "" {
						iface.IPv4 = val
					}
				case key == "IP4.GATEWAY":
					if val != "--" && val != "" {
						iface.Gateway = val
					}
				case strings.HasPrefix(key, "IP4.DNS"):
					if val != "--" && val != "" {
						iface.DNS = append(iface.DNS, val)
					}
				}
			}
		}

		if devState == "connected" && !status.Connected {
			status.Connected = true
			status.Primary = devName
			status.Type = devType
			status.IP = iface.IPv4
			if devType == "wifi" {
				status.SSID = connName
			}
		}

		status.Interfaces = append(status.Interfaces, iface)
	}

	if n.hotspot != nil {
		hs := n.hotspot.Status(ctx)
		status.HotspotActive = hs.Active
		status.HotspotSSID = hs.SSID
	}

	return status, nil
}

// ParseWiFiList parses `nmcli -t -f IN-USE,BSSID,SSID,CHAN,FREQ,SIGNAL,SECURITY dev wifi list` output.
func ParseWiFiList(out []byte) []WiFiAccessPoint {
	bySSID := make(map[string]WiFiAccessPoint)
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := SplitNMFields(line)
		if len(fields) < 7 {
			continue
		}
		inUse := strings.TrimSpace(fields[0]) == "*"
		bssid := fields[1]
		ssid := fields[2]
		chanStr := fields[3]
		freqStr := fields[4]
		sigStr := fields[5]
		secStr := fields[6]

		if ssid == "" || ssid == "--" {
			continue // skip hidden networks without SSIDs in general list
		}

		ch, _ := strconv.Atoi(chanStr)
		sig, _ := strconv.Atoi(sigStr)

		freq := "2.4 GHz"
		if strings.Contains(freqStr, "5") || ch > 14 {
			freq = "5 GHz"
		}

		sec := secStr
		if sec == "" || sec == "--" {
			sec = "Open"
		}

		existing, found := bySSID[ssid]
		if !found {
			bySSID[ssid] = WiFiAccessPoint{
				SSID:     ssid,
				BSSID:    bssid,
				Signal:   sig,
				Channel:  ch,
				Freq:     freq,
				Security: sec,
				InUse:    inUse,
			}
		} else {
			// Prefer connected BSSID, or highest signal strength
			if inUse || (!existing.InUse && sig > existing.Signal) {
				existing.BSSID = bssid
				existing.Signal = sig
				existing.Channel = ch
				existing.Freq = freq
				if inUse {
					existing.InUse = true
				}
				bySSID[ssid] = existing
			}
		}
	}

	aps := make([]WiFiAccessPoint, 0, len(bySSID))
	for _, ap := range bySSID {
		aps = append(aps, ap)
	}

	// Sort: connected first, then highest signal descending, then SSID
	sort.Slice(aps, func(i, j int) bool {
		if aps[i].InUse != aps[j].InUse {
			return aps[i].InUse
		}
		if aps[i].Signal != aps[j].Signal {
			return aps[i].Signal > aps[j].Signal
		}
		return aps[i].SSID < aps[j].SSID
	})

	return aps
}

// ScanWiFi returns list of available Wi-Fi access points.
func (n *Network) ScanWiFi(ctx context.Context, rescan bool) ([]WiFiAccessPoint, error) {
	if rescan {
		// Attempt rescan; ignore error as it may fail if scanned too recently
		_, _ = n.runner.CombinedOutput(ctx, "nmcli", "dev", "wifi", "rescan")
	}

	out, err := n.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "IN-USE,BSSID,SSID,CHAN,FREQ,SIGNAL,SECURITY", "dev", "wifi", "list")
	if err != nil {
		return nil, fmt.Errorf("nmcli wifi list: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return ParseWiFiList(out), nil
}

// ConnectWiFi connects to a Wi-Fi network.
func (n *Network) ConnectWiFi(ctx context.Context, req ConnectRequest) error {
	if err := validateConnect(req); err != nil {
		return err
	}
	if n.hotspot != nil && n.hotspot.active {
		_ = n.hotspot.StopHotspot(ctx)
	}
	return n.connectWiFi(ctx, req)
}

func validateConnect(req ConnectRequest) error {
	ssid := strings.TrimSpace(req.SSID)
	if ssid == "" {
		return errors.New("SSID cannot be empty")
	}
	if len(ssid) > 64 || strings.ContainsAny(ssid, "\r\n\x00") {
		return errors.New("invalid SSID format or length")
	}
	if len(req.Password) > 128 || strings.ContainsAny(req.Password, "\r\n\x00") {
		return errors.New("invalid password format or length")
	}
	if req.Device != "" && (len(req.Device) > 32 || strings.ContainsAny(req.Device, " /\\;\r\n\x00")) {
		return errors.New("invalid device name")
	}
	return nil
}

// connectWiFi runs nmcli without changing the setup hotspot.
func (n *Network) connectWiFi(ctx context.Context, req ConnectRequest) error {
	if err := validateConnect(req); err != nil {
		return err
	}
	ssid := strings.TrimSpace(req.SSID)
	args := []string{"dev", "wifi", "connect", ssid}
	if req.Password != "" {
		args = append(args, "password", req.Password)
	}
	if req.Hidden {
		args = append(args, "hidden", "yes")
	}
	if req.Device != "" {
		args = append(args, "ifname", req.Device)
	}

	out, err := n.runner.CombinedOutput(ctx, "nmcli", args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Secrets were required") || strings.Contains(msg, "802-11-wireless-security") {
			return errors.New("incorrect Wi-Fi password")
		}
		if strings.Contains(msg, "No network with SSID") {
			return fmt.Errorf("network %q not found", req.SSID)
		}
		if msg != "" {
			return fmt.Errorf("connect failed: %s", msg)
		}
		return fmt.Errorf("connect failed: %w", err)
	}

	return nil
}

// Disconnect disconnects an interface.
func (n *Network) Disconnect(ctx context.Context, dev string) error {
	if dev == "" {
		return errors.New("device name required")
	}
	out, err := n.runner.CombinedOutput(ctx, "nmcli", "dev", "disconnect", dev)
	if err != nil {
		return fmt.Errorf("disconnect %s: %w (%s)", dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HTTP Handlers

func (n *Network) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status, err := n.GetStatus(ctx)
	if err != nil {
		n.log.Error("get network status", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to get network status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (n *Network) handleWiFiScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	rescan := r.URL.Query().Get("rescan") == "true"
	aps, err := n.ScanWiFi(ctx, rescan)
	if err != nil {
		n.log.Warn("scan wifi", "err", err)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"networks": []WiFiAccessPoint{}, "error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"networks": aps})
}

func (n *Network) handleWiFiConnect(w http.ResponseWriter, r *http.Request) {
	var req ConnectRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := n.ConnectWiFi(ctx, req); err != nil {
		n.log.Warn("wifi connect failed", "ssid", req.SSID, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "connected",
		"message": fmt.Sprintf("Successfully connected to %s", req.SSID),
	})
}

func (n *Network) handleWiFiDisconnect(w http.ResponseWriter, r *http.Request) {
	var req DisconnectRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := n.Disconnect(ctx, req.Device); err != nil {
		n.log.Warn("disconnect failed", "device", req.Device, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}
