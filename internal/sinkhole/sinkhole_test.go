package sinkhole

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestIsBlocked(t *testing.T) {
	s := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Built-in ad domain
	if !s.IsBlocked("doubleclick.net") {
		t.Errorf("expected doubleclick.net to be blocked")
	}

	// Subdomain of ad domain
	if !s.IsBlocked("ad.pagead2.googlesyndication.com") {
		t.Errorf("expected ad.pagead2.googlesyndication.com to be blocked")
	}

	// Allowed clean domain
	if s.IsBlocked("github.com") {
		t.Errorf("expected github.com to be allowed")
	}
	if s.IsBlocked("wikipedia.org") {
		t.Errorf("expected wikipedia.org to be allowed")
	}

	// Custom blacklist
	s.mu.Lock()
	s.blacklist["annoying-popup.com"] = struct{}{}
	s.mu.Unlock()

	if !s.IsBlocked("annoying-popup.com") {
		t.Errorf("expected custom blacklisted domain to be blocked")
	}
	if !s.IsBlocked("sub.annoying-popup.com") {
		t.Errorf("expected subdomain of blacklisted domain to be blocked")
	}

	// Whitelist overrides built-in block
	s.mu.Lock()
	s.whitelist["doubleclick.net"] = struct{}{}
	s.mu.Unlock()

	if s.IsBlocked("doubleclick.net") {
		t.Errorf("expected whitelisted domain to NOT be blocked")
	}
}

func TestLoadHostsFile(t *testing.T) {
	s := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	hostsData := `
# Example hosts file
127.0.0.1 localhost
::1 localhost
0.0.0.0 malware-tracker.xyz
127.0.0.1 adserver.click
`

	added := s.LoadHostsFile(strings.NewReader(hostsData))
	if added != 2 {
		t.Errorf("expected 2 added rules, got %d", added)
	}

	if !s.IsBlocked("malware-tracker.xyz") {
		t.Errorf("expected malware-tracker.xyz to be blocked")
	}
	if !s.IsBlocked("adserver.click") {
		t.Errorf("expected adserver.click to be blocked")
	}
}

func TestBuildBlockedResponse(t *testing.T) {
	s := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A record query
	name, _ := dnsmessage.NewName("ad.doubleclick.net.")
	q := dnsmessage.Question{
		Name:  name,
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}
	req := &dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1234},
		Questions: []dnsmessage.Question{q},
	}

	respBytes := s.buildBlockedResponse(req, q)
	if respBytes == nil {
		t.Fatalf("expected blocked response bytes, got nil")
	}

	var respMsg dnsmessage.Message
	if err := respMsg.Unpack(respBytes); err != nil {
		t.Fatalf("unpack response: %v", err)
	}

	if respMsg.Header.ID != 1234 {
		t.Errorf("expected ID 1234, got %d", respMsg.Header.ID)
	}
	if len(respMsg.Answers) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(respMsg.Answers))
	}
	ans := respMsg.Answers[0]
	if aRes, ok := ans.Body.(*dnsmessage.AResource); !ok || aRes.A != [4]byte{0, 0, 0, 0} {
		t.Errorf("expected 0.0.0.0 A answer, got %+v", ans.Body)
	}
}

func TestHTTPHandlers(t *testing.T) {
	s := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.port = 25353 // test unprivileged port

	mux := http.NewServeMux()
	noopAuth := func(h http.Handler) http.Handler { return h }
	s.Register(mux, noopAuth)

	// 1. GET /api/sinkhole/status (initially disabled)
	{
		req := httptest.NewRequest("GET", "/api/sinkhole/status", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d", rec.Code)
		}
		var st statusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.Enabled != false {
			t.Errorf("expected enabled=false initially")
		}
		if st.RuleCount == 0 {
			t.Errorf("expected built-in rules > 0")
		}
	}

	// 2. POST /api/sinkhole/rules (add blacklist, add whitelist)
	{
		body := `{"action":"add_blacklist","domain":"spam.net"}`
		req := httptest.NewRequest("POST", "/api/sinkhole/rules", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("add_blacklist code = %d", rec.Code)
		}
		if !s.IsBlocked("spam.net") {
			t.Errorf("expected spam.net to be blocked")
		}

		bodyWl := `{"action":"add_whitelist","domain":"spam.net"}`
		reqWl := httptest.NewRequest("POST", "/api/sinkhole/rules", bytes.NewBufferString(bodyWl))
		recWl := httptest.NewRecorder()
		mux.ServeHTTP(recWl, reqWl)
		if recWl.Code != http.StatusOK {
			t.Fatalf("add_whitelist code = %d", recWl.Code)
		}
		if s.IsBlocked("spam.net") {
			t.Errorf("expected spam.net to be whitelisted now")
		}
	}

	// 3. POST /api/sinkhole/toggle (enable & disable)
	{
		body := `{"enabled":true}`
		req := httptest.NewRequest("POST", "/api/sinkhole/toggle", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("toggle enable code = %d", rec.Code)
		}

		// Disable
		bodyOff := `{"enabled":false}`
		reqOff := httptest.NewRequest("POST", "/api/sinkhole/toggle", bytes.NewBufferString(bodyOff))
		recOff := httptest.NewRecorder()
		mux.ServeHTTP(recOff, reqOff)
		if recOff.Code != http.StatusOK {
			t.Fatalf("toggle disable code = %d", recOff.Code)
		}
	}

	// 4. GET /api/sinkhole/queries
	{
		s.recordQuery(QueryLog{
			Client:  "192.168.1.50",
			Domain:  "doubleclick.net",
			Blocked: true,
		})

		req := httptest.NewRequest("GET", "/api/sinkhole/queries", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("queries code = %d", rec.Code)
		}
		var out struct {
			Queries []QueryLog `json:"queries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Queries) != 1 || out.Queries[0].Domain != "doubleclick.net" {
			t.Errorf("unexpected queries output: %+v", out)
		}
	}
}

func TestStartStop(t *testing.T) {
	s := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.port = 25354 // use unprivileged test port

	// Should not start if disabled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start disabled error: %v", err)
	}

	// Enable and start
	s.enabled = true
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start enabled error: %v", err)
	}

	s.Stop()
}

func TestAllowedClient(t *testing.T) {
	// Private / local addresses must be allowed
	allowed := []string{
		"127.0.0.1",
		"10.0.0.15",
		"172.16.5.2",
		"192.168.1.100",
		"169.254.1.1",
		"::1",
		"fd00::1",
		"fe80::1",
	}
	for _, ip := range allowed {
		if !isAllowedClient(ip) {
			t.Errorf("expected %s to be allowed local client", ip)
		}
	}

	// Public WAN addresses must be denied
	denied := []string{
		"8.8.8.8",
		"1.1.1.1",
		"140.245.194.129",
		"208.67.222.222",
		"2606:4700:4700::1111",
		"not-an-ip",
	}
	for _, ip := range denied {
		if isAllowedClient(ip) {
			t.Errorf("expected %s to be denied public client", ip)
		}
	}
}

