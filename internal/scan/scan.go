// Package scan walks folders and finds exact duplicate files and similar photos.
package scan

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TrashDirName is never scanned, so TidyDesk doesn't find its own trash.
const TrashDirName = "TidyDesk Trash"

// Options controls what a scan looks at.
type Options struct {
	Roots   []string `json:"roots"`
	MinSize int64    `json:"minSize"`
	Kind    string   `json:"kind"` // all, photos, videos, documents
	Exclude []string `json:"exclude"`
	Similar bool     `json:"similar"`
}

// File is one file found by a scan. ID is its index in Result.Files.
type File struct {
	ID      int       `json:"id"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Kind    string    `json:"kind"`
	Thumb   bool      `json:"thumb"` // JPEG or PNG: previews and similarity work
	Width   int       `json:"width,omitempty"`
	Height  int       `json:"height,omitempty"`

	quick, full [32]byte
	whole       bool // the quick hash already covered the whole file
	failed      bool
	dhash       uint64
	hasDHash    bool
	set         int // 1-based exact duplicate set, 0 if unique
}

// Result is a finished scan.
type Result struct {
	Roots      []string
	Files      []*File
	Errors     []string // first few problems, for display
	ErrorCount int
	Elapsed    time.Duration
	Hashed     int64 // bytes read for hashing

	sets   [][]int // exact duplicate sets (file IDs)
	photos []int   // files with a perceptual hash
}

const (
	edge    = 4096 // bytes read from each end for the quick hash
	maxErrs = 100
)

var (
	extKinds = map[string]string{}
	// Folders that are never scanned: system, recycle bins, version control internals.
	skipAlways = set("$recycle.bin", "system volume information", "$windows.~bt", "$windows.~ws",
		"windowsapps", ".git", ".svn", ".hg", strings.ToLower(TrashDirName))
	// Folders skipped only directly under a drive root, e.g. C:\Windows or /proc.
	skipAtRoot = set("windows", "program files", "program files (x86)", "programdata", "recovery",
		"$sysreset", "proc", "sys", "dev", "run", "boot", "usr", "bin", "sbin", "lib", "etc", "var",
		"snap", "system", "library", "private", "cores")
)

func init() {
	for kind, exts := range map[string]string{
		"photo":    "jpg jpeg png gif bmp webp heic heif tif tiff dng cr2 nef arw raw",
		"video":    "mp4 mov avi mkv wmv m4v webm 3gp flv mpg mpeg",
		"document": "pdf doc docx xls xlsx ppt pptx odt ods odp rtf txt csv md epub",
	} {
		for _, e := range strings.Fields(exts) {
			extKinds[e] = kind
		}
	}
}

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range items {
		m[s] = true
	}
	return m
}

func ext(name string) string { return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".") }

// KindOf returns photo, video, document or other.
func KindOf(name string) string {
	if k, ok := extKinds[ext(name)]; ok {
		return k
	}
	return "other"
}

func wantKind(filter, kind string) bool {
	switch filter {
	case "photos":
		return kind == "photo"
	case "videos":
		return kind == "video"
	case "documents":
		return kind == "document"
	}
	return true
}

// Within reports whether path is root or inside it.
func Within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// CleanRoots resolves the chosen folders to real absolute paths and drops
// folders that sit inside another chosen folder, so nothing is scanned twice.
func CleanRoots(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err == nil {
			abs, err = filepath.EvalSymlinks(abs)
		}
		var info fs.FileInfo
		if err == nil {
			info, err = os.Stat(abs)
		}
		if err != nil {
			return nil, fmt.Errorf("can't open the folder %q", p)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%q is a file, not a folder", p)
		}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil, errors.New("choose at least one folder to scan")
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	var roots []string
	for _, p := range out {
		if !slices.ContainsFunc(roots, func(r string) bool { return Within(p, r) }) {
			roots = append(roots, p)
		}
	}
	return roots, nil
}

// Progress is a snapshot of a running scan.
type Progress struct {
	Phase   string  `json:"phase"` // walk, quick, full, photos, done
	Step    int     `json:"step"`
	Files   int64   `json:"files"`
	Hashed  int64   `json:"hashed"`
	Done    int64   `json:"done"`  // work done in this step (files or bytes)
	Total   int64   `json:"total"` // work in this step, 0 = unknown
	Groups  int64   `json:"groups"`
	Errors  int64   `json:"errors"`
	ETA     float64 `json:"eta"` // seconds left in this step, -1 = unknown
	Elapsed float64 `json:"elapsed"`
}

// Tracker collects progress from scan workers. Safe for concurrent use.
type Tracker struct {
	start      time.Time
	mu         sync.Mutex
	phase      string
	step       int
	phaseStart time.Time
	errs       []string

	files, hashed, done, total, groups, errors atomic.Int64
}

func NewTracker() *Tracker {
	now := time.Now()
	return &Tracker{start: now, phase: "walk", step: 1, phaseStart: now}
}

func (t *Tracker) setPhase(phase string, step int, total int64) {
	t.mu.Lock()
	t.phase, t.step, t.phaseStart = phase, step, time.Now()
	t.mu.Unlock()
	t.done.Store(0)
	t.total.Store(total)
}

func (t *Tracker) fail(path string, err error) {
	t.errors.Add(1)
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	t.mu.Lock()
	if len(t.errs) < maxErrs {
		t.errs = append(t.errs, path+": "+err.Error())
	}
	t.mu.Unlock()
}

func (t *Tracker) Snapshot() Progress {
	t.mu.Lock()
	p := Progress{Phase: t.phase, Step: t.step}
	inPhase := time.Since(t.phaseStart).Seconds()
	t.mu.Unlock()
	p.Files, p.Hashed, p.Done, p.Total = t.files.Load(), t.hashed.Load(), t.done.Load(), t.total.Load()
	p.Groups, p.Errors = t.groups.Load(), t.errors.Load()
	p.Elapsed = time.Since(t.start).Seconds()
	p.ETA = -1
	if p.Total > 0 && p.Done > 0 && inPhase > 0.5 {
		p.ETA = float64(p.Total-p.Done) / (float64(p.Done) / inPhase)
	}
	return p
}

func workers() int { return min(max(runtime.NumCPU(), 2), 8) }

// Run scans the folders in opts. It stops early if ctx is cancelled.
func Run(ctx context.Context, opts Options, t *Tracker) (*Result, error) {
	roots, err := CleanRoots(opts.Roots)
	if err != nil {
		return nil, err
	}
	files := walk(ctx, roots, opts, t)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i, f := range files {
		f.ID = i
	}
	r := &Result{Roots: roots, Files: files}
	r.sets = findExact(ctx, files, t)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Similar && (opts.Kind == "" || opts.Kind == "all" || opts.Kind == "photos") {
		r.photos = hashPhotos(ctx, files, t)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, sim := r.Groups(DefaultDistance)
		t.groups.Add(int64(len(sim)))
	}
	t.setPhase("done", 5, 0)
	t.mu.Lock()
	r.Errors = append([]string{}, t.errs...)
	t.mu.Unlock()
	r.ErrorCount = int(t.errors.Load())
	r.Hashed = t.hashed.Load()
	r.Elapsed = time.Since(t.start)
	return r, nil
}

// walk lists files concurrently. Each folder is read by its own goroutine,
// and a semaphore limits how many folders are read at once.
func walk(ctx context.Context, roots []string, opts Options, t *Tracker) []*File {
	var (
		mu    sync.Mutex
		files []*File
		wg    sync.WaitGroup
	)
	sem := make(chan struct{}, workers())
	excluded := excludeMatcher(opts.Exclude)
	minSize := max(opts.MinSize, 1)

	var visit func(dir string)
	visit = func(dir string) {
		defer wg.Done()
		if ctx.Err() != nil {
			return
		}
		sem <- struct{}{}
		defer func() { <-sem }()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.fail(dir, err) // entries may still hold what was read
		}
		var local []*File
		for _, e := range entries {
			name, typ := e.Name(), e.Type()
			p := filepath.Join(dir, name)
			switch {
			case typ&(fs.ModeSymlink|fs.ModeIrregular) != 0:
				// links, junctions and special files are skipped
			case e.IsDir():
				if !skipDir(dir, name) && !excluded(p, name) {
					wg.Add(1)
					go visit(p)
				}
			case typ.IsRegular():
				kind := KindOf(name)
				if !wantKind(opts.Kind, kind) {
					continue
				}
				info, err := e.Info()
				if err != nil {
					t.fail(p, err)
					continue
				}
				if info.Size() < minSize {
					continue
				}
				e := ext(name)
				local = append(local, &File{Path: p, Size: info.Size(), ModTime: info.ModTime(), Kind: kind,
					Thumb: e == "jpg" || e == "jpeg" || e == "png"})
			}
		}
		t.files.Add(int64(len(local)))
		mu.Lock()
		files = append(files, local...)
		mu.Unlock()
	}
	for _, r := range roots {
		wg.Add(1)
		go visit(r)
	}
	wg.Wait()
	return files
}

func skipDir(parent, name string) bool {
	n := strings.ToLower(name)
	return skipAlways[n] || (filepath.Dir(parent) == parent && skipAtRoot[n])
}

// excludeMatcher matches full paths (and everything inside) or bare folder names.
func excludeMatcher(list []string) func(path, name string) bool {
	var paths []string
	names := map[string]bool{}
	for _, e := range list {
		e = strings.Trim(strings.TrimSpace(e), `"`)
		switch {
		case e == "":
		case filepath.IsAbs(e):
			paths = append(paths, filepath.Clean(e))
		default:
			names[strings.ToLower(e)] = true
		}
	}
	return func(path, name string) bool {
		return names[strings.ToLower(name)] || slices.ContainsFunc(paths, func(p string) bool { return Within(path, p) })
	}
}

