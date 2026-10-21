// Package aichat is the server side of Ultra Chat Bot: a chat with hosted models
// (Gemini, Mistral, Groq and OpenRouter, all via their OpenAI-compatible APIs). API keys are kept on
// the server and never sent to the browser; the server calls the provider and
// streams the answer back. Conversations are stored per user, so they follow you
// to every device.
package aichat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// Server-only state keys: none of these are exposed through /api/state.
const (
	configKey = "aichat"
	indexKey  = "aichat:index"
	convKey   = "aichat:conv:"
)

const systemPrompt = "You are Ultra Chat Bot, a helpful assistant built into ultraspace, a personal web desktop " +
	"that runs on the user's own server. Answer clearly and concisely, and use Markdown for formatting and code."

type Provider struct {
	ID   string
	Name string
	Base string // OpenAI-compatible API root, e.g. https://api.mistral.ai/v1
	// FreeOnly lists just the no-cost models (ids ending in ":free"); OpenRouter
	// offers hundreds of models and most of them are paid.
	FreeOnly bool
}

// DefaultProviders are the hosted APIs Ultra Chat Bot knows about.
func DefaultProviders() []Provider {
	return []Provider{
		{ID: "gemini", Name: "Gemini", Base: "https://generativelanguage.googleapis.com/v1beta/openai"},
		{ID: "mistral", Name: "Mistral", Base: "https://api.mistral.ai/v1"},
		{ID: "groq", Name: "Groq", Base: "https://api.groq.com/openai/v1"},
		{ID: "openrouter", Name: "OpenRouter", Base: "https://openrouter.ai/api/v1", FreeOnly: true},
	}
}

type settingsDoc struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Keys         map[string]string `json:"keys,omitempty"`
	DefaultModel string            `json:"default_model,omitempty"`
}

type Handler struct {
	store     *store.Store
	log       *slog.Logger
	providers []Provider
	client    *http.Client
	// SearchURL is the Brave Search API endpoint (tests point it elsewhere).
	SearchURL string

	mu     sync.Mutex // serialises read-modify-write of settings and the index
	models map[string]modelCache
}

type modelCache struct {
	at   time.Time
	list []Model
}

type Model struct {
	ID       string `json:"id"` // "provider:model"
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

func New(st *store.Store, providers []Provider, log *slog.Logger) *Handler {
	return &Handler{
		store: st, log: log, providers: providers, models: map[string]modelCache{},
		SearchURL: "https://api.search.brave.com/res/v1/web/search",
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 8,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	handle := func(p string, f http.HandlerFunc) { mux.Handle(p, require(f)) }
	handle("GET /api/ai/config", h.getConfig)
	handle("PUT /api/ai/config", h.putConfig)
	handle("GET /api/ai/models", h.listModels)
	handle("POST /api/ai/chat", h.chat)
	handle("GET /api/ai/conversations", h.listConversations)
	handle("GET /api/ai/conversations/{id}", h.getConversation)
	handle("PUT /api/ai/conversations/{id}", h.putConversation)
	handle("DELETE /api/ai/conversations/{id}", h.deleteConversation)
}

// ---------- settings ----------

func (h *Handler) load(ctx context.Context, userID int64) settingsDoc {
	var s settingsDoc
	if st, err := h.store.State(ctx, userID, configKey); err == nil {
		_ = json.Unmarshal([]byte(st.Data), &s)
	}
	if s.Keys == nil {
		s.Keys = map[string]string{}
	}
	return s
}

func (h *Handler) save(ctx context.Context, userID int64, s settingsDoc) error {
	b, _ := json.Marshal(s)
	return h.store.SetState(ctx, userID, configKey, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: time.Now()})
}

func hint(key string) string {
	if len(key) < 8 {
		return "set"
	}
	return "••••" + key[len(key)-4:]
}

func (h *Handler) getConfig(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	s := h.load(r.Context(), u.ID)
	provs := []map[string]any{}
	haveKey := false
	for _, p := range h.providers {
		k := s.Keys[p.ID]
		provs = append(provs, map[string]any{"id": p.ID, "name": p.Name, "configured": k != "", "hint": map[bool]string{true: hint(k), false: ""}[k != ""]})
		haveKey = haveKey || k != ""
	}
	bk := s.Keys[braveID]
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled": s.Enabled == nil || *s.Enabled, "providers": provs, "configured": haveKey, "default_model": s.DefaultModel,
		"search": map[string]any{"id": braveID, "name": "Brave Search", "configured": bk != "", "hint": map[bool]string{true: hint(bk), false: ""}[bk != ""]},
	})
}

