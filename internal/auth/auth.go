// Package auth handles first-run setup, login, sessions and the auth middleware.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ultraspace/internal/httpx"
	"ultraspace/internal/store"
)

const cookieName = "ultraspace_session"

const minPasswordLen = 10

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

type ctxKey struct{}

type sessionKey struct{}

// touchEvery limits how often an active session's expiry and last-seen time are written.
const touchEvery = 5 * time.Minute

// UserFrom returns the authenticated user stored by Require.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// SessionFrom returns the session of the current request, stored by Require.
func SessionFrom(ctx context.Context) (store.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(store.Session)
	return s, ok
}

type Options struct {
	TTL            time.Duration
	SetupTokenPath string
	Proxy          httpx.Proxy
	Log            *slog.Logger
}

type Auth struct {
	store     *store.Store
	opts      Options
	limiter   *limiter
	dummyHash []byte
	setupMu   sync.Mutex
	tickets   tickets // sign-ins waiting for the pattern
}

func New(st *store.Store, opts Options) (*Auth, error) {
	// Compared against when the username is unknown, so login timing does not reveal accounts.
	dummy, err := bcrypt.GenerateFromPassword([]byte("ultraspace-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	a := &Auth{store: st, opts: opts, limiter: newLimiter(10, 10*time.Minute), dummyHash: dummy}
	if err := a.ensureSetupToken(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}

// ensureSetupToken writes a one-time token while no user exists, so only someone
// with shell access to the host can claim the first account.
func (a *Auth) ensureSetupToken(ctx context.Context) error {
	n, err := a.store.UserCount(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		if err := os.Remove(a.opts.SetupTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if _, err := os.Stat(a.opts.SetupTokenPath); err == nil {
		a.opts.Log.Warn("setup required; read the token file to create the first account", "token_file", a.opts.SetupTokenPath)
		return nil
	}
	if err := os.WriteFile(a.opts.SetupTokenPath, []byte(randomToken(18)+"\n"), 0o600); err != nil {
		return err
	}
	a.opts.Log.Warn("setup required; read the token file to create the first account", "token_file", a.opts.SetupTokenPath)
	return nil
}

func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", a.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.Handle("GET /api/me", a.Require(http.HandlerFunc(a.handleMe)))
	mux.Handle("POST /api/account/password", a.Require(http.HandlerFunc(a.handleChangePassword)))
	mux.Handle("GET /api/account/settings", a.Require(http.HandlerFunc(a.handleGetSettings)))
	mux.Handle("PUT /api/account/settings", a.Require(http.HandlerFunc(a.handlePutSettings)))
	mux.Handle("GET /api/account/sessions", a.Require(http.HandlerFunc(a.handleSessions)))
	a.registerPattern(mux)
	mux.Handle("POST /api/account/sessions/revoke", a.Require(http.HandlerFunc(a.handleRevoke)))
}

func (a *Auth) handleSessions(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	cur, _ := SessionFrom(r.Context())
	list, err := a.store.Sessions(r.Context(), u.ID)
	if err != nil {
		a.internal(w, "list sessions", err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]any{
			"id": s.ID, "current": s.ID == cur.ID, "created": s.CreatedAt.UnixMilli(),
			"last_seen": s.LastSeen.UnixMilli(), "expires": s.ExpiresAt.UnixMilli(),
			"ip": s.IP, "user_agent": s.UserAgent,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleRevoke signs out one session ({"id": n}) or every other one ({"others": true}).
func (a *Auth) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64 `json:"id"`
		Others bool  `json:"others"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	u, _ := UserFrom(r.Context())
	cur, _ := SessionFrom(r.Context())
	var err error
	switch {
	case req.Others:
		err = a.store.DeleteOtherSessions(r.Context(), u.ID, cur.ID)
	case req.ID > 0:
		err = a.store.DeleteSessionByID(r.Context(), u.ID, req.ID)
	default:
		httpx.Error(w, http.StatusBadRequest, "give a session id or others")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		a.internal(w, "revoke session", err)
		return
	}
	if req.ID == cur.ID {
		http.SetCookie(w, a.cookie(r, "", -1))
	}
	a.opts.Log.Info("sessions revoked", "user", u.Username, "id", req.ID, "others", req.Others)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Auth) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	data, err := a.store.Settings(r.Context(), u.ID)
	if err != nil {
		a.internal(w, "read settings", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(data))
}

// handlePutSettings stores an opaque JSON object of UI preferences (theme, wallpaper, ...).
func (a *Auth) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var obj map[string]json.RawMessage
	if !httpx.DecodeJSONLimit(w, r, &obj, 16<<10) {
		return
	}
	data, err := json.Marshal(obj)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid settings")
		return
	}
	u, _ := UserFrom(r.Context())
	if err := a.store.SetSettings(r.Context(), u.ID, string(data)); err != nil {
		a.internal(w, "save settings", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Require rejects requests without a valid session and stores the user in the context.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" {
			httpx.Error(w, http.StatusUnauthorized, "not signed in")
			return
		}
		u, sess, err := a.store.SessionUser(r.Context(), hashToken(c.Value))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				a.opts.Log.Error("session lookup", "err", err)
			}
			httpx.Error(w, http.StatusUnauthorized, "not signed in")
			return
		}
		// Sliding expiry: a session in use stays valid; only TTL of inactivity signs you out.
		if time.Since(sess.LastSeen) > touchEvery {
			if err := a.store.TouchSession(r.Context(), sess.ID, time.Now().Add(a.opts.TTL), a.opts.Proxy.ClientIP(r)); err != nil {
				a.opts.Log.Error("touch session", "err", err)
			} else {
				http.SetCookie(w, a.cookie(r, c.Value, int(a.opts.TTL.Seconds())))
			}
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, u)
		ctx = context.WithValue(ctx, sessionKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PurgeLoop removes expired sessions until ctx is done.
func (a *Auth) PurgeLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := a.store.PurgeExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			a.opts.Log.Error("purge sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Auth) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.UserCount(r.Context())
	if err != nil {
		a.internal(w, "count users", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"required": n == 0})
}

func (a *Auth) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ip := a.opts.Proxy.ClientIP(r)
	if !a.limiter.allowed(ip) {
		httpx.Error(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}

	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	n, err := a.store.UserCount(r.Context())
	if err != nil {
		a.internal(w, "count users", err)
		return
	}
	if n > 0 {
		httpx.Error(w, http.StatusConflict, "setup is already complete")
		return
	}
	want, err := os.ReadFile(a.opts.SetupTokenPath)
	if err != nil {
		a.internal(w, "read setup token", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(strings.TrimSpace(string(want)))) != 1 {
		a.limiter.fail(ip)
		httpx.Error(w, http.StatusForbidden, "invalid setup token")
		return
	}
	if msg := validateCredentials(req.Username, req.Password); msg != "" {
		httpx.Error(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.internal(w, "hash password", err)
		return
	}
	u, err := a.store.CreateUser(r.Context(), req.Username, string(hash))
	if err != nil {
		a.internal(w, "create user", err)
		return
	}
	if err := os.Remove(a.opts.SetupTokenPath); err != nil {
		a.opts.Log.Error("remove setup token", "err", err)
	}
	a.opts.Log.Info("setup complete", "user", u.Username, "ip", ip)
	if !a.startSession(w, r, u) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, userJSON(u))
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ip := a.opts.Proxy.ClientIP(r)
	if !a.limiter.allowed(ip) {
		httpx.Error(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	u, err := a.store.UserByName(r.Context(), req.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.internal(w, "find user", err)
		return
	}
	hash := a.dummyHash
	if err == nil {
		hash = []byte(u.PasswordHash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) != nil || err != nil {
		a.limiter.fail(ip)
		a.opts.Log.Warn("login failed", "user", req.Username, "ip", ip)
		httpx.Error(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	a.limiter.reset(ip)
	if a.needsPattern(w, r, u) {
		return // the session comes after the pattern
	}
	if !a.startSession(w, r, u) {
		return
	}
	a.opts.Log.Info("login", "user", u.Username, "ip", ip)
	httpx.WriteJSON(w, http.StatusOK, userJSON(u))
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if err := a.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			a.opts.Log.Error("delete session", "err", err)
		}
	}
	http.SetCookie(w, a.cookie(r, "", -1))
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Auth) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, userJSON(u))
}

func (a *Auth) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
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
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Current)) != nil {
		a.limiter.fail(ip)
		httpx.Error(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if msg := validateCredentials(u.Username, req.New); msg != "" {
		httpx.Error(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		a.internal(w, "hash password", err)
		return
	}
	if err := a.store.SetPassword(r.Context(), u.ID, string(hash)); err != nil {
		a.internal(w, "set password", err)
		return
	}
	// Sign out every other device, then give this browser a fresh session.
	if err := a.store.DeleteUserSessions(r.Context(), u.ID); err != nil {
		a.internal(w, "revoke sessions", err)
		return
	}
	if !a.startSession(w, r, u) {
		return
	}
	a.opts.Log.Info("password changed", "user", u.Username, "ip", ip)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Auth) startSession(w http.ResponseWriter, r *http.Request, u store.User) bool {
	token := randomToken(32)
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	ip := a.opts.Proxy.ClientIP(r)
	if err := a.store.CreateSession(r.Context(), hashToken(token), u.ID, time.Now().Add(a.opts.TTL), ip, ua); err != nil {
		a.internal(w, "create session", err)
		return false
	}
	http.SetCookie(w, a.cookie(r, token, int(a.opts.TTL.Seconds())))
	return true
}

func (a *Auth) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.opts.Proxy.IsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	}
}

func (a *Auth) internal(w http.ResponseWriter, what string, err error) {
	a.opts.Log.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

func validateCredentials(username, password string) string {
	if !usernameRE.MatchString(username) {
		return "username must be 1-32 characters: letters, digits, '.', '_' or '-'"
	}
	if len(password) < minPasswordLen {
		return "password must be at least 10 characters"
	}
	if len(password) > 72 {
		return "password must be at most 72 bytes"
	}
	return ""
}

func userJSON(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "pattern": u.PatternHash != ""}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// limiter counts failed attempts per client IP within a sliding window.
type limiter struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{fails: map[string][]time.Time{}, max: max, window: window}
}

func (l *limiter) recent(ip string, now time.Time) []time.Time {
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

func (l *limiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip, time.Now())) < l.max
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.fails) > 10000 {
		for k := range l.fails {
			l.recent(k, now)
		}
	}
	l.fails[ip] = append(l.recent(ip, now), now)
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
