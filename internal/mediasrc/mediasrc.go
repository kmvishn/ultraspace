// Package mediasrc lets the video player read media that is not a plain file in Files:
// a file inside a torrent (playable while it downloads) or a stream from a Stremio addon.
//
// A source is named by a string such as "torrent:<infohash>:<file>" or
// "url:https://…". The browser reads it through /api/media/raw (with Range support);
// ffmpeg reads it from a loopback-only HTTP server under a random, short-lived token, so
// ffmpeg never resolves or connects to anything on its own.
package mediasrc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/browser"
	"ultraspace/internal/httpx"
)

// Source is something playable. Serve must answer GET and HEAD with Range support.
type Source struct {
	Name  string
	Serve http.HandlerFunc
	// Slow sources (torrents) may take a while to deliver their first bytes.
	Slow bool
	// Subtitles that come with the source, such as .srt files in the same torrent.
	Subtitles []Subtitle
}

// Subtitle is a subtitle file that belongs to a source, itself readable as a source.
type Subtitle struct {
	Src   string `json:"src"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
}

// Resolver turns the part after "scheme:" into a source.
type Resolver func(ctx context.Context, ref string) (*Source, error)

// ErrUnknown is returned for a source string no resolver handles.
var ErrUnknown = errors.New("unknown media source")

type Registry struct {
	log       *slog.Logger
	mu        sync.Mutex
	resolvers map[string]Resolver
	tokens    map[string]*Source
	addr      string // loopback server for ffmpeg, started on first use
}

func New(log *slog.Logger) *Registry {
	r := &Registry{log: log, resolvers: map[string]Resolver{}, tokens: map[string]*Source{}}
	r.Handle("url", r.resolveURL)
	return r
}

// Handle adds a resolver for "scheme:…" sources.
func (r *Registry) Handle(scheme string, res Resolver) {
	r.mu.Lock()
	r.resolvers[scheme] = res
	r.mu.Unlock()
}

func (r *Registry) Resolve(ctx context.Context, src string) (*Source, error) {
	scheme, ref, ok := strings.Cut(src, ":")
	r.mu.Lock()
	res := r.resolvers[scheme]
	r.mu.Unlock()
	if !ok || res == nil || ref == "" || len(src) > 8192 {
		return nil, ErrUnknown
	}
	return res(ctx, ref)
}

// Input makes a loopback URL that ffmpeg can read the source from, valid until release.
func (r *Registry) Input(s *Source) (string, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", nil, err
		}
		r.addr = ln.Addr().String()
		srv := &http.Server{Handler: http.HandlerFunc(r.serveToken), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	}
	var b [24]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	r.tokens[tok] = s
	release := func() {
		r.mu.Lock()
		delete(r.tokens, tok)
		r.mu.Unlock()
	}
	return "http://" + r.addr + "/" + tok, release, nil
}

func (r *Registry) serveToken(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	s := r.tokens[strings.TrimPrefix(req.URL.Path, "/")]
	r.mu.Unlock()
	if s == nil || (req.Method != http.MethodGet && req.Method != http.MethodHead) {
		http.NotFound(w, req)
		return
	}
	s.Serve(w, req)
}

// Register adds GET /api/media/raw?src=…, which plays a source straight in the browser.
func (r *Registry) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/media/raw", require(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s, err := r.Resolve(req.Context(), req.URL.Query().Get("src"))
		if err != nil {
			Fail(w, err)
			return
		}
		// The bytes come from strangers: never let them render as a page on this origin.
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		w.Header().Set("Cache-Control", "no-store")
		s.Serve(&mediaTypeWriter{ResponseWriter: w}, req)
	})))
}

// Fail writes a source error as JSON.
func Fail(w http.ResponseWriter, err error) {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		httpx.Error(w, se.Code, se.Msg)
	case errors.Is(err, ErrUnknown):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Error(w, http.StatusGatewayTimeout, "the source did not answer in time")
	default:
		httpx.Error(w, http.StatusBadGateway, err.Error())
	}
}

// StatusError is an error with the HTTP status to report it as.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// mediaTypeWriter only lets media types through; anything else becomes a download.
type mediaTypeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (m *mediaTypeWriter) WriteHeader(code int) {
	if m.wrote {
		return
	}
	m.wrote = true
	h := m.Header()
	ct := strings.ToLower(h.Get("Content-Type"))
	if !strings.HasPrefix(ct, "video/") && !strings.HasPrefix(ct, "audio/") && !strings.HasPrefix(ct, "text/vtt") {
		h.Set("Content-Type", "application/octet-stream")
	}
	m.ResponseWriter.WriteHeader(code)
}

func (m *mediaTypeWriter) Write(b []byte) (int, error) {
	if !m.wrote {
		m.WriteHeader(http.StatusOK)
	}
	return m.ResponseWriter.Write(b)
}

func (m *mediaTypeWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// ContentType picks a media type from a file name, for sources that serve files.
func ContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".ts", ".m2ts", ".mts":
		return "video/mp2t"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".aac":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga", ".opus":
		return "audio/ogg"
	case ".vtt":
		return "text/vtt"
	}
	return "application/octet-stream"
}

// ---------- url: sources (Stremio addon streams, subtitle files) ----------

var publicClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           browser.PublicDialer().DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("unsupported redirect")
		}
		return nil
	},
}

// PublicClient fetches from public internet addresses only.
func PublicClient() *http.Client { return publicClient }

func (r *Registry) resolveURL(_ context.Context, ref string) (*Source, error) {
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, &StatusError{http.StatusBadRequest, "only http and https addresses can be played"}
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" {
		name = u.Host
	}
	return &Source{Name: name, Serve: func(w http.ResponseWriter, req *http.Request) { proxyRange(w, req, u.String()) }}, nil
}

// proxyRange relays a (ranged) GET to a public URL.
func proxyRange(w http.ResponseWriter, req *http.Request, target string) {
	out, err := http.NewRequestWithContext(req.Context(), req.Method, target, nil)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad address")
		return
	}
	out.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) ultraspace")
	if rg := req.Header.Get("Range"); rg != "" {
		out.Header.Set("Range", rg)
	}
	res, err := publicClient.Do(out)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not reach the stream")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		httpx.Error(w, http.StatusBadGateway, "the stream answered "+strconv.Itoa(res.StatusCode))
		return
	}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := res.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, res.Body) // an error here means the viewer went away
}
