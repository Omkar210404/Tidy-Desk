//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

func freeBytes(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	if r, _, err := proc.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0); r == 0 {
		return 0, err
	}
	return free, nil
}

// makeUnreadable holds the file open with no sharing, so any other attempt
// to open it fails until the returned function is called.
func makeUnreadable(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}

// browserCommand opens url in the default browser; attempt 1 is a fallback.
func browserCommand(url string, attempt int) *exec.Cmd {
	if attempt == 0 {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	cmd := exec.Command("cmd", "/c", "start", "", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}
