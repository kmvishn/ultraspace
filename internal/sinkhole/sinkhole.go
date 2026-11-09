// Package sinkhole implements an optional, lightweight network-wide DNS sinkhole
// (ad and tracker blocker) that blocks queries for known advertising and telemetry domains.
package sinkhole

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"ultraspace/internal/httpx"
)

// DefaultUpstreams are fast, privacy-respecting DNS resolvers used for non-blocked queries.
var DefaultUpstreams = []string{"1.1.1.1:53", "1.0.0.1:53"}

// BuiltinBlocklist contains widely-blocked advertising, telemetry, and tracking root domains.
var BuiltinBlocklist = []string{
	"doubleclick.net",
	"googleadservices.com",
	"googlesyndication.com",
	"adnxs.com",
	"ads.yahoo.com",
	"adservice.google.com",
	"pagead2.googlesyndication.com",
	"adform.net",
	"taboola.com",
	"outbrain.com",
	"criteo.com",
	"criteo.net",
	"rubiconproject.com",
	"pubmatic.com",
	"scorecardresearch.com",
	"quantserve.com",
	"advertising.com",
	"adcolony.com",
	"unityads.unity3d.com",
	"applovin.com",
	"flurry.com",
	"chartbeat.com",
	"analytics.tiktok.com",
	"ads.tiktok.com",
	"telemetry.microsoft.com",
	"v10.events.data.microsoft.com",
	"v20.events.data.microsoft.com",
	"activity.windows.com",
	"browser.pipe.aria.microsoft.com",
	"metrics.mzstatic.com",
	"analytics.yahoo.com",
	"smartadserver.com",
	"openx.net",
	"amazon-adsystem.com",
	"adpushup.com",
	"adroll.com",
	"bidswitch.net",
	"casalemedia.com",
	"revcontent.com",
	"mgid.com",
	"popads.net",
	"popcash.net",
	"propellerads.com",
	"exoclick.com",
	"trafficjunky.com",
	"zergnet.com",
}

// QueryLog records a single DNS query processed by the sinkhole.
type QueryLog struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Domain   string    `json:"domain"`
	Type     string    `json:"type"`
	Blocked  bool      `json:"blocked"`
	Duration int64     `json:"duration_ms"`
}

// Stats represents DNS processing counters.
type Stats struct {
	TotalQueries   uint64 `json:"total_queries"`
	BlockedQueries uint64 `json:"blocked_queries"`
	AllowedQueries uint64 `json:"allowed_queries"`
}

// Config holds persistent sinkhole settings.
type Config struct {
	Enabled   bool     `json:"enabled"`
	Port      int      `json:"port"`
	Upstreams []string `json:"upstreams"`
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
}

// Server is the DNS sinkhole manager.
type Server struct {
	mu           sync.RWMutex
	log          *slog.Logger
	dataDir      string
	configFile   string
	enabled      bool
	port         int
	upstreams    []string
	rules        map[string]struct{}
	whitelist    map[string]struct{}
	blacklist    map[string]struct{}
	totalQueries atomic.Uint64
	blockedQ     atomic.Uint64
	allowedQ     atomic.Uint64
	recent       []QueryLog
	maxRecent    int
	udpConn      net.PacketConn
	cancelFunc   context.CancelFunc
	topBlocked   map[string]int
	topAllowed   map[string]int
}

// New creates a new DNS sinkhole instance.
func New(dataDir string, log *slog.Logger) *Server {
	s := &Server{
		log:        log.With("component", "sinkhole"),
		dataDir:    dataDir,
		configFile: filepath.Join(dataDir, "sinkhole.json"),
		port:       53,
		upstreams:  DefaultUpstreams,
		rules:      make(map[string]struct{}),
		whitelist:  make(map[string]struct{}),
		blacklist:  make(map[string]struct{}),
		maxRecent:  100,
		recent:     make([]QueryLog, 0, 100),
		topBlocked: make(map[string]int),
		topAllowed: make(map[string]int),
	}

	// Populate built-in blocklist
	for _, d := range BuiltinBlocklist {
		s.rules[normalizeDomain(d)] = struct{}{}
	}

	// Load user config if exists
	s.loadConfig()

	return s
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	return d
}

