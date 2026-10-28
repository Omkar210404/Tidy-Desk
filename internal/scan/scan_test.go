package scan

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func filled(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func run(t *testing.T, opts Options) *Result {
	t.Helper()
	r, err := Run(context.Background(), opts, NewTracker())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExactDuplicates(t *testing.T) {
	dir := t.TempDir()
	data := filled(50_000, 1)
	write(t, dir, "a/photo.bin", data)
	write(t, dir, "b/photo copy.bin", data)
	// same size and same first/last 4 KB, different middle: must not match
	other := bytes.Clone(data)
	other[25_000] ^= 0xFF
	write(t, dir, "c/almost.bin", other)
	write(t, dir, "d/unique.bin", filled(12_345, 9))

	r := run(t, Options{Roots: []string{dir}})
	exact, _ := r.View(nil, DefaultDistance)
	if len(exact) != 1 || len(exact[0].Files) != 2 {
		t.Fatalf("want 1 group of 2, got %+v", exact)
	}
	if keep := r.Files[exact[0].Keep].Path; filepath.Base(keep) != "photo.bin" {
		t.Errorf("suggested keep = %s, want photo.bin", keep)
	}
}

func TestMinSizeAndKindFilters(t *testing.T) {
	dir := t.TempDir()
	small := filled(500, 2)
	write(t, dir, "x.txt", small)
	write(t, dir, "y.txt", small)
	if r := run(t, Options{Roots: []string{dir}, MinSize: 1024}); len(r.Files) != 0 {
		t.Errorf("min size: want 0 files, got %d", len(r.Files))
	}
	if r := run(t, Options{Roots: []string{dir}, Kind: "photos"}); len(r.Files) != 0 {
		t.Errorf("kind filter: want 0 files, got %d", len(r.Files))
	}
}

func scene(w, h int, phase float64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := 128 + 100*math.Sin(float64(x)/(20+phase*9)+phase)*math.Cos(float64(y)/15-phase)
			img.Set(x, y, color.RGBA{uint8(v), uint8(255 - v), 90, 255})
		}
	}
	return img
}

func TestSimilarPhotos(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	png.Encode(&buf, scene(300, 200, 0))
	write(t, dir, "original.png", buf.Bytes())
	buf.Reset()
	jpeg.Encode(&buf, Shrink(scene(300, 200, 0), 150), &jpeg.Options{Quality: 75})
	write(t, dir, "resized.jpg", buf.Bytes())
	buf.Reset()
	png.Encode(&buf, scene(300, 200, 3))
	write(t, dir, "different.png", buf.Bytes())

	r := run(t, Options{Roots: []string{dir}, Similar: true})
	_, similar := r.View(nil, DefaultDistance)
	if len(similar) != 1 || len(similar[0].Files) != 2 {
		t.Fatalf("want 1 similar group of 2, got %+v", similar)
	}
	for _, id := range similar[0].Files {
		if filepath.Base(r.Files[id].Path) == "different.png" {
			t.Error("different.png should not be grouped")
		}
	}
	if keep := r.Files[similar[0].Keep]; keep.Width != 300 {
		t.Errorf("should keep the larger image, kept %dx%d", keep.Width, keep.Height)
	}
}

func TestCleanRootsDropsNested(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	roots, err := CleanRoots([]string{sub, dir})
	if err != nil || len(roots) != 1 {
		t.Fatalf("want 1 root, got %v %v", roots, err)
	}
	if _, err := CleanRoots([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing folder should be an error")
	}
}
