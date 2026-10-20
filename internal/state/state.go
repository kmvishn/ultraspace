// Package state stores per-user JSON documents that follow the user between devices,
// such as the open windows of the desktop and the browser's bookmarks.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// limits lists the allowed keys and the largest document each may hold.
var limits = map[string]int64{
	"workspace": 512 << 10,
	"browser":   256 << 10,
	"media":     128 << 10, // video resume positions
	"stremio":   512 << 10, // installed addons (with their manifests) and the library
}

type Handler struct {
	store *store.Store
	log   *slog.Logger
}

func New(st *store.Store, log *slog.Logger) *Handler { return &Handler{store: st, log: log} }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/state/{key}", require(http.HandlerFunc(h.get)))
	mux.Handle("PUT /api/state/{key}", require(http.HandlerFunc(h.put)))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, ok := limits[key]; !ok {
		httpx.Error(w, http.StatusNotFound, "unknown state key")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	st, err := h.store.State(r.Context(), u.ID, key)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": nil, "updated": 0})
		return
	}
	if err != nil {
		h.internal(w, "read state", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data":        json.RawMessage(st.Data),
		"device":      st.Device,
		"device_name": st.DeviceName,
		"updated":     st.UpdatedAt.UnixMilli(),
	})
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	limit, ok := limits[key]
	if !ok {
		httpx.Error(w, http.StatusNotFound, "unknown state key")
		return
	}
	var req struct {
		Device     string          `json:"device"`
		DeviceName string          `json:"device_name"`
		Data       json.RawMessage `json:"data"`
		// Base is the version the client last saw. When another device has saved
		// since then, the write is refused so the client can ask what to keep.
		Base *int64 `json:"base"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, limit+4096) {
		return
	}
	if len(req.Device) == 0 || len(req.Device) > 64 || len(req.DeviceName) > 100 {
		httpx.Error(w, http.StatusBadRequest, "invalid device")
		return
	}
	data := bytes.TrimSpace(req.Data)
	if len(data) == 0 || data[0] != '{' {
		httpx.Error(w, http.StatusBadRequest, "data must be a JSON object")
		return
	}
	if int64(len(data)) > limit {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "state too large")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	if req.Base != nil {
		cur, err := h.store.State(r.Context(), u.ID, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.internal(w, "read state", err)
			return
		}
		if err == nil && cur.Device != req.Device && cur.UpdatedAt.UnixMilli() > *req.Base {
			httpx.WriteJSON(w, http.StatusConflict, map[string]any{
				"error": "changed on another device", "device": cur.Device,
				"device_name": cur.DeviceName, "updated": cur.UpdatedAt.UnixMilli(),
			})
			return
		}
	}
	now := time.Now()
	err := h.store.SetState(r.Context(), u.ID, key, store.State{
		Data: string(data), Device: req.Device, DeviceName: req.DeviceName, UpdatedAt: now,
	})
	if err != nil {
		h.internal(w, "save state", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"updated": now.UnixMilli()})
}

func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
