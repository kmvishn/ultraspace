package network

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

func TestSplitNMFields(t *testing.T) {
	cases := []struct {
		input    string
		expected []string
	}{
		{
			input:    "wlan0:wifi:connected:Home",
			expected: []string{"wlan0", "wifi", "connected", "Home"},
		},
		{
			input:    `wlan0:wifi:connected:My\:Special\:SSID`,
			expected: []string{"wlan0", "wifi", "connected", "My:Special:SSID"},
		},
		{
			input:    "eth0:ethernet:unavailable:",
			expected: []string{"eth0", "ethernet", "unavailable", ""},
		},
	}

	for _, tc := range cases {
		got := SplitNMFields(tc.input)
		if len(got) != len(tc.expected) {
			t.Fatalf("SplitNMFields(%q) len = %d, want %d", tc.input, len(got), len(tc.expected))
		}
		for i := range got {
			if got[i] != tc.expected[i] {
				t.Errorf("field %d = %q, want %q", i, got[i], tc.expected[i])
			}
		}
	}
}

func TestGetStatus(t *testing.T) {
	statusOutput := `wlan0:802-11-wireless:connected:HomeWiFi
eth0:802-3-ethernet:disconnected:
lo:loopback:unmanaged:
`
	devShowWlan := `GENERAL.DEVICE:wlan0
GENERAL.HWADDR:11:22:33:44:55:66
IP4.ADDRESS[1]:192.168.1.100/24
IP4.GATEWAY:192.168.1.1
IP4.DNS[1]:1.1.1.1
IP4.DNS[2]:8.8.8.8
`
	devShowEth := `GENERAL.DEVICE:eth0
GENERAL.HWADDR:AA:BB:CC:DD:EE:FF
`

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				return []byte(statusOutput), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte(devShowWlan), nil
			},
			"dev show eth0": func(args []string) ([]byte, error) {
				return []byte(devShowEth), nil
			},
			"con show --active": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	st, err := net.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.Available {
		t.Fatal("expected Available to be true")
	}
	if !st.Connected {
		t.Fatal("expected Connected to be true")
	}
	if st.Primary != "wlan0" {
		t.Errorf("Primary = %q, want %q", st.Primary, "wlan0")
	}
	if st.Type != "wifi" {
		t.Errorf("Type = %q, want %q", st.Type, "wifi")
	}
	if st.SSID != "HomeWiFi" {
		t.Errorf("SSID = %q, want %q", st.SSID, "HomeWiFi")
	}
	if st.IP != "192.168.1.100/24" {
		t.Errorf("IP = %q, want %q", st.IP, "192.168.1.100/24")
	}
	if len(st.Interfaces) != 2 {
		t.Fatalf("Interfaces len = %d, want 2", len(st.Interfaces))
	}

	wlan := st.Interfaces[0]
	if wlan.Name != "wlan0" || wlan.MAC != "11:22:33:44:55:66" || wlan.Gateway != "192.168.1.1" {
		t.Errorf("unexpected wlan iface details: %+v", wlan)
	}
	if len(wlan.DNS) != 2 || wlan.DNS[0] != "1.1.1.1" {
		t.Errorf("unexpected DNS: %+v", wlan.DNS)
	}
}

func TestParseWiFiList(t *testing.T) {
	// 2 APs for HomeWiFi (mesh: 2.4G and 5G), 1 for CoffeeShop (Open), 1 Neighbor (WPA3)
	raw := `*:AA\:BB\:CC\:11\:22\:33:HomeWiFi:6:2437 MHz:75:WPA2
 :AA\:BB\:CC\:11\:22\:34:HomeWiFi:36:5180 MHz:88:WPA2
 :11\:22\:33\:44\:55\:66:CoffeeShop:1:2412 MHz:90:--
 :22\:33\:44\:55\:66\:77:Neighbor5G:40:5200 MHz:42:WPA3
 :33\:44\:55\:66\:77\:88:--:1:2412 MHz:60:WPA2
`
	aps := ParseWiFiList([]byte(raw))

	if len(aps) != 3 { // Hidden network (--) omitted, HomeWiFi deduped
		t.Fatalf("ParseWiFiList len = %d, want 3", len(aps))
	}

	// First should be HomeWiFi because InUse = true
	if aps[0].SSID != "HomeWiFi" {
		t.Errorf("first AP = %q, want HomeWiFi", aps[0].SSID)
	}
	if !aps[0].InUse {
		t.Error("expected HomeWiFi to be InUse")
	}

	// Check CoffeeShop security parsed as Open
	var coffee *WiFiAccessPoint
	for i := range aps {
		if aps[i].SSID == "CoffeeShop" {
			coffee = &aps[i]
		}
	}
	if coffee == nil {
		t.Fatal("CoffeeShop not found")
	}
	if coffee.Security != "Open" {
		t.Errorf("CoffeeShop Security = %q, want Open", coffee.Security)
	}
}

