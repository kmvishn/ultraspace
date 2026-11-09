package files

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"ultraspace/internal/httpx"
)

// Drive represents an attached storage volume (system drive, USB stick, external disk).
type Drive struct {
	Name       string `json:"name"`
	MountPoint string `json:"mountpoint"`
	Device     string `json:"device"`
	FSType     string `json:"fstype"`
	Total      uint64 `json:"total"`
	Used       uint64 `json:"used"`
	Free       uint64 `json:"free"`
	Removable  bool   `json:"removable"`
}

// Place represents a quick-access folder in the file explorer sidebar.
type Place struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Icon string `json:"icon"`
}

// ParseMounts parses lines of mounts (e.g. /proc/mounts) into Drive structs.
func ParseMounts(r []byte) []Drive {
	if len(r) == 0 {
		// Default root drive if no mounts could be read
		total, free, _ := getDiskSpace("/")
		return []Drive{
			{
				Name:       "System Disk (/)",
				MountPoint: "/",
				Device:     "/dev/root",
				FSType:     "ext4",
				Total:      total,
				Free:       free,
				Used:       total - free,
			},
		}
	}

	var drives []Drive
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(r))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		dev := strings.ReplaceAll(fields[0], "\\040", " ")
		mountPoint := strings.ReplaceAll(fields[1], "\\040", " ")
		fstype := fields[2]

		// Filter for real disk devices and storage mounts
		isBlockDev := strings.HasPrefix(dev, "/dev/")
		isStorageMount := mountPoint == "/" ||
			strings.HasPrefix(mountPoint, "/media") ||
			strings.HasPrefix(mountPoint, "/mnt") ||
			strings.HasPrefix(mountPoint, "/run/media") ||
			mountPoint == "/home"

		if (!isBlockDev && !isStorageMount) || seen[mountPoint] {
			continue
		}

		// Filter out virtual/system mounts and snap loops
		if fstype == "swap" || fstype == "squashfs" || fstype == "tmpfs" || fstype == "devtmpfs" || fstype == "overlay" {
			continue
		}
		if strings.HasPrefix(dev, "/dev/loop") || strings.HasPrefix(mountPoint, "/snap") || strings.HasPrefix(mountPoint, "/sys") || strings.HasPrefix(mountPoint, "/proc") || strings.HasPrefix(mountPoint, "/dev") {
			continue
		}

		seen[mountPoint] = true

		name := "Storage Disk"
		removable := false
		switch {
		case mountPoint == "/":
			name = "System Disk (/)"
		case mountPoint == "/home":
			name = "Home Drive (/home)"
		case strings.HasPrefix(mountPoint, "/media") || strings.HasPrefix(mountPoint, "/run/media"):
			name = "USB Drive (" + path.Base(mountPoint) + ")"
			removable = true
		case strings.HasPrefix(mountPoint, "/mnt"):
			name = "Mounted Storage (" + path.Base(mountPoint) + ")"
			removable = true
		default:
			name = path.Base(mountPoint) + " (" + dev + ")"
		}

		total, free, err := getDiskSpace(mountPoint)
		if err != nil && total == 0 {
			total, free, _ = getDiskSpace("/")
		}

		drives = append(drives, Drive{
			Name:       name,
			MountPoint: mountPoint,
			Device:     dev,
			FSType:     fstype,
			Total:      total,
			Free:       free,
			Used:       total - free,
			Removable:  removable,
		})
	}

	if len(drives) == 0 {
		total, free, _ := getDiskSpace("/")
		drives = append(drives, Drive{
			Name:       "System Disk (/)",
			MountPoint: "/",
			Total:      total,
			Free:       free,
			Used:       total - free,
		})
	}

	return drives
}

// GetDrives returns all physical and external storage volumes mounted on the host, cached for 5 seconds.
func (f *Files) GetDrives() []Drive {
	f.drivesMu.RLock()
	if time.Since(f.drivesAt) < 5*time.Second && len(f.cachedDrives) > 0 {
		cached := f.cachedDrives
		f.drivesMu.RUnlock()
		return cached
	}
	f.drivesMu.RUnlock()

	f.drivesMu.Lock()
	defer f.drivesMu.Unlock()

	if time.Since(f.drivesAt) < 5*time.Second && len(f.cachedDrives) > 0 {
		return f.cachedDrives
	}

	raw, err := readProcMounts()
	if err != nil && f.log != nil {
		f.log.Debug("read proc mounts failed", "err", err)
	}
	drives := ParseMounts(raw)
	f.cachedDrives = drives
	f.drivesAt = time.Now()
	return drives
}

// GetPlaces generates standard native file explorer shortcuts.
func (f *Files) GetPlaces() []Place {
	var places []Place

	// 1. Home Directory
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/home"
	}
	homeDir = filepath.ToSlash(homeDir)

	if st, err := os.Stat(homeDir); err == nil && st.IsDir() {
		places = append(places, Place{Name: "Home", Path: homeDir, Icon: "home"})

		// Standard user subdirectories
		subdirs := []struct {
			name string
			icon string
		}{
			{"Downloads", "download"},
			{"Documents", "fileText"},
			{"Music", "music"},
			{"Pictures", "image"},
			{"Videos", "film"},
		}
		for _, s := range subdirs {
			sp := path.Join(homeDir, s.name)
			if sst, err := os.Stat(sp); err == nil && sst.IsDir() {
				places = append(places, Place{Name: s.name, Path: sp, Icon: s.icon})
			}
		}
	} else {
		places = append(places, Place{Name: "Home", Path: "/", Icon: "home"})
	}

	// 2. Root System Disk
	places = append(places, Place{Name: "File System", Path: "/", Icon: "server"})

	return places
}

// FindDriveForPath finds the mounted drive that contains the given path.
func (f *Files) FindDriveForPath(targetPath string) *Drive {
	drives := f.GetDrives()
	var best *Drive
	bestLen := -1

	cleanPath := path.Clean("/" + targetPath)

	for i := range drives {
		mp := drives[i].MountPoint
		if cleanPath == mp || strings.HasPrefix(cleanPath, mp+"/") || mp == "/" {
			if len(mp) > bestLen {
				best = &drives[i]
				bestLen = len(mp)
			}
		}
	}

	return best
}

func (f *Files) handleDrives(w http.ResponseWriter, r *http.Request) {
	drives := f.GetDrives()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"drives": drives})
}

func (f *Files) handlePlaces(w http.ResponseWriter, r *http.Request) {
	places := f.GetPlaces()
	drives := f.GetDrives()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"places": places,
		"drives": drives,
	})
}

func (f *Files) handleStat(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		reqPath = "/"
	}

	drive := f.FindDriveForPath(reqPath)
	if drive != nil {
		httpx.WriteJSON(w, http.StatusOK, drive)
		return
	}

	total, free, _ := getDiskSpace(reqPath)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"name":       "Storage",
		"mountpoint": "/",
		"total":      total,
		"free":       free,
		"used":       total - free,
	})
}

