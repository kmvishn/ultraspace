package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// A signed-in Stremio account keeps addons, the library and watch progress in sync with
// the official apps. The session key lives in a server-side record that the /api/state
// endpoints never expose (like Navidrome's token); the password is never stored.
const authKeyState = "stremio-account"

type account struct {
	AuthKey string `json:"authKey"`
	Email   string `json:"email"`
}

var errNoAccount = errors.New("not signed in to a Stremio account")

func (h *Handler) registerAccount(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	handle := func(p string, f http.HandlerFunc) { mux.Handle(p, require(f)) }
	handle("GET /api/stremio/account", h.handleAccount)
	handle("POST /api/stremio/account/login", h.handleLogin)
	handle("POST /api/stremio/account/logout", h.handleLogout)
	handle("GET /api/stremio/account/addons", h.handleGetAddons)
	handle("PUT /api/stremio/account/addons", h.handleSetAddons)
	handle("GET /api/stremio/account/library", h.handleGetLibrary)
	handle("POST /api/stremio/account/library", h.handleLibraryItem)
}

func (h *Handler) account(ctx context.Context) (account, error) {
	u, _ := auth.UserFrom(ctx)
	st, err := h.store.State(ctx, u.ID, authKeyState)
	if err != nil {
		return account{}, errNoAccount
	}
	var a account
	if json.Unmarshal([]byte(st.Data), &a) != nil || a.AuthKey == "" {
		return account{}, errNoAccount
	}
	return a, nil
}

// call runs an account API method; a rejected session signs the user out here too.
func (h *Handler) call(ctx context.Context, a account, method string, body map[string]any, out any) error {
	body["authKey"] = a.AuthKey
	err := stremioCall(ctx, method, body, out)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "session") {
		u, _ := auth.UserFrom(ctx)
		_ = h.store.DeleteState(context.Background(), u.ID, authKeyState)
		return errNoAccount
	}
	return err
}

func (h *Handler) accountFail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoAccount) {
		httpx.Error(w, http.StatusUnauthorized, err.Error())
		return
	}
	httpx.Error(w, http.StatusBadGateway, err.Error())
}

func (h *Handler) handleAccount(w http.ResponseWriter, r *http.Request) {
	a, err := h.account(r.Context())
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"signedIn": false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"signedIn": true, "email": a.Email})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 4<<10) {
		return
	}
	if req.Email == "" || req.Password == "" {
		httpx.Error(w, http.StatusBadRequest, "give your Stremio email and password")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var login struct {
		AuthKey string `json:"authKey"`
		User    struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := stremioCall(ctx, "login", map[string]any{"type": "Login", "email": req.Email, "password": req.Password, "facebook": false}, &login); err != nil || login.AuthKey == "" {
		msg := "Stremio did not accept that email and password"
		if err != nil && strings.Contains(err.Error(), "reach") {
			msg = err.Error()
		}
		httpx.Error(w, http.StatusForbidden, msg)
		return
	}
	email := login.User.Email
	if email == "" {
		email = req.Email
	}
	b, _ := json.Marshal(account{AuthKey: login.AuthKey, Email: email})
	u, _ := auth.UserFrom(r.Context())
	if err := h.store.SetState(r.Context(), u.ID, authKeyState, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: time.Now()}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not save the session")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"signedIn": true, "email": email})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if a, err := h.account(r.Context()); err == nil {
		_ = stremioCall(r.Context(), "logout", map[string]any{"type": "Logout", "authKey": a.AuthKey}, &struct{}{})
	}
	u, _ := auth.UserFrom(r.Context())
	_ = h.store.DeleteState(r.Context(), u.ID, authKeyState)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"signedIn": false})
}

// ---------- addons ----------

// remoteAddon is an entry of the account's addon collection, kept whole so that fields
// this app does not know (flags, newer manifest keys) survive a round trip.
type remoteAddon = map[string]any

