package remote

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"ultraspace/internal/browser"
)

// egressProxy is the only way the remote browser reaches the network. It forwards
// to public internet addresses and refuses everything else, so a web page cannot use
// the browser to talk to services on this host, the LAN or the cloud metadata API.
type egressProxy struct {
	ln        net.Listener
	srv       *http.Server
	dialer    *net.Dialer
	transport *http.Transport
	log       *slog.Logger
}

func startProxy(log *slog.Logger) (*egressProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	d := browser.PublicDialer()
	p := &egressProxy{
		ln: ln, dialer: d, log: log,
		transport: &http.Transport{
			Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: false,
			MaxIdleConns: 32, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 60 * time.Second,
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *egressProxy) Addr() string { return p.ln.Addr().String() }

func (p *egressProxy) Close() {
	_ = p.srv.Close()
	p.transport.CloseIdleConnections()
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "only proxy requests are accepted", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Keep-Alive", "Te", "Trailer"} {
		out.Header.Del(h)
	}
	upgrade := strings.EqualFold(r.Header.Get("Connection"), "upgrade") || r.Header.Get("Upgrade") != ""
	if !upgrade {
		out.Header.Del("Connection")
		out.Header.Del("Upgrade")
	}
	res, err := p.transport.RoundTrip(out)
	if err != nil {
		p.refuse(w, err)
		return
	}
	defer res.Body.Close()
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if res.StatusCode == http.StatusSwitchingProtocols {
		// Plain ws:// through the proxy: relay the upgraded connection both ways.
		backend, ok := res.Body.(io.ReadWriteCloser)
		hj, ok2 := w.(http.Hijacker)
		if !ok || !ok2 {
			http.Error(w, "upgrade not supported", http.StatusBadGateway)
			return
		}
		w.WriteHeader(res.StatusCode)
		client, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = buf.Flush()
		pipe(client, backend)
		return
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

func (p *egressProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	backend, err := p.dialer.DialContext(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		p.refuse(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		backend.Close()
		http.Error(w, "tunnel not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		backend.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		_, _ = backend.Write(b)
	}
	pipe(client, backend)
}

func (p *egressProxy) refuse(w http.ResponseWriter, err error) {
	if errors.Is(err, browser.ErrBlockedAddr) {
		http.Error(w, "ultraspace: the remote browser may only reach public internet addresses", http.StatusForbidden)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
