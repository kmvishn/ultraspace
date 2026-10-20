// Package browser backs the Browser app. Pages load directly in the user's browser
// (inside an iframe); the server only checks whether a site allows being embedded
// and reads its title, so the app can show a useful page instead of a blank frame.
package browser

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"ultraspace/internal/httpx"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 ultraspace"

var errBlockedAddr = errors.New("address is not public")

type Prober struct {
	client *http.Client
	log    *slog.Logger
}

// ErrBlockedAddr is returned when a connection would go to a non-public address.
var ErrBlockedAddr = errBlockedAddr

// PublicDialer returns a dialer that only connects to public internet addresses. The
// check runs on the resolved address of every connection, so redirects and DNS tricks
// cannot reach the host itself, the LAN or the cloud metadata service.
func PublicDialer() *net.Dialer {
	return &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !IsPublic(ip) {
				return errBlockedAddr
			}
			return nil
		},
	}
}

func New(log *slog.Logger) *Prober {
	dialer := PublicDialer()
	dialer.Timeout = 5 * time.Second
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("unsupported redirect")
			}
			return nil
		},
	}
	return &Prober{client: client, log: log}
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can map to private IPv4
	netip.MustParsePrefix("2001:db8::/32"),
}

// IsPublic reports whether ip is a globally routable unicast address.
func IsPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func (p *Prober) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/browser/probe", require(http.HandlerFunc(p.handleProbe)))
}

// Result says whether a page can be shown in a frame on our origin.
type Result struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	Embeddable bool   `json:"embeddable"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (p *Prober) handleProbe(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		httpx.Error(w, http.StatusBadRequest, "url must be an http or https address")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p.Probe(r.Context(), u, r.Header.Get("Accept-Language")))
}

func (p *Prober) Probe(ctx context.Context, u *url.URL, lang string) Result {
	res := Result{URL: u.String(), Embeddable: true}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	if lang != "" && len(lang) < 200 {
		req.Header.Set("Accept-Language", lang)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// The page may still load in the user's browser (for example a site on their own network).
		if errors.Is(err, errBlockedAddr) {
			res.Error = "private address; not checked"
		} else {
			res.Error = shortError(err)
		}
		return res
	}
	defer resp.Body.Close()
	res.URL = resp.Request.URL.String()
	if resp.Request.URL.Scheme != "https" {
		res.Embeddable, res.Reason = false, "insecure"
	} else if ok, why := frameAllowed(resp.Header); !ok {
		res.Embeddable, res.Reason = false, why
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct == "text/html" || ct == "application/xhtml+xml" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		res.Title = pageTitle(body)
	}
	return res
}

// frameAllowed applies the browser's rules: CSP frame-ancestors wins over X-Frame-Options.
func frameAllowed(h http.Header) (bool, string) {
	for _, policy := range h.Values("Content-Security-Policy") {
		for _, dir := range strings.Split(policy, ";") {
			fields := strings.Fields(strings.ToLower(dir))
			if len(fields) == 0 || fields[0] != "frame-ancestors" {
				continue
			}
			for _, src := range fields[1:] {
				if src == "*" || src == "https:" {
					return true, ""
				}
			}
			return false, "blocked"
		}
	}
	// Browsers ignore values other than DENY and SAMEORIGIN.
	if xfo := strings.ToLower(h.Get("X-Frame-Options")); strings.Contains(xfo, "deny") || strings.Contains(xfo, "sameorigin") {
		return false, "blocked"
	}
	return true, ""
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func pageTitle(body []byte) string {
	m := titleRE.FindSubmatch(body)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	if r := []rune(t); len(r) > 200 {
		t = string(r[:200])
	}
	return t
}

func shortError(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return "site not found"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "Client.Timeout"):
		return "timed out"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprint(urlErr.Err)
	}
	return err.Error()
}