func TestConnectWiFi(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"connect ValidNetwork": func(args []string) ([]byte, error) {
				return []byte("Device 'wlan0' successfully activated"), nil
			},
			"connect WrongPassword": func(args []string) ([]byte, error) {
				return []byte("Error: Connection activation failed: (7) Secrets were required, but not provided."), errors.New("exit 1")
			},
			"connect MissingNetwork": func(args []string) ([]byte, error) {
				return []byte("Error: No network with SSID 'MissingNetwork' found."), errors.New("exit 1")
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)

	// Success
	if err := net.ConnectWiFi(context.Background(), ConnectRequest{SSID: "ValidNetwork", Password: "pass"}); err != nil {
		t.Errorf("expected success, got err: %v", err)
	}

	// Empty SSID
	if err := net.ConnectWiFi(context.Background(), ConnectRequest{SSID: ""}); err == nil {
		t.Error("expected error for empty SSID")
	}

	// Invalid SSID with control character
	if err := net.ConnectWiFi(context.Background(), ConnectRequest{SSID: "Bad\nSSID"}); err == nil {
		t.Error("expected error for SSID with newline")
	}

	// Bad password
	err := net.ConnectWiFi(context.Background(), ConnectRequest{SSID: "WrongPassword", Password: "bad"})
	if err == nil || !strings.Contains(err.Error(), "incorrect Wi-Fi password") {
		t.Errorf("expected incorrect password error, got: %v", err)
	}

	// Missing network
	err = net.ConnectWiFi(context.Background(), ConnectRequest{SSID: "MissingNetwork"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestHTTPHandlers(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				return []byte("wlan0:wifi:connected:TestWiFi\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\nIP4.ADDRESS[1]:10.0.0.5/24\n"), nil
			},
			"wifi list": func(args []string) ([]byte, error) {
				return []byte("*:AA:TestWiFi:1:2412 MHz:80:WPA2\n"), nil
			},
			"connect TestWiFi": func(args []string) ([]byte, error) {
				return []byte("successfully activated"), nil
			},
			"disconnect wlan0": func(args []string) ([]byte, error) {
				return []byte("Device 'wlan0' successfully disconnected"), nil
			},
			"con show --active": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	net.Register(mux, passAuth)

	// 1. GET /api/network/status
	req := httptest.NewRequest("GET", "/api/network/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/network/status code = %d, want 200", w.Code)
	}
	var st Status
	if err := json.NewDecoder(w.Body).Decode(&st); err != nil {
		t.Fatalf("decode status err: %v", err)
	}
	if st.SSID != "TestWiFi" {
		t.Errorf("status SSID = %q, want TestWiFi", st.SSID)
	}

	// 2. GET /api/network/wifi/scan
	req = httptest.NewRequest("GET", "/api/network/wifi/scan", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/network/wifi/scan code = %d", w.Code)
	}
	var scanRes struct {
		Networks []WiFiAccessPoint `json:"networks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&scanRes); err != nil {
		t.Fatalf("decode scan err: %v", err)
	}
	if len(scanRes.Networks) != 1 || scanRes.Networks[0].SSID != "TestWiFi" {
		t.Fatalf("unexpected networks: %+v", scanRes.Networks)
	}

	// 3. POST /api/network/wifi/connect
	body, _ := json.Marshal(ConnectRequest{SSID: "TestWiFi", Password: "test"})
	req = httptest.NewRequest("POST", "/api/network/wifi/connect", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("POST /api/network/wifi/connect code = %d, body: %s", w.Code, string(b))
	}

	// 4. POST /api/network/wifi/disconnect
	body, _ = json.Marshal(DisconnectRequest{Device: "wlan0"})
	req = httptest.NewRequest("POST", "/api/network/wifi/disconnect", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/network/wifi/disconnect code = %d", w.Code)
	}
}

