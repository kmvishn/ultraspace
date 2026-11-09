//go:build !windows

package files

import (
	"os"
	"syscall"
)

func getDiskSpace(p string) (uint64, uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(p, &fs); err != nil {
		return 0, 0, err
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	return total, free, nil
}

func readProcMounts() ([]byte, error) {
	return os.ReadFile("/proc/mounts")
}

