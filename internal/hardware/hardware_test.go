package hardware

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
	fullCmd := name
	if len(args) > 0 {
		fullCmd += " " + strings.Join(args, " ")
	}
	for pattern, handler := range m.handlers {
		if strings.Contains(fullCmd, pattern) {
			return handler(args)
		}
	}
	return nil, errors.New("command not mocked: " + fullCmd)
}

const sampleBTShow = `Controller 00:1A:7D:DA:71:13 (public)
	Name: Laptop-BT
	Alias: Laptop-BT
	Class: 0x006c010c
	Powered: yes
	Discoverable: no
	Pairable: yes
`

const sampleBTDevices = `Device 00:11:22:33:44:55 Sony WH-1000XM4
Device AA:BB:CC:DD:EE:FF Logitech MX Master 3
`

const sampleBTInfo = `Device 00:11:22:33:44:55 (public)
	Name: Sony WH-1000XM4
	Paired: yes
	Trusted: yes
	Connected: yes
	Icon: audio-headphones
`

const sampleLSUSB = `Bus 001 Device 001: ID 1d6b:0002 Linux Foundation 2.0 root hub
Bus 001 Device 003: ID 046d:c52b Logitech, Inc. Unifying Receiver
Bus 001 Device 002: ID 0bda:5682 Realtek Semiconductor Corp. Integrated Webcam
Bus 002 Device 002: ID 1058:25a2 Western Digital Technologies, Inc. Elements 25A2
`

const sampleLSPCI = `00:00.0 Host bridge: Intel Corporation Xeon E3-1200 v6/7th Gen Core Processor Host Bridge/DRAM Registers (rev 02)
00:02.0 VGA compatible controller: Intel Corporation UHD Graphics 620 (rev 07)
00:14.0 USB controller: Intel Corporation Sunrise Point-LP USB 3.0 xHCI Controller (rev 21)
00:1f.3 Audio device: Intel Corporation Sunrise Point-LP HD Audio (rev 21)
01:00.0 Network controller: Intel Corporation Wireless 8265 / 8275 (rev 78)
`

func TestParseBluetoothController(t *testing.T) {
	ctrl := ParseBluetoothController([]byte(sampleBTShow))
	if !ctrl.Available {
		t.Fatal("expected Available to be true")
	}
	if ctrl.MAC != "00:1A:7D:DA:71:13" {
		t.Errorf("MAC = %q", ctrl.MAC)
	}
	if ctrl.Name != "Laptop-BT" {
		t.Errorf("Name = %q", ctrl.Name)
	}
	if !ctrl.Powered {
		t.Error("expected Powered = true")
	}
}

func TestParseBluetoothDevices(t *testing.T) {
	devs := ParseBluetoothDevices([]byte(sampleBTDevices))
	if len(devs) != 2 {
		t.Fatalf("devs len = %d, want 2", len(devs))
	}
	if devs[0].MAC != "00:11:22:33:44:55" || devs[0].Name != "Sony WH-1000XM4" {
		t.Errorf("dev[0] = %+v", devs[0])
	}

	ParseDeviceInfo([]byte(sampleBTInfo), &devs[0])
	if !devs[0].Connected || !devs[0].Paired || devs[0].Icon != "audio-headphones" {
		t.Errorf("unexpected dev[0] info: %+v", devs[0])
	}
}

func TestParseLSUSB(t *testing.T) {
	usbs := ParseLSUSB([]byte(sampleLSUSB))
	// Root hub should be skipped, leaving 3 devices: Receiver (Input), Webcam (Camera), Elements (Storage)
	if len(usbs) != 3 {
		t.Fatalf("usbs len = %d, want 3", len(usbs))
	}

	if usbs[0].Category != "Input" {
		t.Errorf("usbs[0] category = %q, want Input", usbs[0].Category)
	}
	if usbs[1].Category != "Camera" {
		t.Errorf("usbs[1] category = %q, want Camera", usbs[1].Category)
	}
	if usbs[2].Category != "Storage" {
		t.Errorf("usbs[2] category = %q, want Storage", usbs[2].Category)
	}
}

func TestParseLSPCI(t *testing.T) {
	pcis := ParseLSPCI([]byte(sampleLSPCI))
	// VGA, Audio, and Network should be retained
	if len(pcis) != 3 {
		t.Fatalf("pcis len = %d, want 3", len(pcis))
	}
	if !strings.Contains(pcis[0].Description, "Intel Corporation UHD Graphics 620") {
		t.Errorf("pcis[0] = %+v", pcis[0])
	}
	if !strings.Contains(pcis[2].Description, "Wireless 8265") {
		t.Errorf("pcis[2] = %+v", pcis[2])
	}
}

