// Package hardware manages system Bluetooth controllers and devices, USB peripherals, and PCI hardware on Linux.
package hardware

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

// BluetoothController represents the host Bluetooth adapter.
type BluetoothController struct {
	Available    bool   `json:"available"`
	Name         string `json:"name"`
	MAC          string `json:"mac"`
	Powered      bool   `json:"powered"`
	Discoverable bool   `json:"discoverable"`
	Pairable     bool   `json:"pairable"`
}

// BluetoothDevice represents a paired or discovered wireless Bluetooth peripheral.
type BluetoothDevice struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Trusted   bool   `json:"trusted"`
	Icon      string `json:"icon"` // "audio-card", "audio-headphones", "input-keyboard", "input-mouse", "phone", etc.
}

// USBDevice represents a connected USB peripheral.
type USBDevice struct {
	Bus         string `json:"bus"`
	Device      string `json:"device"`
	ID          string `json:"id"`          // "046d:c52b"
	Description string `json:"description"` // vendor + product name
	Category    string `json:"category"`    // "Storage", "Audio", "Input", "Camera", "Network", "Other"
}

// PCIDevice represents an onboard or expansion PCI controller (GPU, Wi-Fi, etc.).
type PCIDevice struct {
	Slot        string `json:"slot"`
	Class       string `json:"class"`       // "VGA compatible controller", "Network controller"
	Description string `json:"description"` // e.g. "Intel Corporation UHD Graphics 620"
}

// Status summarizes host hardware.
type Status struct {
	Bluetooth        BluetoothController `json:"bluetooth"`
	BluetoothDevices []BluetoothDevice   `json:"bluetooth_devices"`
	USBDevices       []USBDevice         `json:"usb_devices"`
	PCIDevices       []PCIDevice         `json:"pci_devices"`
}

type BluetoothActionRequest struct {
	MAC string `json:"mac"`
}

type BluetoothPowerRequest struct {
	Powered bool `json:"powered"`
}

// Hardware coordinates hardware device inspection.
type Hardware struct {
	log    *slog.Logger
	runner Runner
}

// New creates a Hardware manager with execRunner.
func New(log *slog.Logger) *Hardware {
	return &Hardware{log: log, runner: execRunner{}}
}

// NewWithRunner creates a Hardware manager with custom Runner for testing.
func NewWithRunner(log *slog.Logger, r Runner) *Hardware {
	return &Hardware{log: log, runner: r}
}

// Register wires HTTP endpoints into the router.
func (h *Hardware) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/hardware/status", auth(http.HandlerFunc(h.handleStatus)))
	mux.Handle("POST /api/hardware/bluetooth/power", auth(http.HandlerFunc(h.handleBTPower)))
	mux.Handle("POST /api/hardware/bluetooth/scan", auth(http.HandlerFunc(h.handleBTScan)))
	mux.Handle("POST /api/hardware/bluetooth/connect", auth(http.HandlerFunc(h.handleBTConnect)))
	mux.Handle("POST /api/hardware/bluetooth/disconnect", auth(http.HandlerFunc(h.handleBTDisconnect)))
	mux.Handle("POST /api/hardware/bluetooth/pair", auth(http.HandlerFunc(h.handleBTPair)))
	mux.Handle("POST /api/hardware/bluetooth/remove", auth(http.HandlerFunc(h.handleBTRemove)))
}

// ParseBluetoothController parses `bluetoothctl show` output.
func ParseBluetoothController(out []byte) BluetoothController {
	ctrl := BluetoothController{}
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Controller ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ctrl.MAC = parts[1]
				ctrl.Available = true
			}
		}

		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "Name":
			ctrl.Name = v
		case "Powered":
			ctrl.Powered = (strings.ToLower(v) == "yes")
		case "Discoverable":
			ctrl.Discoverable = (strings.ToLower(v) == "yes")
		case "Pairable":
			ctrl.Pairable = (strings.ToLower(v) == "yes")
		}
	}
	return ctrl
}

// ParseBluetoothDevices parses lines from `bluetoothctl devices` or `bluetoothctl paired-devices`.
func ParseBluetoothDevices(out []byte) []BluetoothDevice {
	var list []BluetoothDevice
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Device ") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) >= 3 {
			mac := parts[1]
			name := parts[2]
			list = append(list, BluetoothDevice{
				MAC:  mac,
				Name: name,
			})
		}
	}
	return list
}

// ParseDeviceInfo parses detailed properties from `bluetoothctl info <mac>`.
func ParseDeviceInfo(out []byte, dev *BluetoothDevice) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "Name":
			if dev.Name == "" || dev.Name == dev.MAC {
				dev.Name = v
			}
		case "Paired":
			dev.Paired = (strings.ToLower(v) == "yes")
		case "Connected":
			dev.Connected = (strings.ToLower(v) == "yes")
		case "Trusted":
			dev.Trusted = (strings.ToLower(v) == "yes")
		case "Icon":
			dev.Icon = v
		}
	}
}

var usbRe = regexp.MustCompile(`^Bus\s+(\d+)\s+Device\s+(\d+):\s+ID\s+([0-9a-fA-F]{4}:[0-9a-fA-F]{4})\s+(.+)$`)

