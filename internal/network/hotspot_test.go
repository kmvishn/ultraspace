package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHotspotStatus(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"con show --active": func(args []string) ([]byte, error) {
				return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\nIP4.ADDRESS[1]:10.42.0.1/24\n"), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	hm := net.Hotspot()

	st := hm.Status(context.Background())
	if !st.Active {
		t.Fatal("expected hotspot to be active")
	}
	if st.Device != "wlan0" {
		t.Errorf("Device = %q, want wlan0", st.Device)
	}
	if st.IP != "10.42.0.1/24" {
		t.Errorf("IP = %q, want 10.42.0.1/24", st.IP)
	}
}

func TestStartAndStopHotspot(t *testing.T) {
	var startCalled, stopCalled bool
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				return []byte("wlan0:wifi:disconnected:\n"), nil
			},
			"wifi hotspot": func(args []string) ([]byte, error) {
				mu.Lock()
				startCalled = true
				mu.Unlock()
				return []byte("Device 'wlan0' successfully activated with 'Hotspot'"), nil
			},
			"con down": func(args []string) ([]byte, error) {
				mu.Lock()
				stopCalled = true
				mu.Unlock()
				return []byte("Connection 'Hotspot' successfully deactivated"), nil
			},
			"con show --active": func(args []string) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				if startCalled && !stopCalled {
					return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
				}
				return []byte(""), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\nIP4.ADDRESS[1]:10.42.0.1/24\n"), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	hm := net.Hotspot()

	// 1. Start Hotspot
	if err := hm.StartHotspot(context.Background()); err != nil {
		t.Fatalf("StartHotspot err: %v", err)
	}
	mu.Lock()
	if !startCalled {
		t.Error("expected nmcli wifi hotspot to be called")
	}
	mu.Unlock()

	// 2. Stop Hotspot
	if err := hm.StopHotspot(context.Background()); err != nil {
		t.Fatalf("StopHotspot err: %v", err)
	}
	mu.Lock()
	if !stopCalled {
		t.Error("expected nmcli con down to be called")
	}
	mu.Unlock()
}

func TestHotspotAutoFallback(t *testing.T) {
	var hotspotStarted bool
	var mu sync.Mutex

	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				// No interface connected
				return []byte("wlan0:wifi:disconnected:\neth0:ethernet:unavailable:\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\n"), nil
			},
			"dev show eth0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:eth0\n"), nil
			},
			"wifi hotspot": func(args []string) ([]byte, error) {
				mu.Lock()
				hotspotStarted = true
				mu.Unlock()
				return []byte("Device 'wlan0' successfully activated with 'Hotspot'"), nil
			},
			"con show --active": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	cfg := DefaultHotspotConfig()
	cfg.GracePeriod = 20 * time.Millisecond
	cfg.CheckInterval = 10 * time.Millisecond
	hm := net.EnableHotspot(cfg)

	// First step records offlineSince
	hm.checkStep(context.Background())

	// Wait past grace period
	time.Sleep(35 * time.Millisecond)

	// Second step should trigger StartHotspot
	hm.checkStep(context.Background())

	// Allow goroutine to execute
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	started := hotspotStarted
	mu.Unlock()

	if !started {
		t.Error("expected auto-hotspot to launch after offline grace period")
	}
}

func TestHotspotHTTPHandlers(t *testing.T) {
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"con show --active": func(args []string) ([]byte, error) {
				return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\nIP4.ADDRESS[1]:10.42.0.1/24\n"), nil
			},
			"wifi hotspot": func(args []string) ([]byte, error) {
				return []byte("Device 'wlan0' successfully activated"), nil
			},
			"con down": func(args []string) ([]byte, error) {
				return []byte("Connection successfully deactivated"), nil
			},
			"dev status": func(args []string) ([]byte, error) {
				return []byte("wlan0:wifi:connected:Hotspot\n"), nil
			},
		},
	}

	net := NewWithRunner(slog.Default(), runner)
	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	net.Register(mux, passAuth)

	// 1. GET /api/network/hotspot
	req := httptest.NewRequest("GET", "/api/network/hotspot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/network/hotspot code = %d", w.Code)
	}
	var hs HotspotStatus
	if err := json.NewDecoder(w.Body).Decode(&hs); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !hs.Active {
		t.Error("expected HotspotStatus.Active to be true")
	}

	// 2. POST /api/network/hotspot/stop
	req = httptest.NewRequest("POST", "/api/network/hotspot/stop", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("POST /api/network/hotspot/stop code = %d, body: %s", w.Code, string(b))
	}

	// 3. POST /api/network/hotspot/start
	req = httptest.NewRequest("POST", "/api/network/hotspot/start", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("POST /api/network/hotspot/start code = %d, body: %s", w.Code, string(b))
	}
}