func (h *Handler) collection(ctx context.Context, a account) ([]remoteAddon, error) {
	var coll struct {
		Addons []remoteAddon `json:"addons"`
	}
	err := h.call(ctx, a, "addonCollectionGet", map[string]any{"type": "AddonCollectionGet", "update": true}, &coll)
	return coll.Addons, err
}

func transport(ad remoteAddon) string { s, _ := ad["transportUrl"].(string); return s }

func flag(ad remoteAddon, name string) bool {
	f, _ := ad["flags"].(map[string]any)
	v, _ := f[name].(bool)
	return v
}

// usable reports whether this app can use an account addon (not the desktop app's
// local-files addon on 127.0.0.1, which only exists on that computer).
func usable(ad remoteAddon) bool {
	u, err := manifestURL(transport(ad))
	return err == nil && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && ad["manifest"] != nil
}

func (h *Handler) handleGetAddons(w http.ResponseWriter, r *http.Request) {
	a, err := h.account(r.Context())
	if err != nil {
		h.accountFail(w, err)
		return
	}
	coll, err := h.collection(r.Context(), a)
	if err != nil {
		h.accountFail(w, err)
		return
	}
	out := []map[string]any{}
	for _, ad := range coll {
		if usable(ad) {
			u, _ := manifestURL(transport(ad))
			out = append(out, map[string]any{"url": u.String(), "manifest": ad["manifest"], "protected": flag(ad, "protected")})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"addons": out})
}

