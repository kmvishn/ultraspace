package network

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/httpx"
)

// HotspotConfig holds configuration for the fallback setup access point.
type HotspotConfig struct {
	Enabled       bool          `json:"enabled"`
	SSID          string        `json:"ssid"`
	Password      string        `json:"password"`
	GracePeriod   time.Duration `json:"grace_period"`
	CheckInterval time.Duration `json:"check_interval"`
	Device        string        `json:"device"`
}

// HotspotStatus represents the current state of the setup hotspot.
type HotspotStatus struct {
	Active bool   `json:"active"`
	SSID   string `json:"ssid"`
	IP     string `json:"ip"`
	Device string `json:"device"`
}

// DefaultHotspotConfig returns standard fallback hotspot defaults.
func DefaultHotspotConfig() HotspotConfig {
	return HotspotConfig{
		Enabled:       true,
		SSID:          "Ultraspace-Setup",
		Password:      "ultraspace", // WPA2 requires at least 8 characters
		GracePeriod:   45 * time.Second,
		CheckInterval: 10 * time.Second,
		Device:        "", // auto-detect first wifi interface
	}
}

// HotspotManager handles detecting offline state and activating a fallback AP.
type HotspotManager struct {
	mu           sync.Mutex
	net          *Network
	cfg          HotspotConfig
	active       bool
	starting     bool
	activeSSID   string
	activeDev    string
	offlineSince time.Time
}

// NewHotspotManager initializes a HotspotManager for a given Network instance.
func NewHotspotManager(net *Network, cfg HotspotConfig) *HotspotManager {
	if cfg.SSID == "" {
		cfg.SSID = "Ultraspace-Setup"
	}
	if len(cfg.Password) > 0 && len(cfg.Password) < 8 {
		cfg.Password = "ultraspace"
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = 45 * time.Second
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 10 * time.Second
	}
	return &HotspotManager{
		net: net,
		cfg: cfg,
	}
}

// Status returns current hotspot status.
func (h *HotspotManager) Status(ctx context.Context) HotspotStatus {
	out, err := h.net.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "NAME,TYPE,DEVICE", "con", "show", "--active")
	isActive := false
	activeDev := ""

	if err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			fields := SplitNMFields(strings.TrimSpace(scanner.Text()))
			if len(fields) >= 3 {
				name := fields[0]
				devType := fields[1]
				dev := fields[2]
				if (name == "Hotspot" || name == h.cfg.SSID) && (devType == "wifi" || devType == "802-11-wireless") {
					isActive = true
					activeDev = dev
					break
				}
			}
		}
	}

	ip := ""
	if isActive && activeDev != "" {
		devOut, devErr := h.net.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "IP4.ADDRESS", "dev", "show", activeDev)
		if devErr == nil {
			scanner := bufio.NewScanner(bytes.NewReader(devOut))
			for scanner.Scan() {
				k, v, ok := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
				if ok && strings.HasPrefix(strings.TrimSpace(k), "IP4.ADDRESS") {
					ip = strings.TrimSpace(v)
					break
				}
			}
		}
	}

	h.mu.Lock()
	if isActive {
		h.active = true
		h.activeDev = activeDev
	} else if err == nil {
		h.active = false
	}
	res := HotspotStatus{
		Active: h.active,
		SSID:   h.cfg.SSID,
		IP:     ip,
		Device: h.activeDev,
	}
	h.mu.Unlock()

	return res
}

func (h *HotspotManager) findWiFiDevice(ctx context.Context) (string, error) {
	out, err := h.net.runner.CombinedOutput(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE", "dev", "status")
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := SplitNMFields(strings.TrimSpace(scanner.Text()))
		if len(fields) >= 2 {
			dev := fields[0]
			devType := fields[1]
			if devType == "wifi" || devType == "802-11-wireless" {
				return dev, nil
			}
		}
	}
	return "", errors.New("no wireless interface available for hotspot")
}

