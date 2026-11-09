package auth

import (
	"net/http"
	"regexp"

	"golang.org/x/crypto/bcrypt"

	"ultraspace/internal/httpx"
)

var pinRE = regexp.MustCompile(`^[0-9]{4,12}$`)

func (a *Auth) registerPIN(mux *http.ServeMux) {
	mux.Handle("POST /api/account/pin", a.Require(http.HandlerFunc(a.handleSetPIN)))
	mux.Handle("POST /api/account/unlock", a.Require(http.HandlerFunc(a.handleUnlock)))
}

// handleSetPIN turns the lockscreen PIN on, changes it, or (with an empty PIN)
// turns it off. The current password is required either way.
func (a *Auth) handleSetPIN(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		PIN      string `json:"pin"`
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
	if req.PIN != "" {
		if !pinRE.MatchString(req.PIN) {
			httpx.Error(w, http.StatusBadRequest, "a PIN must be between 4 and 12 digits (numbers only)")
			return
		}
		b, err := bcrypt.GenerateFromPassword([]byte(req.PIN), bcrypt.DefaultCost)
		if err != nil {
			a.internal(w, "hash pin", err)
			return
		}
		hash = string(b)
	}
	if err := a.store.SetPIN(r.Context(), u.ID, hash); err != nil {
		a.internal(w, "save pin", err)
		return
	}
	a.opts.Log.Info("lockscreen PIN changed", "user", u.Username, "on", hash != "")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"pin": hash != ""})
}

// handleUnlock verifies either the lockscreen PIN or the master password to unlock the unattended screen.
func (a *Auth) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PIN      string `json:"pin"`
		Password string `json:"password"`
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

	// Try PIN first if provided
	if req.PIN != "" {
		if u.PINHash == "" {
			httpx.Error(w, http.StatusBadRequest, "no lockscreen PIN configured")
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(u.PINHash), []byte(req.PIN)) != nil {
			a.limiter.fail(ip)
			a.opts.Log.Warn("unlock failed: incorrect PIN", "user", u.Username, "ip", ip)
			httpx.Error(w, http.StatusUnauthorized, "incorrect PIN")
			return
		}
		a.limiter.reset(ip)
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Master password fallback
	if req.Password != "" {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
			a.limiter.fail(ip)
			a.opts.Log.Warn("unlock failed: incorrect password", "user", u.Username, "ip", ip)
			httpx.Error(w, http.StatusUnauthorized, "incorrect password")
			return
		}
		a.limiter.reset(ip)
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	httpx.Error(w, http.StatusBadRequest, "PIN or password required")
}
