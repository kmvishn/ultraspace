package network

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestVisibleNetworksSkipsSetupSSID(t *testing.T) {
	n := NewWithRunner(slog.Default(), &mockRunner{})
	got := visibleNetworks(n, []WiFiAccessPoint{
		{SSID: "Ultraspace-Setup", Signal: 100},
		{SSID: "Home", Signal: 80, Security: "WPA2"},
		{SSID: ""},
		{SSID: "Home", Signal: 40},
	})
	if len(got) != 1 || got[0].SSID != "Home" {
		t.Fatalf("networks = %+v", got)
	}
}

func TestPortalPage(t *testing.T) {
	n := NewWithRunner(slog.Default(), &mockRunner{})
	h := portalHandler(n)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hotspot-detect.html", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Ultraspace-Setup") {
		t.Fatalf("page = %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "Network name") {
		t.Fatal("manual network name field missing")
	}
	if !strings.Contains(w.Body.String(), "Open Ultraspace") || !strings.Contains(w.Body.String(), "http://10.42.0.1:4600/") {
		t.Fatal("desktop link missing")
	}
}

func TestPortalLinkUsesDesktopPort(t *testing.T) {
	n := NewWithRunner(slog.Default(), &mockRunner{})
	n.PublishDesktop(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "127.0.0.1:4601")
	body := string(portalHTML(n, "10.9.9.9"))
	if !strings.Contains(body, "http://10.9.9.9:4601/") {
		t.Fatalf("link = %s", body)
	}
}

func TestDesktopReachableOnSetupWiFi(t *testing.T) {
	if desktopAlreadyReachable("127.0.0.1:4600", "10.42.0.1") {
		t.Fatal("loopback desktop is not reachable from a phone")
	}
	if desktopAlreadyReachable("[::1]:4600", "10.42.0.1") {
		t.Fatal("ipv6 loopback desktop is not reachable from a phone")
	}
	if !desktopAlreadyReachable("0.0.0.0:4600", "10.42.0.1") {
		t.Fatal("a desktop on every address is already reachable")
	}
	if !desktopAlreadyReachable("[::]:4600", "10.42.0.1") {
		t.Fatal("a desktop on every ipv6 address is already reachable")
	}
	if !desktopAlreadyReachable("10.42.0.1:4600", "10.42.0.1") {
		t.Fatal("a desktop already on the setup address is reachable")
	}
}

func TestSetupPageKeepsNetworksSeenBeforeHotspot(t *testing.T) {
	n := NewWithRunner(slog.Default(), &mockRunner{})
	n.hotspot.seen = []WiFiAccessPoint{
		{SSID: "Home", Signal: 70, Security: "WPA2"},
		{SSID: "Ultraspace-Setup", Signal: 100},
	}
	h := portalHandler(n)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/networks", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Home") || strings.Contains(body, "Ultraspace-Setup") {
		t.Fatalf("networks = %d %s", w.Code, body)
	}
}

