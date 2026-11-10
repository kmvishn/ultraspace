// Package power monitors laptop battery capacity, charging status, AC power, and thermal temperatures from Linux sysfs,
// and provides safe system reboot, shutdown, and service restart controls.
package power

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ultraspace/internal/httpx"
)

// FS abstracts filesystem reads for testing sysfs.
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(name)
}

// Runner abstracts external commands execution for testability.
type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Capabilities describes what power actions are permitted on this machine.
type Capabilities struct {
	CanShutdown       bool   `json:"can_shutdown"`
	CanReboot         bool   `json:"can_reboot"`
	CanRestartService bool   `json:"can_restart_service"`
	Environment       string `json:"environment"`  // "laptop", "physical", or "vps"
	Virtualization    string `json:"virtualization"` // "none", "kvm", "qemu", etc.
}

// Status represents laptop battery, thermal status, and host environment capabilities.
type Status struct {
	HasBattery   bool         `json:"has_battery"`
	Percent      int          `json:"percent"`
	Status       string       `json:"status"` // "Charging", "Discharging", "Full", "Not charging", "AC", "Unknown"
	ACOnline     bool         `json:"ac_online"`
	Health       string       `json:"health"`
	TemperatureC float64      `json:"temperature_c"`
	Capabilities Capabilities `json:"capabilities"`
}

// Power monitors power supplies, thermals, and executes reboot/shutdown.
type Power struct {
	log       *slog.Logger
	fs        FS
	runner    Runner
	sysfsPath string
}

// New creates a Power monitor pointing to /sys/class.
func New(log *slog.Logger) *Power {
	return &Power{
		log:       log,
		fs:        osFS{},
		runner:    execRunner{},
		sysfsPath: "/sys/class",
	}
}

// NewWithFS creates a Power monitor with a custom FS and path for unit testing.
func NewWithFS(log *slog.Logger, f FS, sysfsPath string) *Power {
	return NewWithFSAndRunner(log, f, execRunner{}, sysfsPath)
}

// NewWithFSAndRunner creates a Power monitor with custom FS and runner for unit testing.
func NewWithFSAndRunner(log *slog.Logger, f FS, r Runner, sysfsPath string) *Power {
	return &Power{
		log:       log,
		fs:        f,
		runner:    r,
		sysfsPath: sysfsPath,
	}
}

// Register wires HTTP endpoints into the router.
func (p *Power) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/power/status", auth(http.HandlerFunc(p.handleStatus)))
	mux.Handle("POST /api/power/reboot", auth(http.HandlerFunc(p.handleReboot)))
	mux.Handle("POST /api/power/shutdown", auth(http.HandlerFunc(p.handleShutdown)))
	mux.Handle("POST /api/power/restart-service", auth(http.HandlerFunc(p.handleRestartService)))
}

func (p *Power) readInt(path string) (int, error) {
	b, err := p.fs.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	return strconv.Atoi(s)
}

func (p *Power) readString(path string) string {
	b, err := p.fs.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// GetStatus reads battery, AC power, and thermal state from sysfs.
func (p *Power) GetStatus(ctx context.Context) (*Status, error) {
	status := &Status{
		HasBattery: false,
		Percent:    100,
		Status:     "AC",
		ACOnline:   true,
		Health:     "Good",
	}

	powerDir := filepath.Join(p.sysfsPath, "power_supply")
	entries, err := p.fs.ReadDir(powerDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			entryPath := filepath.Join(powerDir, name)

			// Read power supply type from sysfs (e.g. Battery, Mains, USB)
			psType := strings.TrimSpace(p.readString(filepath.Join(entryPath, "type")))

			// AC / Charger check
			if psType == "Mains" || psType == "USB" || psType == "Wireless" ||
				strings.HasPrefix(name, "AC") || strings.HasPrefix(name, "ADP") || strings.HasPrefix(name, "Mains") {
				online, err := p.readInt(filepath.Join(entryPath, "online"))
				if err == nil {
					status.ACOnline = (online == 1)
				}
			}

			// Battery check
			if psType == "Battery" || strings.HasPrefix(name, "BAT") || strings.HasPrefix(name, "battery") {
				capVal, err := p.readInt(filepath.Join(entryPath, "capacity"))
				if err == nil {
					status.HasBattery = true
					status.Percent = capVal
				}

				batStatus := p.readString(filepath.Join(entryPath, "status"))
				if batStatus != "" {
					status.Status = batStatus
				}

				batHealth := p.readString(filepath.Join(entryPath, "health"))
				if batHealth != "" {
					status.Health = batHealth
				}
			}
		}
	}

	// Thermal zones check
	thermalDir := filepath.Join(p.sysfsPath, "thermal")
	tEntries, err := p.fs.ReadDir(thermalDir)
	if err == nil {
		var maxTemp float64
		for _, te := range tEntries {
			if strings.HasPrefix(te.Name(), "thermal_zone") {
				tempMilli, err := p.readInt(filepath.Join(thermalDir, te.Name(), "temp"))
				if err == nil && tempMilli > 0 {
					tempC := float64(tempMilli)
					if tempMilli > 1000 {
						tempC = float64(tempMilli) / 1000.0
					}
					if tempC > maxTemp && tempC < 125 { // filter out absurd sensor readings
						maxTemp = tempC
					}
				}
			}
		}
		status.TemperatureC = maxTemp
	}

	status.Capabilities = p.GetCapabilities(ctx, status.HasBattery)
	return status, nil
}

