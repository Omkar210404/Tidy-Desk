//go:build windows

package web

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
)

// pickFolder shows the Windows folder dialog through PowerShell.
const pickScript = `[Console]::OutputEncoding = [Text.Encoding]::UTF8; ` +
	`Add-Type -AssemblyName System.Windows.Forms; ` +
	`$owner = New-Object System.Windows.Forms.Form; $owner.TopMost = $true; ` +
	`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
	`$d.Description = 'Choose a folder for TidyDesk to scan'; $d.ShowNewFolderButton = $false; ` +
	`if ($d.ShowDialog($owner) -eq 'OK') { [Console]::Out.Write($d.SelectedPath) }`

func pickFolder(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", pickScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// revealFile opens Explorer with the file selected.
func revealFile(path string) error {
	return exec.Command("explorer.exe", "/select,", path).Start()
}
