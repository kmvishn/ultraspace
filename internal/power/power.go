// Package power monitors laptop battery capacity, charging status, AC power, and thermal temperatures from Linux sysfs.
package power

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
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

// Status represents laptop battery and thermal status.
type Status struct {
	HasBattery   bool    `json:"has_battery"`
	Percent      int     `json:"percent"`
	Status       string  `json:"status"` // "Charging", "Discharging", "Full", "Not charging", "AC", "Unknown"
	ACOnline     bool    `json:"ac_online"`
	Health       string  `json:"health"`
	TemperatureC float64 `json:"temperature_c"`
}

// Power monitors power supplies and thermals.
type Power struct {
	log       *slog.Logger
	fs        FS
	sysfsPath string
}

// New creates a Power monitor pointing to /sys/class.
func New(log *slog.Logger) *Power {
	return &Power{
		log:       log,
		fs:        osFS{},
		sysfsPath: "/sys/class",
	}
}

// NewWithFS creates a Power monitor with a custom FS and path for unit testing.
func NewWithFS(log *slog.Logger, f FS, sysfsPath string) *Power {
	return &Power{
		log:       log,
		fs:        f,
		sysfsPath: sysfsPath,
	}
}

// Register wires HTTP endpoints into the router.
func (p *Power) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/power/status", auth(http.HandlerFunc(p.handleStatus)))
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

	return status, nil
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

