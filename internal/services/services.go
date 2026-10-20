// Package services exposes read-only systemd unit status and journal logs.
package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ultraspace/internal/httpx"
)

var unitRE = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}\.service$`)

type Services struct{ log *slog.Logger }

func New(log *slog.Logger) *Services { return &Services{log: log} }

func (s *Services) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/services", auth(http.HandlerFunc(s.handleList)))
	mux.Handle("GET /api/logs", auth(http.HandlerFunc(s.handleLogs)))
}

type Unit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
}

func (s *Services) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--all",
		"--no-pager", "--plain", "--output=json").Output()
	if err != nil {
		s.log.Error("systemctl list-units", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "cannot list services")
		return
	}
	var raw []struct {
		Unit        string `json:"unit"`
		Load        string `json:"load"`
		Active      string `json:"active"`
		Sub         string `json:"sub"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		s.log.Error("parse systemctl output", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "cannot list services")
		return
	}
	units := make([]Unit, 0, len(raw))
	for _, u := range raw {
		if u.Load == "not-found" {
			continue
		}
		units = append(units, Unit{Name: u.Unit, Description: u.Description, Load: u.Load, Active: u.Active, Sub: u.Sub})
	}
	sort.Slice(units, func(i, j int) bool { return strings.ToLower(units[i].Name) < strings.ToLower(units[j].Name) })
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"units": units})
}

type Entry struct {
	Time     int64  `json:"t"` // unix milliseconds
	Priority int    `json:"priority"`
	Unit     string `json:"unit,omitempty"`
	Ident    string `json:"ident,omitempty"`
	Message  string `json:"message"`
}

func (s *Services) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lines := 200
	if n, err := strconv.Atoi(q.Get("lines")); err == nil && n > 0 && n <= 2000 {
		lines = n
	}
	args := []string{"--no-pager", "--output=json", "--lines=" + strconv.Itoa(lines)}
	if unit := q.Get("unit"); unit != "" {
		if !unitRE.MatchString(unit) {
			httpx.Error(w, http.StatusBadRequest, "invalid unit name")
			return
		}
		args = append(args, "--unit="+unit)
	}
	if p := q.Get("priority"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 7 {
			httpx.Error(w, http.StatusBadRequest, "invalid priority")
			return
		}
		args = append(args, "--priority="+p)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		s.log.Error("journalctl", "err", err, "stderr", strings.TrimSpace(stderr.String()))
		httpx.Error(w, http.StatusInternalServerError, "cannot read logs")
		return
	}
	entries := []Entry{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var raw map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		e := Entry{Message: field(raw["MESSAGE"]), Ident: field(raw["SYSLOG_IDENTIFIER"]), Unit: field(raw["_SYSTEMD_UNIT"])}
		if us, err := strconv.ParseInt(field(raw["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
			e.Time = us / 1000
		}
		e.Priority = 6
		if p, err := strconv.Atoi(field(raw["PRIORITY"])); err == nil {
			e.Priority = p
		}
		entries = append(entries, e)
	}
	// journalctl hides other users' entries without the systemd-journal group; say so instead of looking empty.
	note := ""
	if strings.Contains(stderr.String(), "not seeing messages") {
		note = "Some logs are hidden: the ultraspace service user is not in the systemd-journal group."
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries, "note": note})
}

// field decodes a journal JSON field, which is a string, or an array of bytes for binary data.
func field(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var nums []int
	if json.Unmarshal(raw, &nums) == nil {
		for _, n := range nums {
			b = append(b, byte(n))
		}
		return strings.ToValidUTF8(string(b), "�")
	}
	return ""
}