func TestGetStatus(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"bluetoothctl show": func(args []string) ([]byte, error) {
				return []byte(sampleBTShow), nil
			},
			"bluetoothctl devices": func(args []string) ([]byte, error) {
				return []byte(sampleBTDevices), nil
			},
			"info 00:11:22:33:44:55": func(args []string) ([]byte, error) {
				return []byte(sampleBTInfo), nil
			},
			"info AA:BB:CC:DD:EE:FF": func(args []string) ([]byte, error) {
				return []byte("Name: MX Master 3\nPaired: yes\n"), nil
			},
			"lsusb": func(args []string) ([]byte, error) {
				return []byte(sampleLSUSB), nil
			},
			"lspci": func(args []string) ([]byte, error) {
				return []byte(sampleLSPCI), nil
			},
		},
	}

	hw := NewWithRunner(slog.Default(), runner)
	st, err := hw.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.Bluetooth.Available {
		t.Error("expected Bluetooth.Available = true")
	}
	if len(st.BluetoothDevices) != 2 {
		t.Errorf("BluetoothDevices len = %d, want 2", len(st.BluetoothDevices))
	}
	if len(st.USBDevices) != 3 {
		t.Errorf("USBDevices len = %d, want 3", len(st.USBDevices))
	}
	if len(st.PCIDevices) != 3 {
		t.Errorf("PCIDevices len = %d, want 3", len(st.PCIDevices))
	}
}

func TestBluetoothActions(t *testing.T) {
	var capturedCmd string
	var mu sync.Mutex

	record := func(action string) func(args []string) ([]byte, error) {
		return func(args []string) ([]byte, error) {
			mu.Lock()
			capturedCmd = action + " " + strings.Join(args, " ")
			mu.Unlock()
			return []byte("ok"), nil
		}
	}

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"power on":     record("power-on"),
			"connect":      record("connect"),
			"disconnect":   record("disconnect"),
			"pair":         record("pair"),
			"trust":        record("trust"),
			"remove":       record("remove"),
			"scan on":      record("scan"),
			"show":         func(args []string) ([]byte, error) { return []byte(sampleBTShow), nil },
			"devices":      func(args []string) ([]byte, error) { return []byte(""), nil },
		},
	}

	hw := NewWithRunner(slog.Default(), runner)

	// Power
	_ = hw.SetBluetoothPower(context.Background(), true)
	mu.Lock()
	if !strings.Contains(capturedCmd, "power-on") {
		t.Errorf("cmd = %q", capturedCmd)
	}
	mu.Unlock()

	// Connect
	_ = hw.ConnectBluetooth(context.Background(), "AA:BB:CC:DD:EE:FF")
	mu.Lock()
	if !strings.Contains(capturedCmd, "connect") {
		t.Errorf("cmd = %q", capturedCmd)
	}
	mu.Unlock()

	// Disconnect
	_ = hw.DisconnectBluetooth(context.Background(), "AA:BB:CC:DD:EE:FF")
	mu.Lock()
	if !strings.Contains(capturedCmd, "disconnect") {
		t.Errorf("cmd = %q", capturedCmd)
	}
	mu.Unlock()

	// Pair
	_ = hw.PairBluetooth(context.Background(), "AA:BB:CC:DD:EE:FF")
	mu.Lock()
	if !strings.Contains(capturedCmd, "connect") { // last step in pair
		t.Errorf("cmd = %q", capturedCmd)
	}
	mu.Unlock()

	// Remove
	_ = hw.RemoveBluetooth(context.Background(), "AA:BB:CC:DD:EE:FF")
	mu.Lock()
	if !strings.Contains(capturedCmd, "remove") {
		t.Errorf("cmd = %q", capturedCmd)
	}
	mu.Unlock()

	// Reject invalid MACs
	if err := hw.ConnectBluetooth(context.Background(), "invalid-mac-address"); err == nil {
		t.Errorf("expected error for invalid MAC")
	}
	if err := hw.ConnectBluetooth(context.Background(), "00:11;rm -rf /"); err == nil {
		t.Errorf("expected error for injection MAC")
	}
}

func TestHardwareHTTPHandlers(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"bluetoothctl show": func(args []string) ([]byte, error) {
				return []byte(sampleBTShow), nil
			},
			"bluetoothctl devices": func(args []string) ([]byte, error) {
				return []byte(sampleBTDevices), nil
			},
			"info": func(args []string) ([]byte, error) {
				return []byte(sampleBTInfo), nil
			},
			"lsusb": func(args []string) ([]byte, error) {
				return []byte(sampleLSUSB), nil
			},
			"lspci": func(args []string) ([]byte, error) {
				return []byte(sampleLSPCI), nil
			},
			"power": func(args []string) ([]byte, error) {
				return []byte("ok"), nil
			},
			"connect": func(args []string) ([]byte, error) {
				return []byte("ok"), nil
			},
		},
	}

	hw := NewWithRunner(slog.Default(), runner)
	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	hw.Register(mux, passAuth)

	// 1. GET /api/hardware/status
	req := httptest.NewRequest("GET", "/api/hardware/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/hardware/status code = %d", w.Code)
	}
	var st Status
	if err := json.NewDecoder(w.Body).Decode(&st); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !st.Bluetooth.Available || len(st.USBDevices) != 3 {
		t.Errorf("unexpected status: %+v", st)
	}

	// 2. POST /api/hardware/bluetooth/power
	body, _ := json.Marshal(BluetoothPowerRequest{Powered: true})
	req = httptest.NewRequest("POST", "/api/hardware/bluetooth/power", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/hardware/bluetooth/power code = %d", w.Code)
	}

	// 3. POST /api/hardware/bluetooth/connect
	body, _ = json.Marshal(BluetoothActionRequest{MAC: "00:11:22:33:44:55"})
	req = httptest.NewRequest("POST", "/api/hardware/bluetooth/connect", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("POST /api/hardware/bluetooth/connect code = %d, body = %s", w.Code, string(b))
	}
}
