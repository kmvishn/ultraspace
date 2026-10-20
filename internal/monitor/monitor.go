// Package monitor reports host statistics read from /proc. Linux only.
package monitor

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ultraspace/internal/httpx"
)

const (
	sampleEvery = 2 * time.Second
	historyLen  = 180 // 6 minutes of samples
)

// Point is one history sample for the charts.
type Point struct {
	T   int64   `json:"t"` // unix milliseconds
	CPU float64 `json:"cpu"`
	Mem float64 `json:"mem"` // percent
	Rx  float64 `json:"rx"`  // bytes/s
	Tx  float64 `json:"tx"`  // bytes/s
}

// Disk is a mount point to report, labelled for the UI.
type Disk struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type Monitor struct {
	version string
	started time.Time
	disks   []Disk

	mu       sync.Mutex
	cpuPrev  cpuTimes
	netPrev  netTotals
	netAt    time.Time
	cpuPct   float64
	rxRate   float64
	txRate   float64
	netTotal netTotals
	history  []Point

	procMu   sync.Mutex
	procPrev map[int]procSample
	procAt   time.Time
	users    map[uint32]string
	usersAt  time.Time
}

func New(version string, disks []Disk) *Monitor {
	return &Monitor{version: version, started: time.Now(), disks: disks}
}

// Run samples CPU and network counters until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.sample()
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sample()
		}
	}
}

func (m *Monitor) sample() {
	cpu, cpuErr := readCPU()
	nt, netErr := readNet()
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if cpuErr == nil {
		if total := cpu.total - m.cpuPrev.total; m.cpuPrev.total > 0 && total > 0 {
			m.cpuPct = 100 * float64(total-(cpu.idle-m.cpuPrev.idle)) / float64(total)
		}
		m.cpuPrev = cpu
	}
	if netErr == nil {
		if secs := now.Sub(m.netAt).Seconds(); !m.netAt.IsZero() && secs > 0 {
			m.rxRate = float64(nt.rx-m.netPrev.rx) / secs
			m.txRate = float64(nt.tx-m.netPrev.tx) / secs
		}
		m.netPrev, m.netAt, m.netTotal = nt, now, nt
	}
	mem := meminfo()
	var memPct float64
	if mem["MemTotal"] > 0 {
		memPct = 100 * float64(mem["MemTotal"]-mem["MemAvailable"]) / float64(mem["MemTotal"])
	}
	m.history = append(m.history, Point{T: now.UnixMilli(), CPU: m.cpuPct, Mem: memPct, Rx: m.rxRate, Tx: m.txRate})
	if len(m.history) > historyLen {
		m.history = append(m.history[:0], m.history[len(m.history)-historyLen:]...)
	}
}

// History returns a copy of the recent samples, oldest first.
func (m *Monitor) History() []Point {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Point(nil), m.history...)
}

type usage struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type diskUsage struct {
	Disk
	usage
}

type Snapshot struct {
	Hostname string      `json:"hostname"`
	OS       string      `json:"os"`
	Kernel   string      `json:"kernel"`
	Uptime   float64     `json:"uptime"`
	Load     [3]float64  `json:"load"`
	CPU      cpuInfo     `json:"cpu"`
	Memory   usage       `json:"memory"`
	Swap     usage       `json:"swap"`
	Disks    []diskUsage `json:"disks"`
	Network  netInfo     `json:"network"`
	App      appInfo     `json:"app"`
}

type cpuInfo struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type netInfo struct {
	RxRate  float64 `json:"rx_rate"`
	TxRate  float64 `json:"tx_rate"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`
}

type appInfo struct {
	Version    string    `json:"version"`
	Started    time.Time `json:"started"`
	RSS        uint64    `json:"rss"`
	Goroutines int       `json:"goroutines"`
}

func (m *Monitor) Snapshot() Snapshot {
	s := Snapshot{
		OS:     osName(),
		Kernel: strings.TrimSpace(readString("/proc/sys/kernel/osrelease")),
		CPU:    cpuInfo{Cores: runtime.NumCPU()},
		App: appInfo{
			Version:    m.version,
			Started:    m.started,
			RSS:        statusKB("/proc/self/status", "VmRSS") * 1024,
			Goroutines: runtime.NumGoroutine(),
		},
	}
	s.Hostname, _ = os.Hostname()
	if f := strings.Fields(readString("/proc/uptime")); len(f) > 0 {
		s.Uptime, _ = strconv.ParseFloat(f[0], 64)
	}
	if f := strings.Fields(readString("/proc/loadavg")); len(f) >= 3 {
		for i := range 3 {
			s.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	mem := meminfo()
	s.Memory = usage{Total: mem["MemTotal"], Used: mem["MemTotal"] - mem["MemAvailable"]}
	s.Swap = usage{Total: mem["SwapTotal"], Used: mem["SwapTotal"] - mem["SwapFree"]}

	seen := map[uint64]bool{}
	for _, d := range m.disks {
		var st syscall.Stat_t
		if err := syscall.Stat(d.Path, &st); err != nil || seen[st.Dev] {
			continue
		}
		seen[st.Dev] = true
		var fs syscall.Statfs_t
		if err := syscall.Statfs(d.Path, &fs); err != nil {
			continue
		}
		total := fs.Blocks * uint64(fs.Bsize)
		free := fs.Bavail * uint64(fs.Bsize)
		s.Disks = append(s.Disks, diskUsage{Disk: d, usage: usage{Total: total, Used: total - free}})
	}

	m.mu.Lock()
	s.CPU.Percent = m.cpuPct
	s.Network = netInfo{RxRate: m.rxRate, TxRate: m.txRate, RxTotal: m.netTotal.rx, TxTotal: m.netTotal.tx}
	m.mu.Unlock()
	return s
}

func (m *Monitor) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/system", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, m.Snapshot())
	})))
	mux.Handle("GET /api/system/history", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"interval": sampleEvery.Milliseconds(), "points": m.History()})
	})))
	mux.Handle("GET /api/system/processes", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		procs, err := m.Processes()
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "cannot read processes")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"processes": procs})
	})))
}

type cpuTimes struct{ total, idle uint64 }

func readCPU() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	fields := strings.Fields(sc.Text())
	var t cpuTimes
	// cpu user nice system idle iowait irq softirq steal ...
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i >= 8 { // guest time is already counted in user/nice
			break
		}
		t.total += n
		if i == 3 || i == 4 {
			t.idle += n
		}
	}
	return t, sc.Err()
}

type netTotals struct{ rx, tx uint64 }

func readNet() (netTotals, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return netTotals{}, err
	}
	defer f.Close()
	var t netTotals
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		name = strings.TrimSpace(name)
		if !ok || name == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		t.rx += rx
		t.tx += tx
	}
	return t, sc.Err()
}

// meminfo returns /proc/meminfo values in bytes.
func meminfo() map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(readString("/proc/meminfo"), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(rest); len(f) > 0 {
			n, _ := strconv.ParseUint(f[0], 10, 64)
			out[key] = n * 1024
		}
	}
	return out
}

func statusKB(path, key string) uint64 {
	for _, line := range strings.Split(readString(path), "\n") {
		if k, rest, ok := strings.Cut(line, ":"); ok && k == key {
			if f := strings.Fields(rest); len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

func osName() string {
	for _, line := range strings.Split(readString("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

func readString(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
