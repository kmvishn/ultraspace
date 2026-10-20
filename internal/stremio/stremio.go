// Package stremio backs the Stremio app: a client for the open Stremio addon protocol
// (https://github.com/Stremio/stremio-addon-sdk/blob/master/docs/protocol.md).
//
// The browser cannot reach addons itself (the page's CSP allows only this site), so the
// server relays requests, but only to addons the user installed, only for the protocol's
// resources (catalog, meta, stream, subtitles), only to public addresses, and only JSON.
// Posters and backgrounds come through a public-only image relay.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/mediasrc"
	"ultraspace/internal/store"
)

// Addons every user starts with: Cinemeta (the catalog of films and series) and
// OpenSubtitles. Both are run by the Stremio project and only describe content.
var DefaultAddons = []string{
	"https://v3-cinemeta.strem.io/manifest.json",
	"https://opensubtitles-v3.strem.io/manifest.json",
}

const (
	stateKey    = "stremio"
	maxJSON     = 4 << 20
	maxImage    = 6 << 20
	cacheTTL    = 10 * time.Minute
	cacheMax    = 64
	cacheMaxObj = 1 << 20
)

var resources = map[string]bool{"catalog": true, "meta": true, "stream": true, "subtitles": true}

// segment matches one path element of an addon request (type, id, or extra).
var segment = regexp.MustCompile(`^[^/?#\\\x00-\x1f]+$`)

type Handler struct {
	store *store.Store
	log   *slog.Logger

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

func New(st *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: st, log: log, cache: map[string]cached{}}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("POST /api/stremio/manifest", require(http.HandlerFunc(h.handleManifest)))
	mux.Handle("GET /api/stremio/addon", require(http.HandlerFunc(h.handleAddon)))
	mux.Handle("GET /api/stremio/img", require(http.HandlerFunc(h.handleImage)))
	h.registerAccount(mux, require)
	mux.Handle("GET /api/stremio/defaults", require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"addons": DefaultAddons})
	})))
}

// manifestURL normalises what a user pastes: stremio:// links, missing /manifest.json.
func manifestURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(raw, "stremio://"); ok {
		raw = "https://" + rest
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("give the addon's https address (ending in /manifest.json)")
	}
	if !strings.HasSuffix(u.Path, "/manifest.json") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/manifest.json"
	}
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// handleManifest fetches and checks an addon's manifest, for installing it.
func (h *Handler) handleManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 8<<10) {
		return
	}
	u, err := manifestURL(req.URL)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := h.fetchJSON(r.Context(), u.String(), false)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not load the addon: "+err.Error())
		return
	}
	var m struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Resources []json.RawMessage `json:"resources"`
	}
	if json.Unmarshal(body, &m) != nil || m.ID == "" || m.Name == "" || len(m.Resources) == 0 {
		httpx.Error(w, http.StatusBadGateway, "that address is not a Stremio addon")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"url": u.String(), "manifest": json.RawMessage(body)})
}

// handleAddon relays /{resource}/{type}/{id}[/{extra}].json to an installed addon.
func (h *Handler) handleAddon(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	resource, typ, id, extra := q.Get("resource"), q.Get("type"), q.Get("id"), q.Get("extra")
	ok := func(s string) bool { return len(s) <= 1024 && segment.MatchString(s) }
	if !resources[resource] || !ok(typ) || !ok(id) || (extra != "" && !ok(extra)) {
		httpx.Error(w, http.StatusBadRequest, "bad addon request")
		return
	}
	u, err := manifestURL(q.Get("addon"))
	if err != nil || !h.installed(r.Context(), u.String()) {
		httpx.Error(w, http.StatusForbidden, "that addon is not installed")
		return
	}
	base := strings.TrimSuffix(u.String(), "/manifest.json")
	target := base + "/" + resource + "/" + url.PathEscape(typ) + "/" + url.PathEscape(id)
	if extra != "" {
		// extra is already a query string ("search=x&skip=20"); keep & and =, escape the rest.
		target += "/" + strings.NewReplacer("%3D", "=", "%26", "&").Replace(url.PathEscape(extra))
	}
	target += ".json"
	body, err := h.fetchJSON(r.Context(), target, resource != "stream")
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(body)
}

// installed reports whether the user has this addon (or it is a default one).
func (h *Handler) installed(ctx context.Context, manifest string) bool {
	for _, d := range DefaultAddons {
		if d == manifest {
			return true
		}
	}
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return false
	}
	st, err := h.store.State(ctx, u.ID, stateKey)
	if err != nil {
		return false
	}
	var doc struct {
		Addons []struct {
			URL string `json:"url"`
		} `json:"addons"`
	}
	if json.Unmarshal([]byte(st.Data), &doc) != nil {
		return false
	}
	for _, a := range doc.Addons {
		if a.URL == manifest {
			return true
		}
	}
	return false
}

func (h *Handler) fetchJSON(ctx context.Context, target string, cache bool) ([]byte, error) {
	if cache {
		h.mu.Lock()
		c, ok := h.cache[target]
		h.mu.Unlock()
		if ok && time.Since(c.at) < cacheTTL {
			return c.body, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ultraspace (Stremio addon client)")
	res, err := mediasrc.PublicClient().Do(req)
	if err != nil {
		return nil, errors.New("the addon did not answer")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("the addon answered " + res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(body) > maxJSON {
		return nil, errors.New("the addon's answer was too large")
	}
	if !json.Valid(body) {
		return nil, errors.New("the addon's answer was not JSON")
	}
	if cache && len(body) <= cacheMaxObj {
		h.mu.Lock()
		if len(h.cache) >= cacheMax {
			var oldest string
			var at time.Time
			for k, v := range h.cache {
				if oldest == "" || v.at.Before(at) {
					oldest, at = k, v.at
				}
			}
			delete(h.cache, oldest)
		}
		h.cache[target] = cached{body: body, at: time.Now()}
		h.mu.Unlock()
	}
	return body, nil
}

const stremioAPI = "https://api.strem.io/api/"

// stremioCall posts one request to Stremio's account API.
func stremioCall(ctx context.Context, method string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stremioAPI+method, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := mediasrc.PublicClient().Do(req)
	if err != nil {
		return errors.New("could not reach Stremio")
	}
	defer res.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJSON)).Decode(&env); err != nil {
		return errors.New("unexpected answer from Stremio")
	}
	if env.Error != nil {
		return errors.New(env.Error.Message)
	}
	return json.Unmarshal(env.Result, out)
}

// handleImage relays a poster, logo or background image from a public address.
func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(r.URL.Query().Get("u"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		httpx.Error(w, http.StatusBadRequest, "bad image address")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*")
	res, err := mediasrc.PublicClient().Do(req)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "image unavailable")
		return
	}
	defer res.Body.Close()
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") || strings.Contains(ct, "svg") {
		httpx.Error(w, http.StatusBadGateway, "image unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxImage+1))
	if err != nil || len(body) > maxImage {
		httpx.Error(w, http.StatusBadGateway, "image too large")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(body)
}