func detectUSBCategory(desc string) string {
	lower := strings.ToLower(desc)
	switch {
	case strings.Contains(lower, "storage") || strings.Contains(lower, "flash") || strings.Contains(lower, "mass") || strings.Contains(lower, "hard drive") || strings.Contains(lower, "elements") || strings.Contains(lower, "disk"):
		return "Storage"
	case strings.Contains(lower, "audio") || strings.Contains(lower, "sound") || strings.Contains(lower, "dac") || strings.Contains(lower, "headset") || strings.Contains(lower, "speaker"):
		return "Audio"
	case strings.Contains(lower, "keyboard") || strings.Contains(lower, "mouse") || strings.Contains(lower, "unifying") || strings.Contains(lower, "receiver") || strings.Contains(lower, "touchpad"):
		return "Input"
	case strings.Contains(lower, "cam") || strings.Contains(lower, "video"):
		return "Camera"
	case strings.Contains(lower, "wireless") || strings.Contains(lower, "bluetooth") || strings.Contains(lower, "ethernet") || strings.Contains(lower, "802.11"):
		return "Network"
	case strings.Contains(lower, "hub") || strings.Contains(lower, "root hub"):
		return "Hub"
	default:
		return "Peripheral"
	}
}

// ParseLSUSB parses `lsusb` output lines.
func ParseLSUSB(out []byte) []USBDevice {
	var list []USBDevice
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		m := usbRe.FindStringSubmatch(line)
		if len(m) == 5 {
			bus := m[1]
			devNum := m[2]
			id := m[3]
			desc := m[4]

			cat := detectUSBCategory(desc)
			if cat == "Hub" {
				continue // skip internal root hubs for a cleaner user view
			}

			list = append(list, USBDevice{
				Bus:         bus,
				Device:      devNum,
				ID:          id,
				Description: desc,
				Category:    cat,
			})
		}
	}
	return list
}

// ParseLSPCI parses `lspci` output lines.
func ParseLSPCI(out []byte) []PCIDevice {
	var list []PCIDevice
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Format: 00:02.0 VGA compatible controller: Intel Corporation UHD Graphics 620 (rev 07)
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			slot := parts[0]
			rest := parts[1]
			class, desc, ok := strings.Cut(rest, ": ")
			if ok {
				// Only keep interesting end-user controllers (VGA/Display, Network/Wi-Fi, Audio, Storage)
				lower := strings.ToLower(class)
				if strings.Contains(lower, "vga") || strings.Contains(lower, "3d") || strings.Contains(lower, "display") ||
					strings.Contains(lower, "network") || strings.Contains(lower, "wireless") || strings.Contains(lower, "ethernet") ||
					strings.Contains(lower, "audio") || strings.Contains(lower, "non-volatile") || strings.Contains(lower, "sata") {
					list = append(list, PCIDevice{
						Slot:        slot,
						Class:       class,
						Description: desc,
					})
				}
			}
		}
	}
	return list
}

// GetStatus collects current Bluetooth, USB, and PCI hardware information.
func (h *Hardware) GetStatus(ctx context.Context) (*Status, error) {
	status := &Status{
		BluetoothDevices: []BluetoothDevice{},
		USBDevices:       []USBDevice{},
		PCIDevices:       []PCIDevice{},
	}

	// 1. Bluetooth Controller
	showOut, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "show")
	if err == nil {
		status.Bluetooth = ParseBluetoothController(showOut)
	}

	// 2. Bluetooth Devices
	if status.Bluetooth.Available {
		devOut, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "devices")
		if err == nil {
			devs := ParseBluetoothDevices(devOut)
			for i := range devs {
				infoOut, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "info", devs[i].MAC)
				if err == nil {
					ParseDeviceInfo(infoOut, &devs[i])
				}
			}
			status.BluetoothDevices = devs
		}
	}

	// 3. USB Devices
	usbOut, err := h.runner.CombinedOutput(ctx, "lsusb")
	if err == nil {
		status.USBDevices = ParseLSUSB(usbOut)
	}

	// 4. PCI Controllers
	pciOut, err := h.runner.CombinedOutput(ctx, "lspci")
	if err == nil {
		status.PCIDevices = ParseLSPCI(pciOut)
	}

	return status, nil
}

// SetBluetoothPower toggles the Bluetooth controller power state.
func (h *Hardware) SetBluetoothPower(ctx context.Context, powered bool) error {
	arg := "off"
	if powered {
		arg = "on"
	}
	out, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "power", arg)
	if err != nil {
		return fmt.Errorf("bluetooth power %s: %w (%s)", arg, err, strings.TrimSpace(string(out)))
	}
	if powered {
		_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "pairable", "on")
	}
	return nil
}

// ScanBluetooth triggers a 5-second discovery scan for nearby Bluetooth devices.
func (h *Hardware) ScanBluetooth(ctx context.Context) ([]BluetoothDevice, error) {
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "pairable", "on")
	// Trigger scan
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "--timeout", "5", "scan", "on")

	// Query discovered devices
	devOut, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "devices")
	if err != nil {
		return nil, fmt.Errorf("bluetooth devices: %w", err)
	}

	devs := ParseBluetoothDevices(devOut)
	for i := range devs {
		infoOut, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "info", devs[i].MAC)
		if err == nil {
			ParseDeviceInfo(infoOut, &devs[i])
		}
	}
	return devs, nil
}

