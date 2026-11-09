//go:build windows

package files

func getDiskSpace(p string) (uint64, uint64, error) {
	// Fallback mock disk space for Windows development/testing
	return 500 * 1024 * 1024 * 1024, 250 * 1024 * 1024 * 1024, nil
}

func readProcMounts() ([]byte, error) {
	return nil, nil
}

