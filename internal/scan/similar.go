package scan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders
	_ "image/png"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// DefaultDistance is the default dHash Hamming distance for "similar" (0-64).
const DefaultDistance = 8

const maxPixels = 80_000_000 // larger images are skipped to keep memory sane

// Group is a set of duplicate files or similar photos.
type Group struct {
	ID       string `json:"id"`
	Similar  bool   `json:"similar"`
	Files    []int  `json:"files"`
	Keep     int    `json:"keep"`   // suggested file to keep
	Reason   string `json:"reason"` // why that one
	Wasted   int64  `json:"wasted"` // bytes freed by keeping only the suggested file
	Distance int    `json:"distance"`
}

// DecodeImage opens a JPEG or PNG and returns it with its size.
func DecodeImage(path string) (image.Image, int, int, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer fh.Close()
	cfg, _, err := image.DecodeConfig(bufio.NewReader(fh))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("not a readable image (%v)", err)
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, 0, 0, errors.New("image too large to compare")
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	img, _, err := image.Decode(bufio.NewReader(fh))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("not a readable image (%v)", err)
	}
	return img, cfg.Width, cfg.Height, nil
}

// lum returns the brightness (0-255) of one pixel, with fast paths for the
// image types the standard decoders produce.
func lum(img image.Image, x, y int) float64 {
	switch m := img.(type) {
	case *image.YCbCr:
		return float64(m.Y[m.YOffset(x, y)])
	case *image.Gray:
		return float64(m.Pix[m.PixOffset(x, y)])
	case *image.NRGBA:
		p := m.Pix[m.PixOffset(x, y):]
		return 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
	case *image.RGBA:
		p := m.Pix[m.PixOffset(x, y):]
		return 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
	}
	r, g, b, _ := img.At(x, y).RGBA()
	return (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 257
}

// DHash is a 64-bit difference hash: shrink to 9x8 grey cells and record
// whether each cell is darker than its right neighbour. Similar pictures get
// hashes that differ in only a few bits.
func DHash(img image.Image) uint64 {
	const w, h = 9, 8
	b := img.Bounds()
	var cells [w * h]float64
	for cy := range h {
		y0, y1 := b.Min.Y+cy*b.Dy()/h, b.Min.Y+(cy+1)*b.Dy()/h
		for cx := range w {
			x0, x1 := b.Min.X+cx*b.Dx()/w, b.Min.X+(cx+1)*b.Dx()/w
			// average up to 8x8 evenly spaced samples per cell
			sy, sx := max((y1-y0)/8, 1), max((x1-x0)/8, 1)
			var sum float64
			n := 0
			for y := y0; y < max(y1, y0+1); y += sy {
				for x := x0; x < max(x1, x0+1); x += sx {
					sum += lum(img, x, y)
					n++
				}
			}
			cells[cy*w+cx] = sum / float64(n)
		}
	}
	var hash uint64
	for y := range h {
		for x := range w - 1 {
			hash <<= 1
			if cells[y*w+x] < cells[y*w+x+1] {
				hash |= 1
			}
		}
	}
	return hash
}

// Shrink returns a copy that fits in size x size, averaging pixel blocks.
func Shrink(img image.Image, size int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return img
	}
	scale := float64(size) / float64(max(w, h))
	nw, nh := max(int(float64(w)*scale), 1), max(int(float64(h)*scale), 1)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		sy := max((y1-y0)/4, 1)
		for x := range nw {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			sx := max((x1-x0)/4, 1)
			var r, g, bl, n uint32
			for yy := y0; yy < max(y1, y0+1); yy += sy {
				for xx := x0; xx < max(x1, x0+1); xx += sx {
					cr, cg, cb, _ := img.At(xx, yy).RGBA()
					r, g, bl, n = r+cr>>8, g+cg>>8, bl+cb>>8, n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r/n), uint8(g/n), uint8(bl/n), 255
		}
	}
	return dst
}