func (h *Handler) putConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled      *bool             `json:"enabled"`
		Keys         map[string]string `json:"keys"` // "" removes a key
		DefaultModel *string           `json:"default_model"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.load(r.Context(), u.ID)
	if req.Enabled != nil {
		s.Enabled = req.Enabled
	}
	for id, key := range req.Keys {
		if id != braveID && !slices.ContainsFunc(h.providers, func(p Provider) bool { return p.ID == id }) {
			httpx.Error(w, http.StatusBadRequest, "unknown provider "+id)
			return
		}
		key = strings.TrimSpace(key)
		if len(key) > 500 || strings.ContainsAny(key, " \r\n\t") {
			httpx.Error(w, http.StatusBadRequest, "that does not look like an API key")
			return
		}
		if key == "" {
			delete(s.Keys, id)
		} else {
			s.Keys[id] = key
		}
		delete(h.models, cacheKey(u.ID, id))
	}
	if req.DefaultModel != nil {
		s.DefaultModel = *req.DefaultModel
	}
	if err := h.save(r.Context(), u.ID, s); err != nil {
		h.internal(w, "save ai settings", err)
		return
	}
	h.log.Info("ultra chat settings changed", "user", u.Username)
	h.getConfig(w, r)
}

// ---------- models ----------

func cacheKey(userID int64, provider string) string { return fmt.Sprint(userID, "/", provider) }

// Gemini's list includes embedding, image and audio models, Groq's speech and safety
// classifiers; keep the chat ones.
var notChat = regexp.MustCompile(`embed|imagen|veo|tts|aqa|image|audio|live|vision|moderation|ocr|transcribe|fim|whisper|guard|safety`)

// dated matches versioned ids like "ministral-8b-2512" (Mistral) whose "-latest" alias is also listed.
var dated = regexp.MustCompile(`^(.*)-\d{4}$`)

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	s := h.load(r.Context(), u.ID)
	out := []Model{}
	errs := map[string]string{}
	for _, p := range h.providers {
		key := s.Keys[p.ID]
		if key == "" {
			continue
		}
		list, err := h.models_(r.Context(), u.ID, p, key)
		if err != nil {
			errs[p.ID] = err.Error()
			continue
		}
		out = append(out, list...)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": out, "errors": errs, "default_model": s.DefaultModel})
}

func (h *Handler) models_(ctx context.Context, userID int64, p Provider, key string) ([]Model, error) {
	h.mu.Lock()
	c, ok := h.models[cacheKey(userID, p.ID)]
	h.mu.Unlock()
	if ok && time.Since(c.at) < 10*time.Minute {
		return c.list, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.Base+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer", p.Name)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", p.Name, apiError(res))
	}
	var body struct {
		Data []struct {
			ID           string `json:"id"`
			Deprecation  any    `json:"deprecation"`
			Active       *bool  `json:"active"` // Groq marks retired models inactive
			Capabilities *struct {
				Chat bool `json:"completion_chat"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s sent an unexpected model list", p.Name)
	}
	seen := map[string]bool{}
	var list []Model
	for _, m := range body.Data {
		id := strings.TrimPrefix(m.ID, "models/")
		if id == "" || seen[id] || notChat.MatchString(strings.ToLower(id)) || m.Deprecation != nil ||
			m.Active != nil && !*m.Active || p.FreeOnly && !strings.HasSuffix(id, ":free") ||
			m.Capabilities != nil && !m.Capabilities.Chat {
			continue
		}
		seen[id] = true
		list = append(list, Model{ID: p.ID + ":" + id, Provider: p.Name, Name: id})
	}
	// Show "x-latest" instead of every dated "x-2512" version behind it.
	list = slices.DeleteFunc(list, func(m Model) bool {
		g := dated.FindStringSubmatch(m.Name)
		return g != nil && seen[g[1]+"-latest"]
	})
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	h.mu.Lock()
	h.models[cacheKey(userID, p.ID)] = modelCache{at: time.Now(), list: list}
	h.mu.Unlock()
	return list, nil
}