// parallel runs fn on every file with a fixed pool of workers.
func parallel(ctx context.Context, items []*File, fn func(*File)) {
	ch := make(chan *File)
	var wg sync.WaitGroup
	for range workers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				fn(f)
			}
		}()
	}
feed:
	for _, f := range items {
		select {
		case ch <- f:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
}

// findExact narrows candidates in stages: same size, then same quick hash
// (first and last 4 KB), then same full SHA-256.
func findExact(ctx context.Context, files []*File, t *Tracker) [][]int {
	bySize := map[int64][]*File{}
	for _, f := range files {
		bySize[f.Size] = append(bySize[f.Size], f)
	}
	var cands []*File
	for _, g := range bySize {
		if len(g) > 1 {
			cands = append(cands, g...)
		}
	}

	t.setPhase("quick", 2, int64(len(cands)))
	parallel(ctx, cands, func(f *File) {
		if err := quickHash(f, t); err != nil {
			f.failed = true
			t.fail(f.Path, err)
		}
		t.done.Add(1)
	})

	type key struct {
		size int64
		sum  [32]byte
	}
	byQuick := map[key][]*File{}
	for _, f := range cands {
		if !f.failed {
			k := key{f.Size, f.quick}
			byQuick[k] = append(byQuick[k], f)
		}
	}
	var need []*File
	var total int64
	for _, g := range byQuick {
		for _, f := range g {
			if len(g) > 1 && !f.whole {
				need = append(need, f)
				total += f.Size
			}
		}
	}

	t.setPhase("full", 3, total)
	parallel(ctx, need, func(f *File) {
		if err := fullHash(ctx, f, t); err != nil {
			f.failed = true
			if ctx.Err() == nil {
				t.fail(f.Path, err)
			}
		}
	})

	byFull := map[key][]*File{}
	for _, g := range byQuick {
		for _, f := range g {
			if len(g) < 2 || f.failed {
				continue
			}
			sum := f.full
			if f.whole {
				sum = f.quick
			}
			k := key{f.Size, sum}
			byFull[k] = append(byFull[k], f)
		}
	}
	var sets [][]int
	for _, g := range byFull {
		if len(g) < 2 {
			continue
		}
		ids := make([]int, len(g))
		for i, f := range g {
			ids[i] = f.ID
		}
		slices.Sort(ids)
		sets = append(sets, ids)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i][0] < sets[j][0] })
	for i, s := range sets {
		for _, id := range s {
			files[id].set = i + 1
		}
	}
	t.groups.Store(int64(len(sets)))
	return sets
}

func quickHash(f *File, t *Tracker) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	if f.Size <= 2*edge {
		n, err := io.Copy(h, fh)
		t.hashed.Add(n)
		if err != nil {
			return err
		}
		if n != f.Size {
			return errors.New("file changed during the scan")
		}
		f.whole = true
	} else {
		buf := make([]byte, edge)
		if _, err := io.ReadFull(fh, buf); err != nil {
			return err
		}
		h.Write(buf)
		if _, err := fh.ReadAt(buf, f.Size-edge); err != nil {
			return err
		}
		h.Write(buf)
		t.hashed.Add(2 * edge)
	}
	h.Sum(f.quick[:0])
	return nil
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// fullHash streams the file through SHA-256 in 1 MB chunks.
func fullHash(ctx context.Context, f *File, t *Tracker) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	h := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := fh.Read(*bp)
		if n > 0 {
			h.Write((*bp)[:n])
			t.hashed.Add(int64(n))
			t.done.Add(int64(n))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	h.Sum(f.full[:0])
	return nil
}
