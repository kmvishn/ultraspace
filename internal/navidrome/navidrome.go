// Package navidrome connects the Music app to a Navidrome (or any Subsonic) server.
// The browser talks only to ultraspace; ultraspace adds the credentials and relays
// the Subsonic API, including audio streams and cover art. Only a salted token
// derived from the password is stored, server-side, and never sent to the browser.
package navidrome

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// stateKey is deliberately not one of the keys the /api/state endpoints expose.
const stateKey = "navidrome"

const apiVersion = "1.16.1"

// methods the Music app may call; anything administrative is refused.
var methods = map[string]bool{
	"ping": true, "getAlbumList2": true, "getAlbum": true, "getArtists": true, "getArtist": true, "getArtistInfo2": true,
	"search3": true, "getCoverArt": true, "stream": true, "getRandomSongs": true, "getPlaylists": true, "getPlaylist": true,
	"star": true, "unstar": true, "scrobble": true, "getStarred2": true, "getSong": true, "getGenres": true,
	"getSongsByGenre": true, "getTopSongs": true, "getSimilarSongs2": true, "getLyricsBySongId": true,
	"createPlaylist": true, "updatePlaylist": true,
}

// authParams are set by the server; the browser may not supply them.
var authParams = map[string]bool{"u": true, "p": true, "t": true, "s": true, "v": true, "c": true, "f": true, "apiKey": true}

type creds struct {
	URL   string `json:"url"`
	User  string `json:"user"`
	Token string `json:"token"`
	Salt  string `json:"salt"`
}

type Handler struct {
	store  *store.Store
	log    *slog.Logger
	client *http.Client
}

func New(st *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: st, log: log, client: &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 16,
			IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/navidrome", require(http.HandlerFunc(h.status)))
	mux.Handle("POST /api/navidrome/connect", require(http.HandlerFunc(h.connect)))
	mux.Handle("POST /api/navidrome/disconnect", require(http.HandlerFunc(h.disconnect)))
	mux.Handle("GET /api/navidrome/rest/{method}", require(http.HandlerFunc(h.relay)))
}

func (h *Handler) load(ctx context.Context, userID int64) (creds, bool) {
	st, err := h.store.State(ctx, userID, stateKey)
	if err != nil {
		return creds{}, false
	}
	var c creds
	if json.Unmarshal([]byte(st.Data), &c) != nil || c.URL == "" {
		return creds{}, false
	}
	return c, true
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	c, ok := h.load(r.Context(), u.ID)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"connected": false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"connected": true, "url": c.URL, "user": c.User})
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	base, err := normalizeURL(req.URL)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username == "" || req.Password == "" || len(req.Username) > 200 || len(req.Password) > 500 {
		httpx.Error(w, http.StatusBadRequest, "enter your Navidrome username and password")
		return
	}
	salt := randomHex(8)
	sum := md5.Sum([]byte(req.Password + salt)) // the Subsonic token scheme
	c := creds{URL: base, User: req.Username, Token: hex.EncodeToString(sum[:]), Salt: salt}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, err := h.ping(ctx, c)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	b, _ := json.Marshal(c)
	u, _ := auth.UserFrom(r.Context())
	if err := h.store.SetState(r.Context(), u.ID, stateKey, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: time.Now()}); err != nil {
		h.log.Error("save navidrome", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log.Info("navidrome connected", "user", u.Username, "server", base)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"connected": true, "url": base, "user": c.User, "server": info})
}

func (h *Handler) disconnect(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := h.store.DeleteState(r.Context(), u.ID, stateKey); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("enter the Navidrome address, for example https://music.example.com")
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/app")
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return strings.TrimSuffix(u.String(), "/"), nil
}

func (c creds) endpoint(method string, extra url.Values) string {
	q := url.Values{}
	for k, vs := range extra {
		if !authParams[k] {
			q[k] = vs
		}
	}
	q.Set("u", c.User)
	q.Set("t", c.Token)
	q.Set("s", c.Salt)
	q.Set("v", apiVersion)
	q.Set("c", "ultraspace")
	if method != "stream" && method != "getCoverArt" {
		q.Set("f", "json")
	}
	return c.URL + "/rest/" + method + "?" + q.Encode()
}

// ping checks the address and credentials, returning the server type and version.
func (h *Handler) ping(ctx context.Context, c creds) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("ping", nil), nil)
	res, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %v", c.URL, unwrap(err))
	}
	defer res.Body.Close()
	var out struct {
		R struct {
			Status        string `json:"status"`
			Type          string `json:"type"`
			ServerVersion string `json:"serverVersion"`
			Error         *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"subsonic-response"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("%s did not answer like a Navidrome/Subsonic server (HTTP %d)", c.URL, res.StatusCode)
	}
	if out.R.Status != "ok" {
		if out.R.Error != nil && (out.R.Error.Code == 40 || out.R.Error.Code == 41) {
			return "", errors.New("wrong Navidrome username or password")
		}
		if out.R.Error != nil {
			return "", errors.New(out.R.Error.Message)
		}
		return "", errors.New("the server refused the login")
	}
	return strings.TrimSpace(out.R.Type + " " + out.R.ServerVersion), nil
}

func unwrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// relay forwards one Subsonic call and streams the answer back (JSON, audio or images).
func (h *Handler) relay(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimSuffix(r.PathValue("method"), ".view")
	if !methods[method] {
		httpx.Error(w, http.StatusForbidden, "that Subsonic call is not allowed")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	c, ok := h.load(r.Context(), u.ID)
	if !ok {
		httpx.Error(w, http.StatusNotFound, "Navidrome is not connected")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.endpoint(method, r.URL.Query()), nil)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad request")
		return
	}
	for _, k := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "Accept-Language"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := h.client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			httpx.Error(w, http.StatusBadGateway, fmt.Sprintf("Navidrome: %v", unwrap(err)))
		}
		return
	}
	defer res.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Content-Disposition"} {
		if v := res.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	switch method {
	case "getCoverArt":
		w.Header().Set("Cache-Control", "private, max-age=604800")
	case "stream":
		w.Header().Set("Cache-Control", "private, no-transform")
	default:
		w.Header().Set("Cache-Control", "no-store")
	}
	// Audio and images from another server: never let them run as a page here.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
