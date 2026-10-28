//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

func freeBytes(path string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil
}

// makeUnreadable removes all permissions (has no effect when run as root).
func makeUnreadable(path string) (func(), error) {
	if err := os.Chmod(path, 0); err != nil {
		return nil, err
	}
	return func() { os.Chmod(path, 0o644) }, nil
}

// browserCommand opens url in the default browser.
func browserCommand(url string, attempt int) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}
