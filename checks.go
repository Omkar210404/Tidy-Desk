package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Omkar210404/TidyDesk/internal/scan"
	"github.com/Omkar210404/TidyDesk/internal/trash"
	"github.com/Omkar210404/TidyDesk/internal/web"
)

// selftest builds a tiny folder of known files, runs the real scanner and
// actions on it, prints PASS/FAIL per step and deletes everything it made.
func selftest() int {
	dir, err := os.MkdirTemp("", "tidydesk-selftest-")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		fmt.Println("FAIL  create a temporary folder:", err)
		return 1
	}
	unlock := func() {}
	cleanup := func() { unlock(); os.RemoveAll(dir) }
	defer cleanup()
	stopOnCtrlC(cleanup)

	fails := 0
	check := func(name string, ok bool, detail ...any) {
		if ok {
			fmt.Println("PASS ", name)
			return
		}
		fails++
		fmt.Println("FAIL ", name, fmt.Sprint(detail...))
	}
	fmt.Println("TidyDesk self-test in", dir)

	// --- generate files ---
	r := rand.New(rand.NewPCG(1, 2))
	var genErr error
	write := func(rel string, data []byte) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			genErr = err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			genErr = err
		}
		return p
	}
	dup := randBytes(r, 200_000)
	a1 := write("Photos/holiday.bin", dup)
	a2 := write("Backup/holiday.bin", dup)
	a3 := write("Backup/old/holiday - Copy.bin", dup)
	doc := randBytes(r, 50_000)
	b1 := write("Docs/report.txt", doc)
	b2 := write("Downloads/report (1).txt", doc)
	x := randBytes(r, 100_000) // same size, same first/last 4 KB, different middle
	y := slices.Clone(x)
	y[50_000] ^= 0xFF
	x1, x2 := write("Misc/x1.dat", x), write("Misc/x2.dat", y)
	for i := range 5 {
		write(fmt.Sprintf("Misc/unique%d.dat", i), randBytes(r, 10_000+i*1000))
	}
	locked := write("Misc/locked.dat", randBytes(r, 30_000))
	write("Misc/same-size.dat", randBytes(r, 30_000)) // forces the locked file to be opened
	sunset := pattern(320, 240, 0)
	p1 := write("Photos/sunset.png", encodePNG(sunset))
	p2 := write("Photos/sunset-small.jpg", encodeJPEG(scan.Shrink(sunset, 160), 80))
	p3 := write("Phone/sunset-edit.jpg", encodeJPEG(brighter(sunset, 14), 90))
	forest := write("Photos/forest.png", encodePNG(pattern(320, 240, 5)))
	if un, err := makeUnreadable(locked); err == nil {
		unlock = un
	}
	check("generate test files", genErr == nil, genErr)

	// --- scan ---
	app := web.NewApp("127.0.0.1:1")
	err = app.Scan(scan.Options{Roots: []string{dir}, Kind: "all", Similar: true}, true)
	check("scan finishes without crashing", err == nil, err)
	res, sid := app.Result()
	if res == nil {
		fmt.Println("FAIL  no scan result; stopping")
		return 1
	}
	exact, similar := res.View(nil, scan.DefaultDistance)
	paths := func(g scan.Group) []string {
		var out []string
		for _, id := range g.Files {
			out = append(out, res.Files[id].Path)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(p ...string) []string { slices.Sort(p); return p }
	check("exact duplicates: 2 groups (3 copies and 2 copies)",
		len(exact) == 2 && slices.Equal(paths(exact[0]), sorted(a1, a2, a3)) && slices.Equal(paths(exact[1]), sorted(b1, b2)),
		fmt.Sprintf("got %d groups", len(exact)))
	inAny := func(p string) bool {
		return slices.ContainsFunc(slices.Concat(exact, similar), func(g scan.Group) bool { return slices.Contains(paths(g), p) })
	}
	check("same size and same start/end but different middle is not a duplicate", !inAny(x1) && !inAny(x2))
	check("unreadable file is counted as an error, not a crash",
		res.ErrorCount >= 1 && slices.ContainsFunc(res.Errors, func(e string) bool { return strings.Contains(e, "locked.dat") }),
		res.Errors)
	check("similar photos: the 3 sunset versions are grouped, the forest photo is not",
		len(similar) == 1 && slices.Equal(paths(similar[0]), sorted(p1, p2, p3)) && !inAny(forest),
		fmt.Sprintf("got %d similar groups", len(similar)))
	check("suggested keep for duplicates is the original-looking name",
		len(exact) == 2 && res.Files[exact[1].Keep].Path == b1)

	// --- safety rules ---
	id := func(p string) int { return slices.IndexFunc(res.Files, func(f *scan.File) bool { return f.Path == p }) }
	d := scan.DefaultDistance
	_, err = app.Remove(sid, []int{id(a1), id(a2), id(a3)}, d)
	check("refuses to remove every copy in a group", errors.Is(err, web.ErrKeepOne) && exists(a1) && exists(a2) && exists(a3), err)
	_, errStale := app.Remove(sid+1, []int{id(a2)}, d)
	_, errUnknown := app.Remove(sid, []int{id(x1)}, d)
	check("refuses stale results and files that are in no group", errStale != nil && errUnknown != nil && exists(x1))

	manifest := filepath.Join(dir, trash.DirName, "manifest.json")
	rep, err := app.Remove(sid, []int{id(a2), id(a3)}, d)
	st := app.TrashStatus()
	check("Remove moves files into TidyDesk Trash with a manifest",
		err == nil && rep.Moved == 2 && !exists(a2) && !exists(a3) && exists(manifest) && st.Files == 2, err, rep.Failed)
	_, err = app.Remove(sid, []int{id(a1)}, d)
	check("the last remaining copy is protected", errors.Is(err, web.ErrKeepOne) && exists(a1), err)

	os.WriteFile(b2, append(slices.Clone(doc), 'x'), 0o644)
	rep, err = app.Remove(sid, []int{id(b2)}, d)
	check("refuses a file that changed after the scan", err == nil && rep.Moved == 0 && len(rep.Failed) == 1 && exists(b2), err)

	u, err := app.Undo()
	check("Undo puts files back at their original paths, byte for byte",
		err == nil && u.Restored == 2 && sameContent(a2, dup) && sameContent(a3, dup) && app.TrashStatus().Files == 0 && !exists(manifest), err, u.Failed)

	rep, _ = app.Remove(sid, []int{id(a3)}, d)
	_, errNoConfirm := app.Empty("")
	before := app.TrashStatus().Files
	e, err := app.Empty("EMPTY")
	check("Empty TidyDesk Trash needs confirmation, then deletes",
		rep.Moved == 1 && errNoConfirm != nil && before == 1 && err == nil && e.Deleted == 1 &&
			app.TrashStatus().Files == 0 && !exists(filepath.Join(dir, trash.DirName)), err)

	h := app.Handler()
	call := func(method, path, host, token string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		if token != "" {
			req.Header.Set("X-TidyDesk-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	check("API needs the session token and a local Host",
		call("POST", "/api/undo", app.Addr, "") == http.StatusForbidden &&
			call("POST", "/api/undo", app.Addr, "wrong") == http.StatusForbidden &&
			call("GET", "/api/state", "evil.example:80", app.Token) == http.StatusForbidden &&
			call("GET", "/api/state", app.Addr, app.Token) == http.StatusOK)

	cleanup()
	check("cleanup removed everything the self-test created", !exists(dir))
	if fails > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", fails)
		return 1
	}
	fmt.Println("\nAll checks passed.")
	return 0
}

// bench times a scan of about 5,000 small generated files (under 300 MB)
// and deletes them afterwards.
func bench() int {
	tmp := os.TempDir()
	if free, err := freeBytes(tmp); err == nil && free < 2<<30 {
		fmt.Printf("Not running: only %.1f GB free on the temp drive (need 2 GB).\n", float64(free)/(1<<30))
		return 1
	}
	dir, err := os.MkdirTemp(tmp, "tidydesk-bench-")
	if err != nil {
		fmt.Println("Can't create a temporary folder:", err)
		return 1
	}
	cleanup := func() { os.RemoveAll(dir) }
	defer cleanup()
	stopOnCtrlC(cleanup)

	fmt.Println("Generating 5,000 files in", dir)
	r := rand.New(rand.NewPCG(7, 7))
	var total int64
	var prev [][]byte
	for i := range 5000 {
		var data []byte
		name := fmt.Sprintf("file%04d.dat", i)
		switch {
		case i%20 == 0: // 250 photos: 125 originals as PNG + an edited JPEG of each
			img := pattern(256, 192, i/40)
			if i%40 == 0 {
				data, name = encodePNG(img), fmt.Sprintf("photo%04d.png", i)
			} else {
				data, name = encodeJPEG(brighter(img, 10), 85), fmt.Sprintf("photo%04d.jpg", i)
			}
		case i%7 == 1 && len(prev) > 0: // exact duplicates of earlier files
			data = prev[r.IntN(len(prev))]
		case i%11 == 2 && len(prev) > 0: // same size as an earlier file, different content
			data = randBytes(r, len(prev[r.IntN(len(prev))]))
		default:
			data = randBytes(r, 2048+r.IntN(62*1024))
			if len(prev) < 1000 {
				prev = append(prev, data)
			}
		}
		p := filepath.Join(dir, fmt.Sprintf("folder%02d", i%50), fmt.Sprintf("sub%d", i%3), name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			fmt.Println("Can't write test data:", err)
			return 1
		}
		total += int64(len(data))
	}
	fmt.Printf("Dataset: 5,000 files, %.1f MB. The files were just written, so they are probably in the OS file cache (a warm-cache result).\n", float64(total)/(1<<20))
	fmt.Printf("Machine: %s/%s, %d logical CPUs\n\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())

	for _, similar := range []bool{false, true} {
		t := scan.NewTracker()
		start := time.Now()
		res, err := scan.Run(context.Background(), scan.Options{Roots: []string{dir}, Similar: similar}, t)
		el := time.Since(start)
		if err != nil {
			fmt.Println("Scan failed:", err)
			return 1
		}
		exact, sim := res.View(nil, scan.DefaultDistance)
		label := "duplicates only        "
		if similar {
			label = "duplicates + similar   "
		}
		fmt.Printf("%s %6.2f s  %7.0f files/s  hashed %.1f MB  %d duplicate groups, %d similar groups\n",
			label, el.Seconds(), float64(len(res.Files))/el.Seconds(), float64(res.Hashed)/(1<<20), len(exact), len(sim))
	}
	cleanup()
	if _, err := os.Stat(dir); err == nil {
		fmt.Println("\nWarning: could not delete", dir)
		return 1
	}
	fmt.Println("\nTest data deleted.")
	return 0
}

// stopOnCtrlC runs cleanup and exits if the user presses Ctrl+C.
func stopOnCtrlC(cleanup func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		cleanup()
		fmt.Println("\nInterrupted; test data deleted.")
		os.Exit(130)
	}()
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func sameContent(p string, want []byte) bool {
	got, err := os.ReadFile(p)
	return err == nil && bytes.Equal(got, want)
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := r.Uint64()
		for j := i; j < min(i+8, n); j++ {
			b[j] = byte(v)
			v >>= 8
		}
	}
	return b
}

// pattern draws a smooth picture; different seeds give different pictures.
func pattern(w, h, seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fx, fy, ph := 14+float64(seed%9)*3, 10+float64(seed%7)*4, float64(seed)*1.7
	for y := range h {
		for x := range w {
			v := 128 + 100*math.Sin(float64(x)/fx+ph)*math.Cos(float64(y)/fy-ph/2)
			img.Set(x, y, color.RGBA{uint8(v), uint8(v*0.7 + 30), uint8(255 - v), 255})
		}
	}
	return img
}

func brighter(src *image.RGBA, d int) *image.RGBA {
	img := image.NewRGBA(src.Rect)
	for i, v := range src.Pix {
		if i%4 == 3 {
			img.Pix[i] = v
		} else {
			img.Pix[i] = uint8(min(int(v)+d, 255))
		}
	}
	return img
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func encodeJPEG(img image.Image, q int) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: q})
	return b.Bytes()
}