func hashPhotos(ctx context.Context, files []*File, t *Tracker) []int {
	var photos []*File
	for _, f := range files {
		if f.Thumb {
			photos = append(photos, f)
		}
	}
	t.setPhase("photos", 4, int64(len(photos)))
	parallel(ctx, photos, func(f *File) {
		defer t.done.Add(1)
		img, w, h, err := DecodeImage(f.Path)
		if err != nil {
			t.fail(f.Path, err)
			return
		}
		f.Width, f.Height = w, h
		f.dhash, f.hasDHash = DHash(img), true
	})
	var ids []int
	for _, f := range photos {
		if f.hasDHash {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// Groups returns every exact duplicate set and every similar-photo set for a
// maximum Hamming distance, ignoring removals. Similar sets that only hold
// exact copies of one file are left out (they are already duplicates).
func (r *Result) Groups(maxDist int) (exact, similar [][]int) {
	n := len(r.photos)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := range n {
		hi := r.Files[r.photos[i]].dhash
		for j := i + 1; j < n; j++ {
			if bits.OnesCount64(hi^r.Files[r.photos[j]].dhash) <= maxDist {
				if a, b := find(i), find(j); a != b {
					parent[a] = b
				}
			}
		}
	}
	comps := map[int][]int{}
	for i := range n {
		root := find(i)
		comps[root] = append(comps[root], r.photos[i])
	}
	for _, c := range comps {
		contents := map[int]bool{}
		for _, id := range c {
			k := -id - 1
			if s := r.Files[id].set; s > 0 {
				k = s
			}
			contents[k] = true
		}
		if len(contents) > 1 {
			slices.Sort(c)
			similar = append(similar, c)
		}
	}
	sort.Slice(similar, func(i, j int) bool { return similar[i][0] < similar[j][0] })
	return r.sets, similar
}

// View returns the groups to show: removed files are left out, and groups
// with fewer than two files left are dropped.
func (r *Result) View(removed map[int]bool, maxDist int) (exact, similar []Group) {
	ex, sim := r.Groups(maxDist)
	build := func(sets [][]int, isSim bool) []Group {
		out := []Group{}
		for _, s := range sets {
			var ids []int
			for _, id := range s {
				if !removed[id] {
					ids = append(ids, id)
				}
			}
			if len(ids) > 1 {
				out = append(out, r.group(ids, isSim))
			}
		}
		return out
	}
	return build(ex, false), build(sim, true)
}

var (
	copyName = regexp.MustCompile(`(?i)(\bcopy\b|kopie|duplicate|\(\d+\)$)`)
	messyDir = regexp.MustCompile(`(?i)\b(downloads?|temp|tmp|backups?|cache|old|copies)\b`)
)

// penalty scores how much a file looks like a stray copy: a copy-like name,
// or a downloads/temp/backup folder inside the scanned folder.
func (r *Result) penalty(f *File) int {
	p := 0
	if copyName.MatchString(strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))) {
		p += 2
	}
	dir := filepath.Dir(f.Path)
	for _, root := range r.Roots {
		if rel, err := filepath.Rel(root, dir); err == nil && Within(dir, root) {
			dir = rel
			break
		}
	}
	if messyDir.MatchString(dir) {
		p++
	}
	return p
}

// tidier orders copies of the same content: original-looking first, then
// oldest, then shortest path. It returns the order and the deciding reason.
func (r *Result) tidier(a, b *File) (bool, string) {
	if pa, pb := r.penalty(a), r.penalty(b); pa != pb {
		return pa < pb, "its name and folder look like the original"
	}
	if !a.ModTime.Equal(b.ModTime) {
		return a.ModTime.Before(b.ModTime), "it is the oldest copy"
	}
	if len(a.Path) != len(b.Path) {
		return len(a.Path) < len(b.Path), "it has the shortest path"
	}
	return a.Path < b.Path, "it comes first alphabetically"
}

func (r *Result) group(ids []int, isSim bool) Group {
	fs := make([]*File, len(ids))
	for i, id := range ids {
		fs[i] = r.Files[id]
	}
	g := Group{Similar: isSim, Files: ids, ID: fmt.Sprintf("e%d", ids[0])}
	if !isSim {
		sort.SliceStable(fs, func(i, j int) bool { less, _ := r.tidier(fs[i], fs[j]); return less })
		_, g.Reason = r.tidier(fs[0], fs[1])
		g.Keep = fs[0].ID
		g.Wasted = fs[0].Size * int64(len(fs)-1)
		return g
	}

	// Similar photos: keep the most pixels, then the largest file.
	g.ID = fmt.Sprintf("s%d", ids[0])
	better := func(a, b *File) (bool, string) {
		if pa, pb := a.Width*a.Height, b.Width*b.Height; pa != pb {
			return pa > pb, "it has the most pixels"
		}
		if a.Size != b.Size {
			return a.Size > b.Size, "it is the largest file"
		}
		return r.tidier(a, b)
	}
	sort.SliceStable(fs, func(i, j int) bool { less, _ := better(fs[i], fs[j]); return less })
	keep := fs[0]
	g.Keep = keep.ID
	_, g.Reason = better(keep, fs[1])
	for _, f := range fs[1:] {
		g.Wasted += f.Size
		g.Distance = max(g.Distance, bits.OnesCount64(keep.dhash^f.dhash))
	}
	return g
}
