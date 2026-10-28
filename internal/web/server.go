// Package web serves TidyDesk's browser interface and holds the safety rules for
// every action: session token, keep-one-copy, and path checks.
package web

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Omkar210404/Tidy-Desk/internal/scan"
	"github.com/Omkar210404/Tidy-Desk/internal/trash"
)

//go:embed static
var static embed.FS

// ErrKeepOne is returned when a request would remove every file in a group.
var ErrKeepOne = errors.New("TidyDesk always keeps at least one file in every group, so nothing was moved. Untick one file in each group you fully selected.")

// App is one TidyDesk session.
type App struct {
	Token      string
	Addr       string // host:port the server listens on, for the Host check
	ConfigPath string // remembers TidyDesk Trash folders between runs; "" = don't

	mu      sync.Mutex
	state   string // idle, scanning, done, cancelled, error
	message string
	opts    scan.Options
	tracker *scan.Tracker
	cancel  context.CancelFunc
	res     *scan.Result
	scanID  int
	byPath  map[string]int
	removed map[int]bool
	bins    map[string]*trash.Bin

	thumbs   *thumbCache
	thumbSem chan struct{}

	// Visited is closed the first time the page loads, so main can tell
	// whether the browser really opened.
	Visited     chan struct{}
	visitedOnce sync.Once
}

func NewApp(addr string) *App {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &App{Token: hex.EncodeToString(b), Addr: addr, state: "idle", removed: map[int]bool{},
		bins: map[string]*trash.Bin{}, thumbs: newThumbCache(64 << 20), thumbSem: make(chan struct{}, 4), Visited: make(chan struct{})}
}

// Scan starts a scan. With wait it blocks and returns the scan's error.
func (a *App) Scan(opts scan.Options, wait bool) error {
	roots, err := scan.CleanRoots(opts.Roots)
	if err != nil {
		return err
	}
	opts.Roots = roots
	ctx, cancel := context.WithCancel(context.Background())
	t := scan.NewTracker()
	a.mu.Lock()
	if a.state == "scanning" {
		a.mu.Unlock()
		cancel()
		return errors.New("a scan is already running")
	}
	a.state, a.message, a.opts, a.tracker, a.cancel = "scanning", "", opts, t, cancel
	for _, r := range roots {
		a.bin(r) // load any existing TidyDesk Trash so its status shows
	}
	a.mu.Unlock()

	run := func() error {
		defer cancel()
		res, err := scan.Run(ctx, opts, t)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.cancel = nil
		switch {
		case errors.Is(err, context.Canceled):
			a.state, a.message = "cancelled", "Scan cancelled. Nothing was changed."
		case err != nil:
			a.state, a.message = "error", err.Error()
		default:
			a.state, a.res, a.removed = "done", res, map[int]bool{}
			a.scanID++
			a.byPath = make(map[string]int, len(res.Files))
			for _, f := range res.Files {
				a.byPath[f.Path] = f.ID
			}
			a.thumbs.clear()
		}
		return err
	}
	if wait {
		return run()
	}
	go run()
	return nil
}

func (a *App) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// Result returns the latest scan and its ID.
func (a *App) Result() (*scan.Result, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.res, a.scanID
}

