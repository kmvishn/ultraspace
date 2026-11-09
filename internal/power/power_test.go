package power

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mockDirEntry struct {
	name  string
	isDir bool
}

func (m mockDirEntry) Name() string               { return m.name }
func (m mockDirEntry) IsDir() bool                { return m.isDir }
func (m mockDirEntry) Type() fs.FileMode          { return 0 }
func (m mockDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

type mockFS struct {
	files map[string][]byte
}

func (m *mockFS) ReadFile(name string) ([]byte, error) {
	norm := filepath.ToSlash(name)
	b, ok := m.files[norm]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (m *mockFS) ReadDir(name string) ([]fs.DirEntry, error) {
	norm := filepath.ToSlash(name)
	if !strings.HasSuffix(norm, "/") {
		norm += "/"
	}

	seen := make(map[string]bool)
	var entries []fs.DirEntry

	for path := range m.files {
		if strings.HasPrefix(path, norm) {
			rel := strings.TrimPrefix(path, norm)
			parts := strings.Split(rel, "/")
			entryName := parts[0]
			if !seen[entryName] {
				seen[entryName] = true
				entries = append(entries, mockDirEntry{
					name:  entryName,
					isDir: len(parts) > 1,
				})
			}
		}
	}
	return entries, nil
}

func TestGetStatusWithBattery(t *testing.T) {
	fs := &mockFS{
		files: map[string][]byte{
			"/sys/class/power_supply/AC/online":              []byte("1\n"),
			"/sys/class/power_supply/BAT0/capacity":          []byte("82\n"),
			"/sys/class/power_supply/BAT0/status":            []byte("Charging\n"),
			"/sys/class/power_supply/BAT0/health":            []byte("Good\n"),
			"/sys/class/thermal/thermal_zone0/temp":          []byte("48500\n"),
		},
	}

	p := NewWithFS(slog.Default(), fs, "/sys/class")
	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.HasBattery {
		t.Fatal("expected HasBattery to be true")
	}
	if st.Percent != 82 {
		t.Errorf("Percent = %d, want 82", st.Percent)
	}
	if st.Status != "Charging" {
		t.Errorf("Status = %q, want Charging", st.Status)
	}
	if !st.ACOnline {
		t.Error("expected ACOnline to be true")
	}
	if st.Health != "Good" {
		t.Errorf("Health = %q, want Good", st.Health)
	}
	if st.TemperatureC != 48.5 {
		t.Errorf("TemperatureC = %f, want 48.5", st.TemperatureC)
	}
}

func TestGetStatusDischarging(t *testing.T) {
	fs := &mockFS{
		files: map[string][]byte{
			"/sys/class/power_supply/ADP1/online":            []byte("0\n"),
			"/sys/class/power_supply/BAT1/capacity":          []byte("34\n"),
			"/sys/class/power_supply/BAT1/status":            []byte("Discharging\n"),
			"/sys/class/power_supply/BAT1/health":            []byte("Good\n"),
			"/sys/class/thermal/thermal_zone0/temp":          []byte("52000\n"),
		},
	}

	p := NewWithFS(slog.Default(), fs, "/sys/class")
	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.HasBattery {
		t.Fatal("expected HasBattery to be true")
	}
	if st.Percent != 34 {
		t.Errorf("Percent = %d, want 34", st.Percent)
	}
	if st.ACOnline {
		t.Error("expected ACOnline to be false (discharging)")
	}
}

func TestGetStatusNoBattery(t *testing.T) {
	fs := &mockFS{
		files: map[string][]byte{},
	}

	p := NewWithFS(slog.Default(), fs, "/sys/class")
	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if st.HasBattery {
		t.Error("expected HasBattery to be false")
	}
	if !st.ACOnline {
		t.Error("expected ACOnline to be true on desktop")
	}
}

func TestPowerHTTPHandler(t *testing.T) {
	fs := &mockFS{
		files: map[string][]byte{
			"/sys/class/power_supply/BAT0/capacity": []byte("95\n"),
			"/sys/class/power_supply/BAT0/status":   []byte("Full\n"),
		},
	}

	p := NewWithFS(slog.Default(), fs, "/sys/class")
	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	p.Register(mux, passAuth)

	req := httptest.NewRequest("GET", "/api/power/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var st Status
	if err := json.NewDecoder(w.Body).Decode(&st); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !st.HasBattery || st.Percent != 95 || st.Status != "Full" {
		t.Errorf("unexpected status: %+v", st)
	}
}
