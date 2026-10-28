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
	"path/filepath"
	"time"

	"github.com/Omkar210404/Tidy-Desk/internal/web"
)

// version is set at build time, e.g. -ldflags "-X main.version=v1.1.0".
var version = "dev"

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
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage:\n  tidydesk [--port N] [--no-browser]   start the app\n  tidydesk --version                   print the version\n  tidydesk selftest                    run the built-in checks\n  tidydesk bench                       time a scan of a small generated dataset")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("TidyDesk", version)
		return
	}

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
	fmt.Printf("TidyDesk %s is running at %s\nEverything stays on this computer. Keep this window open while you use TidyDesk; close it to stop.\n", version, url)
	if !*noBrowser {
		go openBrowser(url, app.Visited)
	}
	srv := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fatal("TidyDesk stopped: %v", err)
	}
}

// openBrowser opens the page and checks that it actually loaded. If it
// hasn't after a few seconds it tries a second way, then tells the user.
func openBrowser(url string, visited <-chan struct{}) {
	fmt.Println("Opening TidyDesk in your browser...")
	for attempt := 0; attempt < 2; attempt++ {
		if err := browserCommand(url, attempt).Start(); err == nil {
			select {
			case <-visited:
				fmt.Println("TidyDesk is open in your browser.")
				return
			case <-time.After(8 * time.Second):
			}
		}
	}
	fmt.Printf("\nYour browser didn't open by itself.\nOpen your browser and go to this address:\n\n    %s\n\n", url)
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