// bin returns the TidyDesk Trash for a scanned folder. Callers hold a.mu.
func (a *App) bin(root string) (*trash.Bin, error) {
	dir := filepath.Join(root, trash.DirName)
	if b, ok := a.bins[dir]; ok {
		return b, nil
	}
	b, err := trash.Open(dir)
	if err != nil {
		return nil, err
	}
	a.bins[dir] = b
	return b, nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// check makes sure a file from the scan is still safe to touch: inside a
// chosen folder, a regular file (not a link), unchanged since the scan, and
// not reached through a folder that has become a link. Callers hold a.mu.
func (a *App) check(f *scan.File) (root string, err error) {
	for _, r := range a.res.Roots {
		if scan.Within(f.Path, r) {
			root = r
			break
		}
	}
	if root == "" {
		return "", errors.New("it is outside the folders you chose")
	}
	info, err := os.Lstat(f.Path)
	if err != nil {
		return "", errors.New("it is no longer there")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("it is not a regular file")
	}
	if info.Size() != f.Size || !info.ModTime().Equal(f.ModTime) {
		return "", errors.New("it changed after the scan; scan again first")
	}
	dir := filepath.Dir(f.Path)
	if real, err := filepath.EvalSymlinks(dir); err != nil || !samePath(real, dir) {
		return "", errors.New("its folder is now a link")
	}
	return root, nil
}

type MoveReport struct {
	Moved  int      `json:"moved"`
	Bytes  int64    `json:"bytes"`
	Failed []string `json:"failed"`
}

// Remove moves files from the latest scan into TidyDesk Trash. It refuses the
// whole request if any group would be left with no file.
func (a *App) Remove(scanID int, ids []int, maxDist int) (MoveReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rep := MoveReport{Failed: []string{}}
	switch {
	case a.state == "scanning":
		return rep, errors.New("wait for the scan to finish")
	case a.res == nil || scanID != a.scanID:
		return rep, errors.New("these results are out of date; please scan again")
	case len(ids) == 0:
		return rep, errors.New("no files are ticked")
	}
	exact, similar := a.res.Groups(maxDist)
	groups := slices.Concat(exact, similar)
	inGroup := map[int]bool{}
	for _, g := range groups {
		for _, id := range g {
			inGroup[id] = true
		}
	}
	sel := map[int]bool{}
	for _, id := range ids {
		if !inGroup[id] {
			return rep, fmt.Errorf("file %d is not part of the latest scan results", id)
		}
		if a.removed[id] {
			return rep, errors.New("some ticked files are already in TidyDesk Trash")
		}
		sel[id] = true
	}
	for _, g := range groups {
		if !slices.ContainsFunc(g, func(id int) bool { return !a.removed[id] && !sel[id] }) {
			return rep, ErrKeepOne
		}
	}

	batch := trash.NewBatchID()
	byBin := map[*trash.Bin][]trash.Item{}
	for id := range sel {
		f := a.res.Files[id]
		root, err := a.check(f)
		var b *trash.Bin
		if err == nil {
			b, err = a.bin(root)
		}
		if err != nil {
			rep.Failed = append(rep.Failed, f.Path+": skipped because "+err.Error())
			continue
		}
		byBin[b] = append(byBin[b], trash.Item{Path: f.Path, Size: f.Size})
	}
	for b, items := range byBin {
		moved, failed := b.Move(items, batch)
		for _, it := range moved {
			a.removed[a.byPath[it.Path]] = true
			rep.Moved++
			rep.Bytes += it.Size
		}
		rep.Failed = append(rep.Failed, failed...)
	}
	a.saveBins()
	return rep, nil
}

type UndoReport struct {
	Restored int      `json:"restored"`
	Bytes    int64    `json:"bytes"`
	Failed   []string `json:"failed"`
}

// Undo puts back the most recent move.
func (a *App) Undo() (UndoReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rep := UndoReport{Failed: []string{}}
	latest := ""
	for _, b := range a.bins {
		latest = max(latest, b.LastBatch())
	}
	if latest == "" {
		return rep, errors.New("nothing to undo: TidyDesk Trash is empty")
	}
	for _, b := range a.bins {
		restored, failed := b.Restore(latest)
		for _, e := range restored {
			if id, ok := a.byPath[e.Original]; ok {
				delete(a.removed, id)
			}
			rep.Restored++
			rep.Bytes += e.Size
		}
		rep.Failed = append(rep.Failed, failed...)
	}
	a.saveBins()
	return rep, nil
}

type EmptyReport struct {
	Deleted int      `json:"deleted"`
	Bytes   int64    `json:"bytes"`
	Failed  []string `json:"failed"`
}

// Empty permanently deletes everything in TidyDesk Trash. confirm must be "EMPTY".
func (a *App) Empty(confirm string) (EmptyReport, error) {
	rep := EmptyReport{Failed: []string{}}
	if confirm != "EMPTY" {
		return rep, errors.New("emptying TidyDesk Trash needs your confirmation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range a.bins {
		n, bytes, failed := b.Empty()
		rep.Deleted += n
		rep.Bytes += bytes
		rep.Failed = append(rep.Failed, failed...)
	}
	a.saveBins()
	return rep, nil
}

type TrashStatus struct {
	Files     int      `json:"files"`
	Bytes     int64    `json:"bytes"`
	LastFiles int      `json:"lastFiles"`
	LastBytes int64    `json:"lastBytes"`
	Folders   []string `json:"folders"`
}

func (a *App) TrashStatus() TrashStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trashStatus()
}

func (a *App) trashStatus() TrashStatus {
	st := TrashStatus{Folders: []string{}}
	latest := ""
	for _, b := range a.bins {
		latest = max(latest, b.LastBatch())
	}
	for dir, b := range a.bins {
		n, bytes := b.Status()
		if n == 0 {
			continue
		}
		st.Files += n
		st.Bytes += bytes
		st.Folders = append(st.Folders, dir)
		ln, lb := b.BatchStatus(latest)
		st.LastFiles += ln
		st.LastBytes += lb
	}
	slices.Sort(st.Folders)
	return st
}

// LoadBins reopens TidyDesk Trash folders from earlier runs so Undo still works.
func (a *App) LoadBins() {
	if a.ConfigPath == "" {
		return
	}
	data, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		return
	}
	var dirs []string
	json.Unmarshal(data, &dirs)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, dir := range dirs {
		if b, err := trash.Open(dir); err == nil {
			a.bins[dir] = b
		}
	}
}