func (p *Power) detectVirtualization(ctx context.Context) string {
	if p.runner == nil {
		return "none"
	}
	out, err := p.runner.CombinedOutput(ctx, "systemd-detect-virt")
	if err != nil {
		return "none"
	}
	v := strings.TrimSpace(string(out))
	if v == "" || v == "none" {
		return "none"
	}
	return v
}

// GetCapabilities determines allowed power controls based on hardware vs VPS environment.
func (p *Power) GetCapabilities(ctx context.Context, hasBattery bool) Capabilities {
	virt := p.detectVirtualization(ctx)
	caps := Capabilities{
		CanReboot:         true,
		CanRestartService: true,
		Virtualization:    virt,
	}

	if hasBattery {
		caps.Environment = "laptop"
		caps.CanShutdown = true
	} else if virt == "none" {
		caps.Environment = "physical"
		caps.CanShutdown = true
	} else {
		// VPS / cloud instance: allow reboot/restart, prevent accidental poweroff lockout
		caps.Environment = "vps"
		caps.CanShutdown = false
	}
	return caps
}

// Reboot restarts the host system.
func (p *Power) Reboot(ctx context.Context) error {
	p.log.Info("system reboot initiated via ultraspace API")
	go func() {
		time.Sleep(800 * time.Millisecond)
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := p.runner.CombinedOutput(bgCtx, "systemctl", "reboot"); err == nil {
			return
		}
		if _, err := p.runner.CombinedOutput(bgCtx, "shutdown", "-r", "now"); err == nil {
			return
		}
		_, _ = p.runner.CombinedOutput(bgCtx, "reboot")
	}()
	return nil
}

// Shutdown powers off the host system if permitted.
func (p *Power) Shutdown(ctx context.Context) error {
	caps := p.GetCapabilities(ctx, false)
	st, _ := p.GetStatus(ctx)
	if st != nil && st.HasBattery {
		caps.CanShutdown = true
	}
	if !caps.CanShutdown {
		return errors.New("system shutdown is disabled on VPS environments to prevent remote host lock-out")
	}

	p.log.Info("system poweroff initiated via ultraspace API")
	go func() {
		time.Sleep(800 * time.Millisecond)
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := p.runner.CombinedOutput(bgCtx, "systemctl", "poweroff"); err == nil {
			return
		}
		if _, err := p.runner.CombinedOutput(bgCtx, "shutdown", "-h", "now"); err == nil {
			return
		}
		_, _ = p.runner.CombinedOutput(bgCtx, "poweroff")
	}()
	return nil
}

// RestartService restarts the ultraspace systemd service.
func (p *Power) RestartService(ctx context.Context) error {
	p.log.Info("ultraspace service restart initiated via API")
	go func() {
		time.Sleep(800 * time.Millisecond)
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = p.runner.CombinedOutput(bgCtx, "systemctl", "restart", "ultraspace.service")
	}()
	return nil
}

func (p *Power) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	st, err := p.GetStatus(ctx)
	if err != nil {
		p.log.Error("get power status", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to get power status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (p *Power) handleReboot(w http.ResponseWriter, r *http.Request) {
	if err := p.Reboot(r.Context()); err != nil {
		p.log.Error("reboot failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "rebooting", "message": "System is rebooting…"})
}

func (p *Power) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if err := p.Shutdown(r.Context()); err != nil {
		p.log.Warn("shutdown rejected", "err", err)
		httpx.Error(w, http.StatusForbidden, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "shutting_down", "message": "System is powering off…"})
}

func (p *Power) handleRestartService(w http.ResponseWriter, r *http.Request) {
	if err := p.RestartService(r.Context()); err != nil {
		p.log.Error("restart service failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "restarting", "message": "Ultraspace service is restarting…"})
}

