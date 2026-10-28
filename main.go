// TidyDesk finds duplicate files and similar photos and moves the extras to a
// recoverable "TidyDesk Trash". It runs a local web UI on 127.0.0.1.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Omkar210404/Tidy-Desk/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "selftest":
			os.Exit(selftest())
		case "bench":
			os.Exit(bench())
		}
	}
	port := flag.Int("port", 0, "port to listen on (default: a free port)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage:\n  tidydesk [--port N] [--no-browser]   start the app\n  tidydesk selftest                    run the built-in checks\n  tidydesk bench                       time a scan of a small generated dataset")
		flag.PrintDefaults()
	}
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fatal("TidyDesk couldn't start: %v", err)
	}
	app := web.NewApp(ln.Addr().String())
	if dir, err := os.UserConfigDir(); err == nil {
		app.ConfigPath = filepath.Join(dir, "TidyDesk", "trash-folders.json")
		app.LoadBins()
	}
	url := "http://" + ln.Addr().String() + "/"
	fmt.Printf("TidyDesk is running at %s\nEverything stays on this computer. Close this window (or press Ctrl+C) to stop TidyDesk.\n", url)
	if !*noBrowser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fatal("TidyDesk stopped: %v", err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Open this address in your browser:", url)
	}
}

// fatal prints the error and, when started by double-click, waits so the
// window doesn't vanish before it can be read.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	if len(os.Args) == 1 {
		fmt.Println("Press Enter to close.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}
