package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"ultraspace/internal/httpx"
)

// Client is the ultraspace side: authenticated routes that proxy to the agent socket.
type Client struct {
	sock   string
	proxy  httpx.Proxy
	public string
	rp     *httputil.ReverseProxy
	http   *http.Client
}

func NewClient(sock string, proxy httpx.Proxy, publicOrigin string) *Client {
	c := &Client{sock: sock, proxy: proxy, public: publicOrigin}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 16, IdleConnTimeout: 60 * time.Second,
	}
	target, _ := url.Parse("http://agent")
	c.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/api")
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Cookie") // the agent never needs the ultraspace session
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			if strings.HasPrefix(res.Request.URL.Path, "/code/") {
				// VS Code is shown in a window of the desktop, which is the same site.
				res.Header.Set("X-Frame-Options", "SAMEORIGIN")
				res.Header.Add("Content-Security-Policy", "frame-ancestors 'self'")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
				httpx.Error(w, http.StatusServiceUnavailable, "the terminal service (ultraspace-agent) is not installed or not running")
				return
			}
			httpx.Error(w, http.StatusBadGateway, "terminal service error: "+err.Error())
		},
	}
	c.http = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	return c
}

func (c *Client) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/agent/status", require(http.HandlerFunc(c.status)))
	mux.Handle("/api/term/", require(c.sameOrigin(c.rp)))
	mux.Handle("/code/", require(c.sameOrigin(c.rp)))
}

func (c *Client) status(w http.ResponseWriter, r *http.Request) {
	res, err := c.http.Get("http://agent/status")
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	defer res.Body.Close()
	var out map[string]any
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		out = map[string]any{}
	}
	out["available"] = true
	httpx.WriteJSON(w, http.StatusOK, out)
}

// sameOrigin refuses requests (sockets included) that another site started. The
// session cookie is SameSite=Strict too, so this is a second lock on the same door.
func (c *Client) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			httpx.Error(w, http.StatusForbidden, "cross-site request rejected")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != c.proxy.Origin(r) && (c.public == "" || o != c.public) {
			httpx.Error(w, http.StatusForbidden, "origin not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}
