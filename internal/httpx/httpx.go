// Package httpx holds small HTTP helpers shared by the API packages.
package httpx

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
)

// Proxy decides how much to trust forwarded headers.
type Proxy struct{ Trust bool }

func (p Proxy) fromProxy(r *http.Request) bool {
	if !p.Trust {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p Proxy) ClientIP(r *http.Request) string {
	if p.fromProxy(r) {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (p Proxy) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return p.fromProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// Origin is the origin the browser used to reach us.
func (p Proxy) Origin(r *http.Request) string {
	if p.IsHTTPS(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Error(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// DecodeJSON reads a small JSON body (up to 1 MiB) into v and writes a 400 on failure.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeJSONLimit(w, r, v, 1<<20)
}

// DecodeJSONLimit is DecodeJSON with an explicit body size limit in bytes.
func DecodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			Error(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			Error(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}