// loadConfig loads saved whitelist, blacklist, and enabled status.
func (s *Server) loadConfig() {
	b, err := os.ReadFile(s.configFile)
	if err != nil {
		return
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return
	}
	s.enabled = cfg.Enabled
	if cfg.Port > 0 {
		s.port = cfg.Port
	}
	if len(cfg.Upstreams) > 0 {
		s.upstreams = cfg.Upstreams
	}
	for _, d := range cfg.Whitelist {
		s.whitelist[normalizeDomain(d)] = struct{}{}
	}
	for _, d := range cfg.Blacklist {
		s.blacklist[normalizeDomain(d)] = struct{}{}
	}
}

// saveConfig saves current configuration to disk.
func (s *Server) saveConfig() error {
	if s.dataDir == "" {
		return nil
	}
	_ = os.MkdirAll(s.dataDir, 0o750)

	cfg := Config{
		Enabled:   s.enabled,
		Port:      s.port,
		Upstreams: s.upstreams,
	}
	for d := range s.whitelist {
		cfg.Whitelist = append(cfg.Whitelist, d)
	}
	for d := range s.blacklist {
		cfg.Blacklist = append(cfg.Blacklist, d)
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.configFile, b, 0o640)
}

// Start launches the DNS UDP listener if enabled.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		return nil
	}
	return s.startListenerLocked(ctx)
}

func (s *Server) startListenerLocked(ctx context.Context) error {
	if s.udpConn != nil {
		return nil
	}

	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.log.Warn("cannot bind DNS sinkhole port", "addr", addr, "err", err)
		return err
	}

	s.udpConn = conn
	srvCtx, cancel := context.WithCancel(ctx)
	s.cancelFunc = cancel

	go s.serveUDP(srvCtx, conn)
	s.log.Info("DNS sinkhole running", "addr", addr, "rules", len(s.rules)+len(s.blacklist))
	return nil
}

// Stop shuts down the DNS listener.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopListenerLocked()
}

func (s *Server) stopListenerLocked() {
	if s.cancelFunc != nil {
		s.cancelFunc()
		s.cancelFunc = nil
	}
	if s.udpConn != nil {
		_ = s.udpConn.Close()
		s.udpConn = nil
	}
	s.log.Info("DNS sinkhole stopped")
}

// IsBlocked checks if a given domain or any of its parent domains is blocked.
func (s *Server) IsBlocked(domain string) bool {
	d := normalizeDomain(domain)
	if d == "" {
		return false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 1. Whitelist always takes precedence
	if _, ok := s.whitelist[d]; ok {
		return false
	}

	// 2. Custom blacklist
	if _, ok := s.blacklist[d]; ok {
		return true
	}

	// 3. Built-in rules: check exact and parent domains
	parts := strings.Split(d, ".")
	for i := 0; i < len(parts); i++ {
		candidate := strings.Join(parts[i:], ".")
		if _, ok := s.rules[candidate]; ok {
			return true
		}
		if _, ok := s.blacklist[candidate]; ok {
			return true
		}
	}

	return false
}

// serveUDP listens for incoming DNS packets and dispatches them.
func (s *Server) serveUDP(ctx context.Context, conn net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, clientAddr, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}

		reqBytes := make([]byte, n)
		copy(reqBytes, buf[:n])

		go s.handlePacket(conn, clientAddr, reqBytes)
	}
}