// apiError pulls the provider's error message out of a failed response.
func apiError(res *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	// Some APIs wrap the error in a list.
	var list []struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &list) == nil && len(list) > 0 && list[0].Error.Message != "" {
		return list[0].Error.Message
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return "the API key was refused"
	}
	return fmt.Sprintf("HTTP %d", res.StatusCode)
}

// ---------- chat ----------

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Search   bool      `json:"search"` // look the question up on the web first
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 2<<20) {
		return
	}
	provID, model, ok := strings.Cut(req.Model, ":")
	i := slices.IndexFunc(h.providers, func(p Provider) bool { return p.ID == provID })
	if !ok || i < 0 || model == "" {
		httpx.Error(w, http.StatusBadRequest, "choose a model")
		return
	}
	p := h.providers[i]
	msgs := []message{{Role: "system", Content: systemPrompt}}
	for _, m := range req.Messages {
		if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) != "" {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) < 2 || msgs[len(msgs)-1].Role != "user" {
		httpx.Error(w, http.StatusBadRequest, "nothing to answer")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	cfg := h.load(r.Context(), u.ID)
	key := cfg.Keys[p.ID]
	if key == "" {
		httpx.Error(w, http.StatusPreconditionFailed, "add a "+p.Name+" API key in Settings → Ultra Chat Bot")
		return
	}
	var sources []source
	var searchNote string
	if req.Search {
		bk := cfg.Keys[braveID]
		if bk == "" {
			httpx.Error(w, http.StatusPreconditionFailed, "add a Brave Search API key in Settings → Ultra Chat Bot to search the web")
			return
		}
		var err error
		sources, err = h.search(r.Context(), bk, msgs[len(msgs)-1].Content)
		if err != nil {
			searchNote = "Web search failed (" + err.Error() + "); answering without it."
		} else if len(sources) > 0 {
			// Results go in as a second system message, right before the conversation.
			msgs = append(msgs[:1], append([]message{{Role: "system", Content: searchContext(sources)}}, msgs[1:]...)...)
		} else {
			searchNote = "The web search found nothing; answering without it."
		}
	}

	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "stream": true})
	up, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, p.Base+"/chat/completions", bytes.NewReader(body))
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+key)
	res, err := h.client.Do(up)
	if err != nil {
		if r.Context().Err() == nil {
			httpx.Error(w, http.StatusBadGateway, p.Name+" did not answer")
		}
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		httpx.Error(w, http.StatusBadGateway, p.Name+": "+apiError(res))
		return
	}

	// Relay as Server-Sent Events: "delta" events with text, then "done".
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if len(sources) > 0 {
		send("sources", map[string]any{"sources": sources})
	}
	if searchNote != "" {
		send("notice", map[string]string{"text": searchNote})
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	finish := ""
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			send("error", map[string]string{"error": chunk.Error.Message})
			return
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" && !send("delta", map[string]string{"text": c.Delta.Content}) {
				return // the browser went away; the context cancels the upstream request
			}
			if c.Finish != "" {
				finish = c.Finish
			}
		}
	}
	if err := sc.Err(); err != nil && r.Context().Err() == nil {
		send("error", map[string]string{"error": "the answer was cut off"})
		return
	}
	send("done", map[string]string{"finish": finish})
}

// ---------- web search (Brave) ----------

const braveID = "brave"

