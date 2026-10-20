// Package clip is a clipboard shared by all of a user's devices, plus the live event
// stream (Server-Sent Events) that tells open desktops when it changes.
package clip

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

const (
	stateKey = "clipboard" // server-only: not one of the keys /api/state exposes
	maxItems = 25
	maxText  = 256 << 10
)

type Item struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Device string `json:"device"`
	Time   int64  `json:"t"`
}

type Handler struct {
	store *store.Store
	log   *slog.Logger
	mu    sync.Mutex // serialises read-modify-write of the history
	hub   hub
}

func New(st *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: st, log: log, hub: hub{subs: map[int64]map[chan []byte]struct{}{}}}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/clipboard", require(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/clipboard", require(http.HandlerFunc(h.add)))
	mux.Handle("DELETE /api/clipboard/{id}", require(http.HandlerFunc(h.remove)))
	mux.Handle("DELETE /api/clipboard", require(http.HandlerFunc(h.clear)))
	mux.Handle("GET /api/events", require(http.HandlerFunc(h.events)))
}

func (h *Handler) load(r *http.Request, userID int64) []Item {
	st, err := h.store.State(r.Context(), userID, stateKey)
	if err != nil {
		return []Item{}
	}
	var doc struct {
		Items []Item `json:"items"`
	}
	if json.Unmarshal([]byte(st.Data), &doc) != nil || doc.Items == nil {
		return []Item{}
	}
	return doc.Items
}

func (h *Handler) save(r *http.Request, userID int64, items []Item) error {
	b, _ := json.Marshal(map[string]any{"items": items})
	return h.store.SetState(r.Context(), userID, stateKey, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: time.Now()})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	h.mu.Lock()
	items := h.load(r, u.ID)
	h.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		Device string `json:"device"`
		Origin string `json:"origin"` // the device id that sent it, so it can ignore its own event
	}
	if !httpx.DecodeJSONLimit(w, r, &req, maxText+4096) {
		return
	}
	if req.Text == "" || len(req.Text) > maxText || len(req.Device) > 100 || len(req.Origin) > 64 {
		httpx.Error(w, http.StatusBadRequest, "nothing to share, or too large (max 256 KB)")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	h.mu.Lock()
	items := h.load(r, u.ID)
	item := Item{ID: randomID(), Text: req.Text, Device: req.Device, Time: time.Now().UnixMilli()}
	kept := []Item{item}
	for _, it := range items {
		if it.Text != req.Text && len(kept) < maxItems {
			kept = append(kept, it)
		}
	}
	err := h.save(r, u.ID, kept)
	h.mu.Unlock()
	if err != nil {
		h.log.Error("save clipboard", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.hub.publish(u.ID, "clipboard", map[string]any{"item": item, "origin": req.Origin})
	httpx.WriteJSON(w, http.StatusOK, item)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	h.mu.Lock()
	items := h.load(r, u.ID)
	kept := items[:0]
	for _, it := range items {
		if it.ID != id {
			kept = append(kept, it)
		}
	}
	err := h.save(r, u.ID, kept)
	h.mu.Unlock()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.hub.publish(u.ID, "clipboard-changed", nil)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) clear(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	h.mu.Lock()
	err := h.save(r, u.ID, []Item{})
	h.mu.Unlock()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.hub.publish(u.ID, "clipboard-changed", nil)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Publish sends an event to every open desktop of the user (for other packages).
func (h *Handler) Publish(userID int64, event string, data any) { h.hub.publish(userID, event, data) }

// events streams Server-Sent Events until the client goes away.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w) // sees through wrapping middleware
	u, _ := auth.UserFrom(r.Context())
	ch := h.hub.subscribe(u.ID)
	defer h.hub.unsubscribe(u.ID, ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	if rc.Flush() != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil || rc.Flush() != nil {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

type hub struct {
	mu   sync.Mutex
	subs map[int64]map[chan []byte]struct{}
}

func (h *hub) subscribe(userID int64) chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[chan []byte]struct{}{}
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(userID int64, ch chan []byte) {
	h.mu.Lock()
	delete(h.subs[userID], ch)
	if len(h.subs[userID]) == 0 {
		delete(h.subs, userID)
	}
	h.mu.Unlock()
}

func (h *hub) publish(userID int64, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- msg:
		default: // a stalled stream misses an event; the client resyncs on reconnect
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
