package power

import (
	"context"
	"encoding/json"
	"errors"
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

func TestGetStatusPMICBattery(t *testing.T) {
	fs := &mockFS{
		files: map[string][]byte{
			"/sys/class/power_supply/axp288_charger/type":     []byte("USB\n"),
			"/sys/class/power_supply/axp288_charger/online":   []byte("1\n"),
			"/sys/class/power_supply/axp288_fuel_gauge/type":  []byte("Battery\n"),
			"/sys/class/power_supply/axp288_fuel_gauge/capacity": []byte("42\n"),
			"/sys/class/power_supply/axp288_fuel_gauge/status":   []byte("Discharging\n"),
			"/sys/class/power_supply/axp288_fuel_gauge/health":   []byte("Good\n"),
			"/sys/class/thermal/thermal_zone0/temp":           []byte("56000\n"),
		},
	}

	p := NewWithFS(slog.Default(), fs, "/sys/class")
	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus err: %v", err)
	}

	if !st.HasBattery {
		t.Fatal("expected HasBattery to be true for PMIC fuel gauge")
	}
	if st.Percent != 42 {
		t.Errorf("Percent = %d, want 42", st.Percent)
	}
	if st.Status != "Discharging" {
		t.Errorf("Status = %q, want Discharging", st.Status)
	}
	if !st.ACOnline {
		t.Error("expected ACOnline to be true")
	}
	if st.Health != "Good" {
		t.Errorf("Health = %q, want Good", st.Health)
	}
	if st.TemperatureC != 56.0 {
		t.Errorf("TemperatureC = %f, want 56.0", st.TemperatureC)
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

type mockRunner struct {
	commands []string
	virtOut  string
}

func (m *mockRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmdStr := name + " " + strings.Join(args, " ")
	m.commands = append(m.commands, cmdStr)
	if name == "systemd-detect-virt" {
		if m.virtOut == "" || m.virtOut == "none" {
			return []byte("none\n"), errors.New("exit status 1")
		}
		return []byte(m.virtOut + "\n"), nil
	}
	return []byte("ok"), nil
}

func TestCapabilities(t *testing.T) {
	runner := &mockRunner{virtOut: "kvm"}
	p := NewWithFSAndRunner(slog.Default(), &mockFS{files: map[string][]byte{}}, runner, "/sys/class")

	// 1. VPS with no battery: can reboot, but cannot shutdown
	caps := p.GetCapabilities(context.Background(), false)
	if caps.Environment != "vps" {
		t.Errorf("expected Environment = vps, got %s", caps.Environment)
	}
	if !caps.CanReboot {
		t.Error("expected CanReboot = true on VPS")
	}
	if caps.CanShutdown {
		t.Error("expected CanShutdown = false on VPS")
	}

	// 2. Laptop (has battery): can both reboot and shutdown
	capsLaptop := p.GetCapabilities(context.Background(), true)
	if capsLaptop.Environment != "laptop" {
		t.Errorf("expected Environment = laptop, got %s", capsLaptop.Environment)
	}
	if !capsLaptop.CanReboot || !capsLaptop.CanShutdown {
		t.Error("expected CanReboot and CanShutdown to be true on laptop")
	}

	// 3. Physical server (no virt, no battery): can both reboot and shutdown
	runner.virtOut = "none"
	capsPhysical := p.GetCapabilities(context.Background(), false)
	if capsPhysical.Environment != "physical" {
		t.Errorf("expected Environment = physical, got %s", capsPhysical.Environment)
	}
	if !capsPhysical.CanReboot || !capsPhysical.CanShutdown {
		t.Error("expected CanReboot and CanShutdown to be true on physical machine")
	}
}

func TestPowerActionEndpoints(t *testing.T) {
	runner := &mockRunner{virtOut: "none"}
	fs := &mockFS{files: map[string][]byte{}}
	p := NewWithFSAndRunner(slog.Default(), fs, runner, "/sys/class")

	mux := http.NewServeMux()
	passAuth := func(h http.Handler) http.Handler { return h }
	p.Register(mux, passAuth)

	// 1. Reboot
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/power/reboot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reboot code = %d, want 200", rec.Code)
	}

	// 2. Shutdown (Physical allowed)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/power/shutdown", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("shutdown code = %d, want 200", rec.Code)
	}

	// 3. Restart Service
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/power/restart-service", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart-service code = %d, want 200", rec.Code)
	}

	// 4. Shutdown on VPS rejected with 403
	runner.virtOut = "kvm"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/power/shutdown", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("vps shutdown code = %d, want 403", rec.Code)
	}
}