func (a *App) saveBins() {
	if a.ConfigPath == "" {
		return
	}
	dirs := []string{}
	for dir, b := range a.bins {
		if n, _ := b.Status(); n > 0 {
			dirs = append(dirs, dir)
		}
	}
	data, _ := json.Marshal(dirs)
	os.MkdirAll(filepath.Dir(a.ConfigPath), 0o755)
	os.WriteFile(a.ConfigPath, data, 0o644)
}

// ---- HTTP ----

// Handler returns the HTTP handler for the UI and API.
func (a *App) Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	mux.HandleFunc("GET /api/state", a.hState)
	mux.HandleFunc("GET /api/events", a.hEvents)
	mux.HandleFunc("GET /api/result", a.hResult)
	mux.HandleFunc("GET /api/thumb", a.hThumb)
	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		var opts scan.Options
		if !readJSON(w, r, &opts) {
			return
		}
		if err := a.Scan(opts, false); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		a.Cancel()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ScanID   int   `json:"scanId"`
			IDs      []int `json:"ids"`
			Distance int   `json:"distance"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		rep, err := a.Remove(req.ScanID, req.IDs, req.Distance)
		a.respond(w, rep, err)
	})
	mux.HandleFunc("POST /api/undo", func(w http.ResponseWriter, r *http.Request) {
		rep, err := a.Undo()
		a.respond(w, rep, err)
	})
	mux.HandleFunc("POST /api/empty", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Confirm string `json:"confirm"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		rep, err := a.Empty(req.Confirm)
		a.respond(w, rep, err)
	})
	mux.HandleFunc("POST /api/pick", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		path, err := pickFolder(ctx)
		if err != nil {
			writeErr(w, http.StatusNotImplemented, errors.New("the folder picker isn't available here; paste the folder path instead"))
			return
		}
		writeJSON(w, map[string]string{"path": path})
	})
	return a.guard(mux)
}

// guard blocks other websites: the Host must be this local server (stops DNS
// rebinding), and every API call needs the session token. POSTs must send it
// in a header, which other origins can't do without a CORS preflight we never
// answer.
func (a *App) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(a.Addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Host != a.Addr && r.Host != "localhost:"+port {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			tok := r.Header.Get("X-TidyDesk-Token")
			if tok == "" && r.Method == http.MethodGet {
				tok = r.URL.Query().Get("t")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(a.Token)) != 1 {
				http.Error(w, "missing or wrong session token", http.StatusForbidden)
				return
			}
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	a.visitedOnce.Do(func() { close(a.Visited) })
	page, _ := static.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(bytes.Replace(page, []byte("%TOKEN%"), []byte(a.Token), 1))
}

type status struct {
	State    string         `json:"state"`
	Message  string         `json:"message"`
	ScanID   int            `json:"scanId"`
	Progress *scan.Progress `json:"progress,omitempty"`
}

func (a *App) status() status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := status{State: a.state, Message: a.message, ScanID: a.scanID}
	if a.tracker != nil {
		p := a.tracker.Snapshot()
		st.Progress = &p
	}
	return st
}

func (a *App) hState(w http.ResponseWriter, r *http.Request) {
	st := a.status()
	a.mu.Lock()
	out := map[string]any{"status": st, "opts": a.opts, "trash": a.trashStatus(), "hasResult": a.res != nil}
	a.mu.Unlock()
	writeJSON(w, out)
}