func TestHotspotStaysUpWithoutUplink(t *testing.T) {
	for _, conn := range []string{"Hotspot", "Ultraspace-Setup"} {
		t.Run(conn, func(t *testing.T) {
			var stopped bool
			var mu sync.Mutex
			runner := &mockRunner{
				handlers: map[string]func(args []string) ([]byte, error){
					"dev status": func(args []string) ([]byte, error) {
						return []byte("wlan0:wifi:connected:" + conn + "\neth0:ethernet:unavailable:\ndocker0:bridge:connected:docker0\n"), nil
					},
					"dev show wlan0": func(args []string) ([]byte, error) {
						return []byte("IP4.ADDRESS[1]:10.42.0.1/24\n"), nil
					},
					"dev show eth0": func(args []string) ([]byte, error) {
						return []byte("GENERAL.DEVICE:eth0\n"), nil
					},
					"dev show docker0": func(args []string) ([]byte, error) {
						return []byte("IP4.ADDRESS[1]:172.17.0.1/16\n"), nil
					},
					"con show --active": func(args []string) ([]byte, error) {
						return []byte(conn + ":802-11-wireless:wlan0\n"), nil
					},
					"con down": func(args []string) ([]byte, error) {
						mu.Lock()
						stopped = true
						mu.Unlock()
						return []byte("ok"), nil
					},
				},
			}

			hm := NewWithRunner(slog.Default(), runner).Hotspot()
			hm.checkStep(context.Background())
			time.Sleep(30 * time.Millisecond)

			mu.Lock()
			defer mu.Unlock()
			if stopped {
				t.Fatal("hotspot was stopped while it was the only link")
			}
			if !hm.active {
				t.Fatal("expected hotspot to stay active")
			}
		})
	}
}

func TestHotspotStopsWhenEthernetConnects(t *testing.T) {
	var stopped bool
	var mu sync.Mutex
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				return []byte("wlan0:wifi:connected:Hotspot\neth0:ethernet:connected:Wired connection 1\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("IP4.ADDRESS[1]:10.42.0.1/24\n"), nil
			},
			"dev show eth0": func(args []string) ([]byte, error) {
				return []byte("IP4.ADDRESS[1]:192.168.1.20/24\nIP4.GATEWAY:192.168.1.1\n"), nil
			},
			"con show --active": func(args []string) ([]byte, error) {
				return []byte("Hotspot:802-11-wireless:wlan0\nWired connection 1:802-3-ethernet:eth0\n"), nil
			},
			"con down": func(args []string) ([]byte, error) {
				mu.Lock()
				stopped = true
				mu.Unlock()
				return []byte("ok"), nil
			},
		},
	}

	hm := NewWithRunner(slog.Default(), runner).Hotspot()
	hm.checkStep(context.Background())
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !stopped {
		t.Fatal("expected hotspot to stop once ethernet is connected")
	}
}

func TestHotspotRetriesAfterStartFailure(t *testing.T) {
	var calls int
	var mu sync.Mutex
	runner := &mockRunner{
		handlers: map[string]func(args []string) ([]byte, error){
			"dev status": func(args []string) ([]byte, error) {
				return []byte("wlan0:wifi:disconnected:\neth0:ethernet:unavailable:\n"), nil
			},
			"dev show wlan0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:wlan0\n"), nil
			},
			"dev show eth0": func(args []string) ([]byte, error) {
				return []byte("GENERAL.DEVICE:eth0\n"), nil
			},
			"wifi hotspot": func(args []string) ([]byte, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return []byte("Not authorized"), fmt.Errorf("exit status 4")
			},
			"con show --active": func(args []string) ([]byte, error) {
				return []byte(""), nil
			},
		},
	}

	cfg := DefaultHotspotConfig()
	cfg.GracePeriod = 20 * time.Millisecond
	hm := NewWithRunner(slog.Default(), runner).EnableHotspot(cfg)

	hm.checkStep(context.Background())
	time.Sleep(35 * time.Millisecond)
	hm.checkStep(context.Background())
	time.Sleep(30 * time.Millisecond)
	hm.checkStep(context.Background())
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("start attempts = %d, want at least 2 after a failure", calls)
	}
}
