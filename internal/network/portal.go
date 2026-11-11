package network

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/httpx"
)

// setupClientWait is how long the radio is given to leave hotspot mode before
// a client connect is attempted. Tests set this to zero.
var setupClientWait = time.Second

// portal is the sign-in page phones see after joining Ultraspace-Setup.
// It listens only on the hotspot address. When the desktop itself is only on
// loopback, the same address also serves it so a phone can open Ultraspace.
type portal struct {
	mu     sync.Mutex
	srv    *http.Server
	appSrv *http.Server
	ip     string
}

func (p *portal) start(n *Network, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srv != nil && p.ip == ip {
		return
	}
	if p.srv != nil {
		p.shutdownLocked()
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "80"))
	if err != nil {
		n.log.Warn("setup page unavailable", "ip", ip, "err", err)
		return
	}
	srv := &http.Server{
		Handler:           portalHandler(n),
		ReadHeaderTimeout: 5 * time.Second,
	}
	p.srv = srv
	p.ip = ip
	n.log.Info("setup page listening", "addr", ln.Addr().String())
	go func() { _ = srv.Serve(ln) }()
	p.publishDesktopLocked(n, ip)
}

func (p *portal) stop() {
	p.mu.Lock()
	p.shutdownLocked()
	p.ip = ""
	p.mu.Unlock()
}

func (p *portal) shutdownLocked() {
	srvs := []*http.Server{p.srv, p.appSrv}
	p.srv = nil
	p.appSrv = nil
	for _, srv := range srvs {
		if srv == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
}

// publishDesktopLocked serves Ultraspace on the setup address when the app
// would otherwise be reachable only from this machine.
func (p *portal) publishDesktopLocked(n *Network, ip string) {
	if n.desktop == nil || desktopAlreadyReachable(n.desktopListen, ip) {
		return
	}
	port := desktopPort(n.desktopListen)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
	if err != nil {
		n.log.Warn("ultraspace on setup wifi unavailable", "ip", ip, "err", err)
		return
	}
	srv := &http.Server{
		Handler:           n.desktop,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	p.appSrv = srv
	n.log.Info("ultraspace on setup wifi", "addr", ln.Addr().String())
	go func() { _ = srv.Serve(ln) }()
}

func desktopPort(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "4600"
	}
	return port
}

// desktopAlreadyReachable reports whether a phone on the setup Wi-Fi can open
// the app without a second listener. A loopback listener cannot be reached.
func desktopAlreadyReachable(listen, hotspotIP string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::", hotspotIP:
		return true
	default:
		return false
	}
}

func portalHTML(n *Network, ip string) []byte {
	page := portalPage
	if net.ParseIP(ip) != nil {
		page = bytes.ReplaceAll(page, []byte("10.42.0.1"), []byte(ip))
	}
	port := "4600"
	if n != nil && n.desktopListen != "" {
		port = desktopPort(n.desktopListen)
	}
	if port != "4600" {
		page = bytes.ReplaceAll(page, []byte(":4600"), []byte(":"+port))
	}
	return page
}

func (h *HotspotManager) ensurePortal(ctx context.Context) {
	st := h.Status(ctx)
	if !st.Active {
		return
	}
	ip, _, _ := strings.Cut(strings.TrimSpace(st.IP), "/")
	ip = strings.TrimSpace(ip)
	if net.ParseIP(ip) == nil {
		return
	}
	h.portal.start(h.net, ip)
}

func portalHandler(n *Network) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/networks", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		// Do not rescan here. A rescan while the radio is the hotspot often drops the page.
		var visible []WiFiAccessPoint
		if aps, err := n.ScanWiFi(ctx, false); err != nil {
			n.log.Warn("setup scan", "err", err)
		} else {
			visible = visibleNetworks(n, aps)
		}
		if len(visible) == 0 && n.hotspot != nil {
			visible = visibleNetworks(n, n.hotspot.seenNetworks())
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"networks": visible})
	})
	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		var req ConnectRequest
		if !httpx.DecodeJSON(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if err := n.connectFromSetup(ctx, req); err != nil {
			n.log.Warn("setup connect failed", "ssid", req.SSID, "err", err)
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{
			"status":  "connected",
			"message": "Connected. You can leave Ultraspace-Setup.",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		ip := ""
		if n.hotspot != nil {
			st := n.hotspot.Status(r.Context())
			ip, _, _ = strings.Cut(strings.TrimSpace(st.IP), "/")
			ip = strings.TrimSpace(ip)
		}
		_, _ = w.Write(portalHTML(n, ip))
	})
	return mux
}

func visibleNetworks(n *Network, aps []WiFiAccessPoint) []WiFiAccessPoint {
	own := ""
	if n.hotspot != nil {
		own = n.hotspot.cfg.SSID
	}
	out := make([]WiFiAccessPoint, 0, len(aps))
	seen := map[string]bool{}
	for _, ap := range aps {
		if ap.SSID == "" || ap.SSID == own || seen[ap.SSID] {
			continue
		}
		seen[ap.SSID] = true
		out = append(out, ap)
	}
	return out
}

// connectFromSetup joins a network from the setup page.
//
// One radio cannot be the hotspot and a client at the same time. A first attempt
// that says the network was not found usually means the radio could not see it
// while hosting Ultraspace-Setup, including a name typed by hand. The hotspot is
// then stopped and the join is tried again. A wrong password leaves the hotspot
// up when the radio could already see the network, and brings it back when the
// join was attempted with the radio free.
func (n *Network) connectFromSetup(ctx context.Context, req ConnectRequest) error {
	err := n.connectWiFi(ctx, req)
	if err == nil || n.hotspot == nil {
		return err
	}
	if st := n.hotspot.Status(ctx); !st.Active {
		return err
	}
	if isAuthFailure(err) {
		return err
	}
	if stopErr := n.hotspot.StopHotspot(ctx); stopErr != nil {
		return err
	}
	if err = n.connectAsClient(ctx, req); err != nil {
		_ = n.hotspot.StartHotspot(ctx)
		return err
	}
	return nil
}

// connectAsClient joins after the hotspot radio has been released.
func (n *Network) connectAsClient(ctx context.Context, req ConnectRequest) error {
	if err := waitSetup(ctx); err != nil {
		return err
	}
	_, _ = n.ScanWiFi(ctx, true)
	err := n.connectWiFi(ctx, req)
	if err == nil || isAuthFailure(err) || !strings.Contains(err.Error(), "not found") {
		return err
	}
	if err := waitSetup(ctx); err != nil {
		return err
	}
	_, _ = n.ScanWiFi(ctx, true)
	return n.connectWiFi(ctx, req)
}

func waitSetup(ctx context.Context) error {
	if setupClientWait <= 0 {
		return nil
	}
	timer := time.NewTimer(setupClientWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "incorrect") || strings.Contains(s, "SSID cannot") || strings.Contains(s, "invalid ")
}

func (h *HotspotManager) rememberNetworks(ctx context.Context) {
	aps, err := h.net.ScanWiFi(ctx, true)
	if err != nil || len(aps) == 0 {
		return
	}
	h.mu.Lock()
	h.seen = aps
	h.mu.Unlock()
}

func (h *HotspotManager) seenNetworks() []WiFiAccessPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]WiFiAccessPoint, len(h.seen))
	copy(out, h.seen)
	return out
}
