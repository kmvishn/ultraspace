package remote

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The remote browser's proxy must refuse anything that is not a public address.
func TestProxyRefusesPrivateAddresses(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer target.Close()
	p, err := startProxy(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	host := strings.TrimPrefix(target.URL, "http://")

	for _, req := range []string{
		"CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n",
		"GET " + target.URL + "/ HTTP/1.1\r\nHost: " + host + "\r\n\r\n",
		"CONNECT 169.254.169.254:80 HTTP/1.1\r\nHost: 169.254.169.254:80\r\n\r\n",
		"GET http://localhost:" + strings.Split(host, ":")[1] + "/ HTTP/1.1\r\nHost: localhost\r\n\r\n",
	} {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, req)
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		c.Close()
		if err != nil {
			t.Fatalf("%q: %v", req, err)
		}
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusForbidden || strings.Contains(string(body), "secret") {
			t.Errorf("%q: status %d body %q, want 403", strings.SplitN(req, "\r\n", 2)[0], res.StatusCode, body)
		}
	}
}