// StartHotspot activates the Wi-Fi hotspot on the designated or auto-detected wireless interface.
func (h *HotspotManager) StartHotspot(ctx context.Context) error {
	dev := h.cfg.Device
	if dev == "" {
		d, err := h.findWiFiDevice(ctx)
		if err != nil {
			return err
		}
		dev = d
	}

	args := []string{"dev", "wifi", "hotspot", "ifname", dev, "ssid", h.cfg.SSID}
	if h.cfg.Password != "" {
		args = append(args, "password", h.cfg.Password)
	}

	out, err := h.net.runner.CombinedOutput(ctx, "nmcli", args...)
	if err != nil {
		return fmt.Errorf("start hotspot: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	h.mu.Lock()
	h.active = true
	h.activeSSID = h.cfg.SSID
	h.activeDev = dev
	h.mu.Unlock()

	h.net.log.Info("fallback setup hotspot activated", "ssid", h.cfg.SSID, "dev", dev)
	return nil
}

// StopHotspot deactivates any active hotspot.
func (h *HotspotManager) StopHotspot(ctx context.Context) error {
	_, _ = h.net.runner.CombinedOutput(ctx, "nmcli", "con", "down", "Hotspot")
	if h.cfg.SSID != "" {
		_, _ = h.net.runner.CombinedOutput(ctx, "nmcli", "con", "down", h.cfg.SSID)
	}

	h.mu.Lock()
	h.active = false
	h.mu.Unlock()

	h.net.log.Info("hotspot deactivated")
	return nil
}

// Run starts the background check loop to automatically start the hotspot when offline.
func (h *HotspotManager) Run(ctx context.Context) {
	if !h.cfg.Enabled {
		return
	}

	ticker := time.NewTicker(h.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkStep(ctx)
		}
	}
}

// hasUplink reports whether a Wi-Fi or Ethernet interface is connected to something
// other than this fallback hotspot. The hotspot's own link must not count, or the
// watchdog brings the AP up and then immediately tears it down.
func (h *HotspotManager) hasUplink(st *Status) bool {
	for _, iface := range st.Interfaces {
		if iface.State != "connected" || h.isHotspotLink(iface) {
			continue
		}
		if iface.Type == "wifi" || iface.Type == "ethernet" {
			return true
		}
	}
	return false
}

func (h *HotspotManager) isHotspotLink(iface Interface) bool {
	name := iface.Connection
	return name != "" && (name == "Hotspot" || name == h.cfg.SSID)
}

func (h *HotspotManager) checkStep(ctx context.Context) {
	st, err := h.net.GetStatus(ctx)
	if err != nil || !st.Available {
		return
	}

	uplink := h.hasUplink(st)

	h.mu.Lock()
	defer h.mu.Unlock()

	if uplink {
		h.offlineSince = time.Time{}
		if h.active || h.starting {
			go func() {
				_ = h.StopHotspot(context.Background())
			}()
		}
		return
	}

	// Hotspot is already up, or a start is in flight. Its own connection is not an uplink.
	if h.active || h.starting {
		return
	}

	now := time.Now()
	if h.offlineSince.IsZero() {
		h.offlineSince = now
		return
	}

	if now.Sub(h.offlineSince) >= h.cfg.GracePeriod {
		h.starting = true
		h.net.log.Info("offline timeout reached, launching fallback hotspot")
		go func() {
			err := h.StartHotspot(context.Background())
			h.mu.Lock()
			h.starting = false
			h.mu.Unlock()
			if err != nil {
				h.net.log.Error("fallback hotspot start failed", "err", err)
			}
		}()
	}
}

// HTTP handlers for Hotspot

func (n *Network) handleHotspotStatus(w http.ResponseWriter, r *http.Request) {
	if n.hotspot == nil {
		httpx.WriteJSON(w, http.StatusOK, HotspotStatus{Active: false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status := n.hotspot.Status(ctx)
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (n *Network) handleHotspotStart(w http.ResponseWriter, r *http.Request) {
	if n.hotspot == nil {
		httpx.Error(w, http.StatusBadRequest, "hotspot manager not enabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := n.hotspot.StartHotspot(ctx); err != nil {
		n.log.Error("start hotspot failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "started", "ssid": n.hotspot.cfg.SSID})
}

func (n *Network) handleHotspotStop(w http.ResponseWriter, r *http.Request) {
	if n.hotspot == nil {
		httpx.Error(w, http.StatusBadRequest, "hotspot manager not enabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := n.hotspot.StopHotspot(ctx); err != nil {
		n.log.Error("stop hotspot failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}
