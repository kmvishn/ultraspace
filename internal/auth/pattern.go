package auth

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

// Optional pattern lock: a 3x3 pattern drawn after the password. With it on, a correct
// password only earns a short-lived ticket; the session is created once the pattern
// matches. A ticket allows a few tries, then the password must be entered again.

const (
	ticketTTL      = 2 * time.Minute
	patternTries   = 5
	minPatternDots = 4
)

type ticket struct {
	userID  int64
	ip      string
	expires time.Time
	tries   int
}

type tickets struct {
	mu sync.Mutex
	m  map[string]*ticket
}

func (t *tickets) add(userID int64, ip string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*ticket{}
	}
	now := time.Now()
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	id := randomToken(24)
	t.m[id] = &ticket{userID: userID, ip: ip, expires: now.Add(ticketTTL)}
	return id
}

// normalizePattern checks a pattern given as dot numbers 0-8 ("0,4,8,5") and returns
// its canonical form, or "" if it is not a valid pattern.
func normalizePattern(s string) string {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) < minPatternDots || len(parts) > 9 {
		return ""
	}
	seen := map[int]bool{}
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 8 || seen[n] {
			return ""
		}
		seen[n] = true
	}
	return strings.Join(parts, ",")
}

func (a *Auth) registerPattern(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login/pattern", a.handlePatternLogin)
	mux.Handle("POST /api/account/pattern", a.Require(http.HandlerFunc(a.handleSetPattern)))
}

// handlePatternLogin finishes a sign-in that needs the pattern.
func (a *Auth) handlePatternLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ticket  string `json:"ticket"`
		Pattern string `json:"pattern"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ip := a.opts.Proxy.ClientIP(r)
	if !a.limiter.allowed(ip) {
		httpx.Error(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	a.tickets.mu.Lock()
	t := a.tickets.m[req.Ticket]
	if t != nil && (time.Now().After(t.expires) || t.ip != ip) {
		delete(a.tickets.m, req.Ticket)
		t = nil
	}
	a.tickets.mu.Unlock()
	if t == nil {
		httpx.Error(w, http.StatusUnauthorized, "that sign-in expired; enter your password again")
		return
	}
	u, err := a.store.UserByID(r.Context(), t.userID)
	if err != nil || u.PatternHash == "" {
		httpx.Error(w, http.StatusUnauthorized, "that sign-in expired; enter your password again")
		return
	}
	p := normalizePattern(req.Pattern)
	if p == "" || bcrypt.CompareHashAndPassword([]byte(u.PatternHash), []byte(p)) != nil {
		a.limiter.fail(ip)
		a.tickets.mu.Lock()
		t.tries++
		left := patternTries - t.tries
		if left <= 0 {
			delete(a.tickets.m, req.Ticket)
		}
		a.tickets.mu.Unlock()
		a.opts.Log.Warn("wrong pattern", "user", u.Username, "ip", ip)
		if left <= 0 {
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "too many wrong patterns; enter your password again", "restart": true})
			return
		}
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong pattern", "tries_left": left})
		return
	}
	a.tickets.mu.Lock()
	delete(a.tickets.m, req.Ticket)
	a.tickets.mu.Unlock()
	a.limiter.reset(ip)
	if !a.startSession(w, r, u) {
		return
	}
	a.opts.Log.Info("login", "user", u.Username, "ip", ip, "pattern", true)
	httpx.WriteJSON(w, http.StatusOK, userJSON(u))
}

// handleSetPattern turns the pattern lock on, changes it, or (with an empty pattern)
// turns it off. The current password is required either way.
func (a *Auth) handleSetPattern(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Pattern  string `json:"pattern"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	u, _ := UserFrom(r.Context())
	ip := a.opts.Proxy.ClientIP(r)
	if !a.limiter.allowed(ip) {
		httpx.Error(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		a.limiter.fail(ip)
		httpx.Error(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	hash := ""
	if req.Pattern != "" {
		p := normalizePattern(req.Pattern)
		if p == "" {
			httpx.Error(w, http.StatusBadRequest, "a pattern joins at least 4 different dots")
			return
		}
		b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if err != nil {
			a.internal(w, "hash pattern", err)
			return
		}
		hash = string(b)
	}
	if err := a.store.SetPattern(r.Context(), u.ID, hash); err != nil {
		a.internal(w, "save pattern", err)
		return
	}
	a.opts.Log.Info("pattern lock changed", "user", u.Username, "on", hash != "")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"pattern": hash != ""})
}

// needsPattern is used by the password step: with a pattern set, it answers with a
// ticket instead of a session.
func (a *Auth) needsPattern(w http.ResponseWriter, r *http.Request, u store.User) bool {
	if u.PatternHash == "" {
		return false
	}
	t := a.tickets.add(u.ID, a.opts.Proxy.ClientIP(r))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pattern_required": true, "ticket": t})
	return true
}
