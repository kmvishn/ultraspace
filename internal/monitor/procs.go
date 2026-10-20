package monitor

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, which is 100 on every mainstream Linux architecture.
const clockTicks = 100

type Process struct {
	PID     int     `json:"pid"`
	Name    string  `json:"name"`
	User    string  `json:"user"`
	State   string  `json:"state"`
	CPU     float64 `json:"cpu"` // percent of one core
	RSS     uint64  `json:"rss"`
	Threads int     `json:"threads"`
	Command string  `json:"command"`
}

type procSample struct {
	ticks uint64
}

// Processes lists running processes with CPU usage measured since the previous call.
func (m *Monitor) Processes() ([]Process, error) {
	m.procMu.Lock()
	defer m.procMu.Unlock()

	if m.procPrev == nil {
		// First call: take a short baseline so CPU numbers are meaningful.
		_, samples, err := readProcs(m.userName)
		if err != nil {
			return nil, err
		}
		m.procPrev, m.procAt = samples, time.Now()
		time.Sleep(300 * time.Millisecond)
	}
	procs, samples, err := readProcs(m.userName)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if elapsed := now.Sub(m.procAt).Seconds(); elapsed > 0 {
		for i := range procs {
			cur := samples[procs[i].PID]
			if prev, ok := m.procPrev[procs[i].PID]; ok && cur.ticks >= prev.ticks {
				procs[i].CPU = 100 * float64(cur.ticks-prev.ticks) / clockTicks / elapsed
			}
		}
	}
	m.procPrev, m.procAt = samples, now
	sort.Slice(procs, func(i, j int) bool {
		if procs[i].CPU != procs[j].CPU {
			return procs[i].CPU > procs[j].CPU
		}
		return procs[i].RSS > procs[j].RSS
	})
	return procs, nil
}

func readProcs(userName func(uint32) string) ([]Process, map[int]procSample, error) {
	dir, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil, err
	}
	pageSize := uint64(os.Getpagesize())
	procs := make([]Process, 0, len(dir))
	samples := make(map[int]procSample, len(dir))
	for _, d := range dir {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		base := "/proc/" + d.Name()
		stat := readString(base + "/stat")
		// The command name is in parentheses and may itself contain spaces or ')'.
		open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
		if open < 0 || end < open {
			continue
		}
		f := strings.Fields(stat[end+1:])
		if len(f) < 22 {
			continue
		}
		utime, _ := strconv.ParseUint(f[11], 10, 64)
		stime, _ := strconv.ParseUint(f[12], 10, 64)
		threads, _ := strconv.Atoi(f[17])
		rssPages, _ := strconv.ParseUint(f[21], 10, 64)
		p := Process{
			PID:     pid,
			Name:    stat[open+1 : end],
			State:   f[0],
			RSS:     rssPages * pageSize,
			Threads: threads,
		}
		if uid, ok := statusUID(base + "/status"); ok {
			p.User = userName(uid)
		}
		cmd := strings.TrimRight(strings.ReplaceAll(readString(base+"/cmdline"), "\x00", " "), " ")
		if len(cmd) > 300 {
			cmd = cmd[:300] + "…"
		}
		p.Command = cmd
		procs = append(procs, p)
		samples[pid] = procSample{ticks: utime + stime}
	}
	return procs, samples, nil
}

func statusUID(path string) (uint32, bool) {
	for _, line := range strings.Split(readString(path), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, err := strconv.ParseUint(f[0], 10, 32)
				return uint32(n), err == nil
			}
		}
	}
	return 0, false
}

// userName maps a UID to a login name using /etc/passwd, refreshed every minute.
func (m *Monitor) userName(uid uint32) string {
	if m.users == nil || time.Since(m.usersAt) > time.Minute {
		m.users = map[uint32]string{}
		for _, line := range strings.Split(readString("/etc/passwd"), "\n") {
			f := strings.Split(line, ":")
			if len(f) < 3 {
				continue
			}
			if n, err := strconv.ParseUint(f[2], 10, 32); err == nil {
				m.users[uint32(n)] = f[0]
			}
		}
		m.usersAt = time.Now()
	}
	if name, ok := m.users[uid]; ok {
		return name
	}
	return strconv.FormatUint(uint64(uid), 10)
}
