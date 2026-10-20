// Package sysmem reads the host's memory and picks a profile that keeps ultraspace
// usable on small machines (down to 512 MB of RAM).
package sysmem

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Info is memory in bytes, from /proc/meminfo.
type Info struct {
	Total, Available, SwapTotal, SwapFree uint64
}

func Read() Info {
	var in Info
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return in
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		n *= 1024
		switch k {
		case "MemTotal":
			in.Total = n
		case "MemAvailable":
			in.Available = n
		case "SwapTotal":
			in.SwapTotal = n
		case "SwapFree":
			in.SwapFree = n
		}
	}
	return in
}

// Profile sizes the heavy apps for the machine.
type Profile struct {
	Name string
	// Remote browser
	RendererLimit int // Chrome renderer processes
	LiveTabs      int // tabs kept loaded; the rest sleep and reload when opened
	SleepAfterMin int // background tabs sleep after this many idle minutes
	BrowserIdle   int // minutes Chrome keeps running with no window connected
	MinFreeMB     int // refuse to start Chrome below this much available memory
	// VS Code
	CodeIdle int // minutes code-server keeps running unused
}

var profiles = map[string]Profile{
	"tiny":   {Name: "tiny", RendererLimit: 1, LiveTabs: 1, SleepAfterMin: 2, BrowserIdle: 3, MinFreeMB: 140, CodeIdle: 5},
	"small":  {Name: "small", RendererLimit: 2, LiveTabs: 2, SleepAfterMin: 10, BrowserIdle: 5, MinFreeMB: 160, CodeIdle: 10},
	"normal": {Name: "normal", RendererLimit: 4, LiveTabs: 6, SleepAfterMin: 30, BrowserIdle: 10, MinFreeMB: 200, CodeIdle: 20},
}

// Pick returns the named profile, or for "auto"/"" the one that fits total RAM.
func Pick(name string) Profile {
	if p, ok := profiles[name]; ok {
		return p
	}
	total := Read().Total
	switch {
	case total == 0:
		return profiles["small"]
	case total <= 700<<20:
		return profiles["tiny"]
	case total <= 1536<<20:
		return profiles["small"]
	default:
		return profiles["normal"]
	}
}

// PreferOOMKill raises a process's oom_score_adj, so under memory pressure the kernel
// kills it (and the children it starts later) before sshd, nginx or ultraspace itself.
func PreferOOMKill(pid, score int) {
	_ = os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(score)), 0)
}