// handlePacket parses, filters, and responds to or forwards a DNS query.
func (s *Server) handlePacket(conn net.PacketConn, clientAddr net.Addr, reqBytes []byte) {
	start := time.Now()
	var msg dnsmessage.Message
	if err := msg.Unpack(reqBytes); err != nil || len(msg.Questions) == 0 {
		return
	}

	q := msg.Questions[0]
	qName := q.Name.String()
	cleanName := normalizeDomain(qName)
	clientIP, _, _ := net.SplitHostPort(clientAddr.String())
	if clientIP == "" {
		clientIP = clientAddr.String()
	}

	qType := q.Type.String()
	blocked := s.IsBlocked(cleanName)

	if blocked {
		s.blockedQ.Add(1)
		s.totalQueries.Add(1)
		s.recordQuery(QueryLog{
			Time:     time.Now(),
			Client:   clientIP,
			Domain:   cleanName,
			Type:     qType,
			Blocked:  true,
			Duration: time.Since(start).Milliseconds(),
		})

		// Return 0.0.0.0 (A) or :: (AAAA)
		respBytes := s.buildBlockedResponse(&msg, q)
		if respBytes != nil {
			_, _ = conn.WriteTo(respBytes, clientAddr)
		}
		return
	}

	// Forward to upstream
	s.allowedQ.Add(1)
	s.totalQueries.Add(1)

	resp, err := s.forwardUpstream(reqBytes)
	duration := time.Since(start).Milliseconds()

	s.recordQuery(QueryLog{
		Time:     time.Now(),
		Client:   clientIP,
		Domain:   cleanName,
		Type:     qType,
		Blocked:  false,
		Duration: duration,
	})

	if err == nil && resp != nil {
		_, _ = conn.WriteTo(resp, clientAddr)
	}
}

func (s *Server) buildBlockedResponse(req *dnsmessage.Message, q dnsmessage.Question) []byte {
	var resp dnsmessage.Message
	resp.Header = dnsmessage.Header{
		ID:                 req.Header.ID,
		Response:           true,
		OpCode:             req.Header.OpCode,
		Authoritative:      true,
		RecursionDesired:   req.Header.RecursionDesired,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeSuccess,
	}
	resp.Questions = []dnsmessage.Question{q}

	if q.Type == dnsmessage.TypeA {
		resp.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   300,
				},
				Body: &dnsmessage.AResource{A: [4]byte{0, 0, 0, 0}},
			},
		}
	} else if q.Type == dnsmessage.TypeAAAA {
		resp.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeAAAA,
					Class: dnsmessage.ClassINET,
					TTL:   300,
				},
				Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
			},
		}
	}

	out, err := resp.Pack()
	if err != nil {
		return nil
	}
	return out
}

func (s *Server) forwardUpstream(req []byte) ([]byte, error) {
	s.mu.RLock()
	upstreams := s.upstreams
	s.mu.RUnlock()

	for _, upstream := range upstreams {
		c, err := net.DialTimeout("udp", upstream, 2*time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = c.Write(req)
		if err != nil {
			_ = c.Close()
			continue
		}
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		_ = c.Close()
		if err == nil && n > 0 {
			return buf[:n], nil
		}
	}

	return nil, errors.New("upstreams unreachable")
}

func (s *Server) recordQuery(q QueryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.recent) >= s.maxRecent {
		s.recent = s.recent[1:]
	}
	s.recent = append(s.recent, q)

	if q.Blocked {
		s.topBlocked[q.Domain]++
	} else {
		s.topAllowed[q.Domain]++
	}
}

// AddRule adds an in-memory rule.
func (s *Server) AddRule(domain string) {
	d := normalizeDomain(domain)
	if d == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[d] = struct{}{}
}

// LoadHostsFile parses standard /etc/hosts format lines into block rules.
func (s *Server) LoadHostsFile(r io.Reader) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	added := 0
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			ip := fields[0]
			if ip == "127.0.0.1" || ip == "0.0.0.0" {
				for _, host := range fields[1:] {
					host = normalizeDomain(host)
					if host != "" && host != "localhost" && host != "broadcasthost" {
						s.rules[host] = struct{}{}
						added++
					}
				}
			}
		}
	}
	return added
}

// Register wires HTTP endpoints for sinkhole management into the router.
func (s *Server) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/sinkhole/status", auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("POST /api/sinkhole/toggle", auth(http.HandlerFunc(s.handleToggle)))
	mux.Handle("POST /api/sinkhole/rules", auth(http.HandlerFunc(s.handleRules)))
	mux.Handle("GET /api/sinkhole/queries", auth(http.HandlerFunc(s.handleQueries)))
	mux.Handle("POST /api/sinkhole/update", auth(http.HandlerFunc(s.handleUpdateBlocklist)))
}

