//go:build !windows

package web

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// pickFolder uses osascript on macOS and zenity on Linux. Cancelling returns "".
func pickFolder(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "zenity", "--file-selection", "--directory", "--title=Choose a folder for TidyDesk to scan")
	if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "osascript", "-e", `POSIX path of (choose folder with prompt "Choose a folder for TidyDesk to scan")`)
	}
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}