type source struct {
	N       int    `json:"n"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// search asks Brave for the top results for the question.
func (h *Handler) search(ctx context.Context, key, question string) ([]source, error) {
	q := strings.Join(strings.Fields(question), " ")
	if r := []rune(q); len(r) > 380 { // Brave limits queries to 400 characters
		q = string(r[:380])
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.SearchURL+"?count=6&safesearch=moderate&q="+urlQuery(q), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	res, err := h.client.Do(req)
	if err != nil {
		return nil, errors.New("Brave did not answer")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("Brave: " + apiError(res))
	}
	var body struct {
		Web struct {
			Results []struct {
				Title       string   `json:"title"`
				URL         string   `json:"url"`
				Description string   `json:"description"`
				Extra       []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return nil, errors.New("unexpected answer from Brave")
	}
	var out []source
	for _, r := range body.Web.Results {
		if !strings.HasPrefix(r.URL, "https://") && !strings.HasPrefix(r.URL, "http://") {
			continue
		}
		snip := strings.TrimSpace(tagRE.ReplaceAllString(strings.Join(append([]string{r.Description}, r.Extra...), " "), ""))
		if rr := []rune(snip); len(rr) > 700 {
			snip = string(rr[:700]) + "…"
		}
		out = append(out, source{N: len(out) + 1, Title: tagRE.ReplaceAllString(r.Title, ""), URL: r.URL, Snippet: snip})
		if len(out) == 5 {
			break
		}
	}
	return out, nil
}

func searchContext(sources []source) string {
	var b strings.Builder
	b.WriteString("Web search results for the user's latest message. Use them when they help, cite them like [1], " +
		"and say so if they do not answer the question.\n\n")
	for _, s := range sources {
		fmt.Fprintf(&b, "[%d] %s (%s)\n%s\n\n", s.N, s.Title, s.URL, s.Snippet)
	}
	return b.String()
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// ---------- conversations ----------

type convMeta struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Model   string `json:"model"`
	Updated int64  `json:"updated"`
}

var idRE = regexp.MustCompile(`^[a-f0-9]{16}$`)

func (h *Handler) index(ctx context.Context, userID int64) []convMeta {
	var idx struct {
		Items []convMeta `json:"items"`
	}
	if st, err := h.store.State(ctx, userID, indexKey); err == nil {
		_ = json.Unmarshal([]byte(st.Data), &idx)
	}
	return idx.Items
}

func (h *Handler) saveIndex(ctx context.Context, userID int64, items []convMeta) error {
	sort.Slice(items, func(i, j int) bool { return items[i].Updated > items[j].Updated })
	b, _ := json.Marshal(map[string]any{"items": items})
	return h.store.SetState(ctx, userID, indexKey, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: time.Now()})
}

func (h *Handler) listConversations(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	items := h.index(r.Context(), u.ID)
	if items == nil {
		items = []convMeta{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"conversations": items})
}

func (h *Handler) getConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpx.Error(w, http.StatusBadRequest, "bad id")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	st, err := h.store.State(r.Context(), u.ID, convKey+id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(w, http.StatusNotFound, "no such conversation")
		return
	}
	if err != nil {
		h.internal(w, "read conversation", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(st.Data))
}

func (h *Handler) putConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpx.Error(w, http.StatusBadRequest, "bad id")
		return
	}
	var req struct {
		Title    string    `json:"title"`
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 1<<20) {
		return
	}
	if len(req.Messages) > 400 {
		req.Messages = req.Messages[len(req.Messages)-400:]
	}
	title := strings.TrimSpace(req.Title)
	if r := []rune(title); len(r) > 80 {
		title = string(r[:80])
	}
	u, _ := auth.UserFrom(r.Context())
	now := time.Now()
	b, _ := json.Marshal(map[string]any{"id": id, "title": title, "model": req.Model, "messages": req.Messages, "updated": now.UnixMilli()})
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.store.SetState(r.Context(), u.ID, convKey+id, store.State{Data: string(b), Device: "server", DeviceName: "server", UpdatedAt: now}); err != nil {
		h.internal(w, "save conversation", err)
		return
	}
	items := slices.DeleteFunc(h.index(r.Context(), u.ID), func(c convMeta) bool { return c.ID == id })
	items = append(items, convMeta{ID: id, Title: title, Model: req.Model, Updated: now.UnixMilli()})
	if len(items) > 200 { // keep the newest 200 conversations
		sort.Slice(items, func(i, j int) bool { return items[i].Updated > items[j].Updated })
		for _, old := range items[200:] {
			_ = h.store.DeleteState(r.Context(), u.ID, convKey+old.ID)
		}
		items = items[:200]
	}
	if err := h.saveIndex(r.Context(), u.ID, items); err != nil {
		h.internal(w, "save conversation index", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "updated": now.UnixMilli()})
}

func (h *Handler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpx.Error(w, http.StatusBadRequest, "bad id")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.store.DeleteState(r.Context(), u.ID, convKey+id)
	items := slices.DeleteFunc(h.index(r.Context(), u.ID), func(c convMeta) bool { return c.ID == id })
	if err := h.saveIndex(r.Context(), u.ID, items); err != nil {
		h.internal(w, "save conversation index", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// NewID makes a conversation id (16 hex digits); exported for tests.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