// handleSetAddons makes the account's collection match this app's list, in its order.
// Protected addons and ones this app cannot use (the desktop local-files addon) are kept.
func (h *Handler) handleSetAddons(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addons []struct {
			URL      string          `json:"url"`
			Manifest json.RawMessage `json:"manifest"`
		} `json:"addons"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 4<<20) {
		return
	}
	a, err := h.account(r.Context())
	if err != nil {
		h.accountFail(w, err)
		return
	}
	coll, err := h.collection(r.Context(), a)
	if err != nil {
		h.accountFail(w, err)
		return
	}
	urls := make([]string, len(req.Addons))
	manifests := make([]json.RawMessage, len(req.Addons))
	for i, x := range req.Addons {
		urls[i], manifests[i] = x.URL, x.Manifest
	}
	next := mergeCollection(coll, urls, manifests)
	if err := h.call(r.Context(), a, "addonCollectionSet", map[string]any{"type": "AddonCollectionSet", "addons": next}, &struct{}{}); err != nil {
		h.accountFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// mergeCollection builds the account's new addon collection: this app's list in its
// order (reusing the account's entries, so flags survive), then any protected or
// unusable addons the account had that this app may not remove.
func mergeCollection(coll []remoteAddon, urls []string, manifests []json.RawMessage) []remoteAddon {
	byURL := map[string]remoteAddon{}
	for _, ad := range coll {
		if u, err := manifestURL(transport(ad)); err == nil {
			byURL[u.String()] = ad
		}
	}
	next := []remoteAddon{}
	seen := map[string]bool{}
	for i, raw := range urls {
		u, err := manifestURL(raw)
		if err != nil || seen[u.String()] || len(manifests[i]) == 0 {
			continue
		}
		seen[u.String()] = true
		if old := byURL[u.String()]; old != nil {
			next = append(next, old)
			continue
		}
		var m any
		if json.Unmarshal(manifests[i], &m) != nil {
			continue
		}
		next = append(next, remoteAddon{"transportUrl": u.String(), "transportName": "http", "manifest": m, "flags": map[string]any{"official": false, "protected": false}})
	}
	for _, ad := range coll {
		u, err := manifestURL(transport(ad))
		if err == nil && !seen[u.String()] && (flag(ad, "protected") || !usable(ad)) {
			next = append(next, ad) // never drop what this app may not remove or cannot see
		}
	}
	return next
}

// ---------- library and watch progress ----------

type libItem = map[string]any

func (h *Handler) libraryGet(ctx context.Context, a account, ids []string) ([]libItem, error) {
	var items []libItem
	body := map[string]any{"collection": "libraryItem", "ids": ids, "all": len(ids) == 0}
	if ids == nil {
		body["ids"] = []string{}
	}
	err := h.call(ctx, a, "datastoreGet", body, &items)
	return items, err
}

// handleGetLibrary returns the account library: saved titles and, as "temp" items,
// titles only watched (Stremio's continue watching).
func (h *Handler) handleGetLibrary(w http.ResponseWriter, r *http.Request) {
	a, err := h.account(r.Context())
	if err != nil {
		h.accountFail(w, err)
		return
	}
	items, err := h.libraryGet(r.Context(), a, nil)
	if err != nil {
		h.accountFail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if removed, _ := it["removed"].(bool); removed {
			if temp, _ := it["temp"].(bool); !temp {
				continue
			}
		}
		st, _ := it["state"].(map[string]any)
		out = append(out, map[string]any{
			"id": it["_id"], "type": it["type"], "name": it["name"], "poster": it["poster"],
			"removed": it["removed"], "temp": it["temp"], "mtime": it["_mtime"],
			"offset": st["timeOffset"], "duration": st["duration"], "video": st["video_id"], "lastWatched": st["lastWatched"],
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleLibraryItem changes one title: {id, type, name, poster} plus "saved" (add to or
// remove from the library) and/or watch progress {video, t, d} in seconds.
func (h *Handler) handleLibraryItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string   `json:"id"`
		Type   string   `json:"type"`
		Name   string   `json:"name"`
		Poster string   `json:"poster"`
		Saved  *bool    `json:"saved"`
		Video  string   `json:"video"`
		T      *float64 `json:"t"`
		D      float64  `json:"d"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 16<<10) {
		return
	}
	if req.ID == "" || req.Type == "" || len(req.ID) > 300 {
		httpx.Error(w, http.StatusBadRequest, "give the title's id and type")
		return
	}
	a, err := h.account(r.Context())
	if err != nil {
		h.accountFail(w, err)
		return
	}
	items, err := h.libraryGet(r.Context(), a, []string{req.ID})
	if err != nil {
		h.accountFail(w, err)
		return
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	it := libItem{"_id": req.ID, "type": req.Type, "name": req.Name, "poster": req.Poster, "posterShape": "poster",
		"removed": true, "temp": true, "_ctime": now, "behaviorHints": map[string]any{"defaultVideoId": nil, "featuredVideoId": nil, "hasScheduledVideos": false},
		"state": map[string]any{"lastWatched": now, "timeWatched": 0, "timeOffset": 0, "overallTimeWatched": 0, "timesWatched": 0,
			"flaggedWatched": 0, "duration": 0, "video_id": nil, "watched": nil, "noNotif": false}}
	if i := slices.IndexFunc(items, func(x libItem) bool { return x["_id"] == req.ID }); i >= 0 {
		it = items[i]
	}
	if req.Name != "" {
		it["name"] = req.Name
	}
	if req.Poster != "" {
		it["poster"] = req.Poster
	}
	if req.Saved != nil {
		it["removed"], it["temp"] = !*req.Saved, false
	}
	if req.T != nil {
		st, _ := it["state"].(map[string]any)
		if st == nil {
			st = map[string]any{}
		}
		ms := func(s float64) int64 { return int64(s * 1000) }
		st["timeOffset"], st["duration"], st["lastWatched"] = ms(*req.T), ms(req.D), now
		if req.Video != "" {
			st["video_id"] = req.Video
		}
		if req.D > 0 && *req.T >= req.D-20 { // finished
			st["timeOffset"] = 0
			n, _ := st["timesWatched"].(float64)
			st["timesWatched"] = int64(n) + 1
		}
		it["state"] = st
		if removed, _ := it["removed"].(bool); removed {
			it["temp"] = true // watched but not saved: shows in continue watching
		}
	}
	it["_mtime"] = now
	if err := h.call(r.Context(), a, "datastorePut", map[string]any{"collection": "libraryItem", "changes": []libItem{it}}, &struct{}{}); err != nil {
		h.accountFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