// hEvents streams scan progress as server-sent events until the scan ends.
func (a *App) hEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		st := a.status()
		data, _ := json.Marshal(st)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
		if st.State != "scanning" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) hResult(w http.ResponseWriter, r *http.Request) {
	d, err := strconv.Atoi(r.URL.Query().Get("d"))
	if err != nil {
		d = scan.DefaultDistance
	}
	d = min(max(d, 0), 20)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.res == nil {
		writeErr(w, http.StatusNotFound, errors.New("no scan yet"))
		return
	}
	exact, similar := a.res.View(a.removed, d)
	files := []*scan.File{}
	seen := map[int]bool{}
	var sum struct {
		ExactBytes, SimilarBytes                             int64
		ExactFiles, SimilarFiles, ExactGroups, SimilarGroups int
	}
	for _, g := range slices.Concat(exact, similar) {
		for _, id := range g.Files {
			if !seen[id] {
				seen[id] = true
				files = append(files, a.res.Files[id])
			}
		}
		if g.Similar {
			sum.SimilarBytes += g.Wasted
			sum.SimilarFiles += len(g.Files) - 1
			sum.SimilarGroups++
		} else {
			sum.ExactBytes += g.Wasted
			sum.ExactFiles += len(g.Files) - 1
			sum.ExactGroups++
		}
	}
	writeJSON(w, map[string]any{
		"scanId": a.scanID, "roots": a.res.Roots, "elapsed": a.res.Elapsed.Seconds(),
		"filesScanned": len(a.res.Files), "errorCount": a.res.ErrorCount, "errors": a.res.Errors,
		"exact": exact, "similar": similar, "files": files, "distance": d,
		"summary": map[string]any{
			"exactBytes": sum.ExactBytes, "exactFiles": sum.ExactFiles, "exactGroups": sum.ExactGroups,
			"similarBytes": sum.SimilarBytes, "similarFiles": sum.SimilarFiles, "similarGroups": sum.SimilarGroups,
		},
	})
}

// hThumb returns a downscaled JPEG preview, generated on demand and cached.
func (a *App) hThumb(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	size := 160
	if r.URL.Query().Get("s") == "large" {
		size = 900
	}
	a.mu.Lock()
	var f *scan.File
	if a.res != nil && id >= 0 && id < len(a.res.Files) && a.res.Files[id].Thumb {
		f = a.res.Files[id]
		if _, err := a.check(f); err != nil {
			f = nil
		}
	}
	key := fmt.Sprintf("%d/%d/%d", a.scanID, id, size)
	a.mu.Unlock()
	if f == nil {
		http.NotFound(w, r)
		return
	}
	data := a.thumbs.get(key)
	if data == nil {
		a.thumbSem <- struct{}{}
		img, _, _, err := scan.DecodeImage(f.Path)
		if err == nil {
			var buf bytes.Buffer
			if err = jpeg.Encode(&buf, scan.Shrink(img, size), &jpeg.Options{Quality: 80}); err == nil {
				data = buf.Bytes()
				a.thumbs.put(key, data)
			}
		}
		<-a.thumbSem
		if err != nil {
			http.Error(w, "no preview", http.StatusUnsupportedMediaType)
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=600")
	w.Write(data)
}

func (a *App) respond(w http.ResponseWriter, rep any, err error) {
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"report": rep, "trash": a.TrashStatus()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad request"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// thumbCache is a small LRU cache limited by total bytes.
type thumbCache struct {
	mu        sync.Mutex
	max, size int
	ll        *list.List
	m         map[string]*list.Element
}

type thumbEntry struct {
	key  string
	data []byte
}

func newThumbCache(max int) *thumbCache {
	return &thumbCache{max: max, ll: list.New(), m: map[string]*list.Element{}}
}

func (c *thumbCache) get(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*thumbEntry).data
	}
	return nil
}

func (c *thumbCache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return
	}
	c.m[key] = c.ll.PushFront(&thumbEntry{key, data})
	c.size += len(data)
	for c.size > c.max {
		el := c.ll.Back()
		e := el.Value.(*thumbEntry)
		c.ll.Remove(el)
		delete(c.m, e.key)
		c.size -= len(e.data)
	}
}

func (c *thumbCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.m = map[string]*list.Element{}
	c.size = 0
}