func TestTypedNetworkIsJoinedAfterHotspotDrops(t *testing.T) {
	prev := setupClientWait
	setupClientWait = 0
	t.Cleanup(func() { setupClientWait = prev })

	var mu sync.Mutex
	hotspotUp := true
	connects := 0
	stopped := false
	restarted := false
	n := NewWithRunner(slog.Default(), &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"wifi connect": func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			connects++
			if hotspotUp {
				return []byte("Error: No network with SSID 'Home' found."), fmt.Errorf("exit 10")
			}
			return []byte("Connection successfully activated"), nil
		},
		"con down": func(args []string) ([]byte, error) {
			mu.Lock()
			hotspotUp = false
			stopped = true
			mu.Unlock()
			return []byte("ok"), nil
		},
		"con show --active": func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if hotspotUp {
				return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
			}
			return []byte(""), nil
		},
		"dev show wlan0": func(args []string) ([]byte, error) {
			return []byte("IP4.ADDRESS[1]:10.42.0.1/24\n"), nil
		},
		"wifi rescan": func(args []string) ([]byte, error) { return nil, nil },
		"wifi list": func(args []string) ([]byte, error) {
			return []byte(" :AA\\:BB\\:CC\\:DD\\:EE\\:FF:Home:6:2437 MHz:80:WPA2\n"), nil
		},
		"wifi hotspot": func(args []string) ([]byte, error) {
			mu.Lock()
			restarted = true
			mu.Unlock()
			return []byte("ok"), nil
		},
	}})

	if err := n.connectFromSetup(context.Background(), ConnectRequest{SSID: "Home", Password: "secret", Hidden: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !stopped || connects < 2 || restarted {
		t.Fatalf("stopped=%v connects=%d restarted=%v", stopped, connects, restarted)
	}
}

func TestWrongPasswordKeepsHotspotUp(t *testing.T) {
	prev := setupClientWait
	setupClientWait = 0
	t.Cleanup(func() { setupClientWait = prev })

	stopped := false
	n := NewWithRunner(slog.Default(), &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"wifi connect": func(args []string) ([]byte, error) {
			return []byte("Secrets were required, but not provided"), fmt.Errorf("exit 4")
		},
		"con show --active": func(args []string) ([]byte, error) {
			return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
		},
		"dev show wlan0": func(args []string) ([]byte, error) {
			return []byte("IP4.ADDRESS[1]:10.42.0.1/24\n"), nil
		},
		"con down": func(args []string) ([]byte, error) {
			stopped = true
			return []byte("ok"), nil
		},
	}})
	err := n.connectFromSetup(context.Background(), ConnectRequest{SSID: "Home", Password: "nope"})
	if err == nil || !strings.Contains(err.Error(), "incorrect") || stopped {
		t.Fatalf("err=%v stopped=%v", err, stopped)
	}
}

func TestFailedJoinBringsSetupHotspotBack(t *testing.T) {
	prev := setupClientWait
	setupClientWait = 0
	t.Cleanup(func() { setupClientWait = prev })

	var mu sync.Mutex
	hotspotUp := true
	restarted := false
	n := NewWithRunner(slog.Default(), &mockRunner{handlers: map[string]func([]string) ([]byte, error){
		"wifi connect": func(args []string) ([]byte, error) {
			mu.Lock()
			up := hotspotUp
			mu.Unlock()
			if up {
				return []byte("Error: No network with SSID 'Home' found."), fmt.Errorf("exit 10")
			}
			return []byte("Secrets were required, but not provided"), fmt.Errorf("exit 4")
		},
		"con down": func(args []string) ([]byte, error) {
			mu.Lock()
			hotspotUp = false
			mu.Unlock()
			return []byte("ok"), nil
		},
		"con show --active": func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if hotspotUp {
				return []byte("Hotspot:802-11-wireless:wlan0\n"), nil
			}
			return []byte(""), nil
		},
		"dev show wlan0": func(args []string) ([]byte, error) {
			return []byte("IP4.ADDRESS[1]:10.42.0.1/24\n"), nil
		},
		"wifi rescan": func(args []string) ([]byte, error) { return nil, nil },
		"wifi list":   func(args []string) ([]byte, error) { return []byte(""), nil },
		"dev status": func(args []string) ([]byte, error) {
			return []byte("wlan0:wifi:disconnected\n"), nil
		},
		"wifi hotspot": func(args []string) ([]byte, error) {
			mu.Lock()
			restarted = true
			mu.Unlock()
			return []byte("ok"), nil
		},
	}})
	err := n.connectFromSetup(context.Background(), ConnectRequest{SSID: "Home", Password: "nope", Hidden: true})
	if err == nil || !strings.Contains(err.Error(), "incorrect") {
		t.Fatalf("err=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !restarted {
		t.Fatal("setup hotspot was not brought back")
	}
}