var macRegex = regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$`)

func isValidMAC(mac string) bool {
	return macRegex.MatchString(strings.TrimSpace(mac))
}

// ConnectBluetooth connects to a Bluetooth device.
func (h *Hardware) ConnectBluetooth(ctx context.Context, mac string) error {
	mac = strings.TrimSpace(mac)
	if !isValidMAC(mac) {
		return errors.New("invalid MAC address format")
	}
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "pairable", "on")
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "trust", mac)
	out, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "connect", mac)
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if strings.Contains(outStr, "br-connection-profile-unavailable") || strings.Contains(outStr, "Protocol not available") {
			// If not paired yet, attempt pair first then connect
			pairOut, pairErr := h.runner.CombinedOutput(ctx, "bluetoothctl", "pair", mac)
			if pairErr == nil {
				connOut, connErr := h.runner.CombinedOutput(ctx, "bluetoothctl", "connect", mac)
				if connErr == nil {
					return nil
				}
				outStr = strings.TrimSpace(string(connOut))
			} else {
				outStr = strings.TrimSpace(string(pairOut))
			}
			if strings.Contains(outStr, "br-connection-profile-unavailable") || strings.Contains(outStr, "Protocol not available") {
				return fmt.Errorf("bluetooth audio profile unavailable: ensure pulseaudio-module-bluetooth or libspa-0.2-bluetooth is running on the host")
			}
		}
		return fmt.Errorf("connect %s: %w (%s)", mac, err, outStr)
	}
	return nil
}

// DisconnectBluetooth disconnects an active Bluetooth connection.
func (h *Hardware) DisconnectBluetooth(ctx context.Context, mac string) error {
	mac = strings.TrimSpace(mac)
	if !isValidMAC(mac) {
		return errors.New("invalid MAC address format")
	}
	out, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "disconnect", mac)
	if err != nil {
		return fmt.Errorf("disconnect %s: %w (%s)", mac, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PairBluetooth pairs and trusts a Bluetooth device.
func (h *Hardware) PairBluetooth(ctx context.Context, mac string) error {
	mac = strings.TrimSpace(mac)
	if !isValidMAC(mac) {
		return errors.New("invalid MAC address format")
	}
	out, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "pair", mac)
	if err != nil {
		return fmt.Errorf("pair %s: %w (%s)", mac, err, strings.TrimSpace(string(out)))
	}
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "trust", mac)
	_, _ = h.runner.CombinedOutput(ctx, "bluetoothctl", "connect", mac)
	return nil
}

// RemoveBluetooth unpairs and removes a Bluetooth device.
func (h *Hardware) RemoveBluetooth(ctx context.Context, mac string) error {
	mac = strings.TrimSpace(mac)
	if !isValidMAC(mac) {
		return errors.New("invalid MAC address format")
	}
	out, err := h.runner.CombinedOutput(ctx, "bluetoothctl", "remove", mac)
	if err != nil {
		return fmt.Errorf("remove %s: %w (%s)", mac, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HTTP Handlers

func (h *Hardware) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	status, err := h.GetStatus(ctx)
	if err != nil {
		h.log.Error("get hardware status", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to get hardware status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (h *Hardware) handleBTPower(w http.ResponseWriter, r *http.Request) {
	var req BluetoothPowerRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := h.SetBluetoothPower(ctx, req.Powered); err != nil {
		h.log.Error("set bt power", "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "powered": req.Powered})
}

func (h *Hardware) handleBTScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	devs, err := h.ScanBluetooth(ctx)
	if err != nil {
		h.log.Error("scan bluetooth", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

func (h *Hardware) handleBTConnect(w http.ResponseWriter, r *http.Request) {
	var req BluetoothActionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.ConnectBluetooth(ctx, req.MAC); err != nil {
		h.log.Error("connect bt device", "mac", req.MAC, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "connected", "mac": req.MAC})
}

func (h *Hardware) handleBTDisconnect(w http.ResponseWriter, r *http.Request) {
	var req BluetoothActionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := h.DisconnectBluetooth(ctx, req.MAC); err != nil {
		h.log.Error("disconnect bt device", "mac", req.MAC, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "disconnected", "mac": req.MAC})
}

func (h *Hardware) handleBTPair(w http.ResponseWriter, r *http.Request) {
	var req BluetoothActionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := h.PairBluetooth(ctx, req.MAC); err != nil {
		h.log.Error("pair bt device", "mac", req.MAC, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "paired", "mac": req.MAC})
}

func (h *Hardware) handleBTRemove(w http.ResponseWriter, r *http.Request) {
	var req BluetoothActionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := h.RemoveBluetooth(ctx, req.MAC); err != nil {
		h.log.Error("remove bt device", "mac", req.MAC, "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "removed", "mac": req.MAC})
}