type statusResponse struct {
	Enabled        bool       `json:"enabled"`
	Listening      bool       `json:"listening"`
	Port           int        `json:"port"`
	Upstreams      []string   `json:"upstreams"`
	RuleCount      int        `json:"rule_count"`
	WhitelistCount int        `json:"whitelist_count"`
	BlacklistCount int        `json:"blacklist_count"`
	Whitelist      []string   `json:"whitelist"`
	Blacklist      []string   `json:"blacklist"`
	Stats          Stats      `json:"stats"`
	PercentBlocked float64    `json:"percent_blocked"`
	TopBlocked     []nameFreq `json:"top_blocked"`
	TopAllowed     []nameFreq `json:"top_allowed"`
}

type nameFreq struct {
	Domain string `json:"domain"`
	Count  int    `json:"count"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := s.totalQueries.Load()
	blocked := s.blockedQ.Load()
	allowed := s.allowedQ.Load()

	var pct float64
	if total > 0 {
		pct = float64(blocked) * 100.0 / float64(total)
	}

	wl := make([]string, 0, len(s.whitelist))
	for d := range s.whitelist {
		wl = append(wl, d)
	}
	bl := make([]string, 0, len(s.blacklist))
	for d := range s.blacklist {
		bl = append(bl, d)
	}

	resp := statusResponse{
		Enabled:        s.enabled,
		Listening:      s.udpConn != nil,
		Port:           s.port,
		Upstreams:      s.upstreams,
		RuleCount:      len(s.rules),
		WhitelistCount: len(s.whitelist),
		BlacklistCount: len(s.blacklist),
		Whitelist:      wl,
		Blacklist:      bl,
		Stats: Stats{
			TotalQueries:   total,
			BlockedQueries: blocked,
			AllowedQueries: allowed,
		},
		PercentBlocked: pct,
		TopBlocked:     getTopDomains(s.topBlocked, 5),
		TopAllowed:     getTopDomains(s.topAllowed, 5),
	}

	httpx.WriteJSON(w, http.StatusOK, resp)
}

func getTopDomains(m map[string]int, limit int) []nameFreq {
	out := make([]nameFreq, 0, len(m))
	for d, c := range m {
		out = append(out, nameFreq{Domain: d, Count: c})
	}
	// Sort descending
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Count > out[i].Count {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	s.mu.Lock()
	s.enabled = req.Enabled
	var err error
	if s.enabled {
		err = s.startListenerLocked(r.Context())
	} else {
		s.stopListenerLocked()
	}
	_ = s.saveConfig()
	s.mu.Unlock()

	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"enabled": s.enabled,
			"error":   fmt.Sprintf("Could not bind port %d: %v", s.port, err),
		})
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": s.enabled,
	})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"` // "add_whitelist", "remove_whitelist", "add_blacklist", "remove_blacklist"
		Domain string `json:"domain"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	domain := normalizeDomain(req.Domain)
	if domain == "" {
		httpx.Error(w, http.StatusBadRequest, "domain is required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch req.Action {
	case "add_whitelist":
		delete(s.blacklist, domain)
		s.whitelist[domain] = struct{}{}
	case "remove_whitelist":
		delete(s.whitelist, domain)
	case "add_blacklist":
		delete(s.whitelist, domain)
		s.blacklist[domain] = struct{}{}
	case "remove_blacklist":
		delete(s.blacklist, domain)
	default:
		httpx.Error(w, http.StatusBadRequest, "unknown action")
		return
	}

	_ = s.saveConfig()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Return queries in reverse chronological order
	out := make([]QueryLog, 0, len(s.recent))
	for i := len(s.recent) - 1; i >= 0; i-- {
		out = append(out, s.recent[i])
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"queries": out,
	})
}

func (s *Server) handleUpdateBlocklist(w http.ResponseWriter, r *http.Request) {
	// Download standard StevenBlack hosts list asynchronously
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts", nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return
		}
		defer resp.Body.Close()

		added := s.LoadHostsFile(resp.Body)
		s.log.Info("updated blocklist from StevenBlack hosts", "added_rules", added)
	}()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Blocklist update initiated in background",
	})
}
