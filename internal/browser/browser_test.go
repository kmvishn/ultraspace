package browser

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.5": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := IsPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestFrameAllowed(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"no headers", http.Header{}, true},
		{"xfo deny", http.Header{"X-Frame-Options": {"DENY"}}, false},
		{"xfo sameorigin", http.Header{"X-Frame-Options": {"SAMEORIGIN"}}, false},
		{"xfo junk is ignored", http.Header{"X-Frame-Options": {"ALLOWALL"}}, true},
		{"csp none", http.Header{"Content-Security-Policy": {"default-src *; frame-ancestors 'none'"}}, false},
		{"csp self", http.Header{"Content-Security-Policy": {"frame-ancestors 'self'"}}, false},
		{"csp star wins over xfo", http.Header{"Content-Security-Policy": {"frame-ancestors *"}, "X-Frame-Options": {"DENY"}}, true},
		{"csp without frame-ancestors", http.Header{"Content-Security-Policy": {"script-src 'self'"}}, true},
	}
	for _, c := range cases {
		if got, _ := frameAllowed(c.h); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPageTitle(t *testing.T) {
	if got := pageTitle([]byte("<html><head><TITLE>\n  Go &amp; you  </TITLE>")); got != "Go & you" {
		t.Fatalf("title = %q", got)
	}
}
