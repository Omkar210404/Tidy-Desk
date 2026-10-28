// Package trash moves files into a "TidyDesk Trash" folder and can put them back.
// A manifest.json next to the moved files remembers where each one came from.
package trash

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DirName is the trash folder created inside each scanned folder.
const DirName = "TidyDesk Trash"

const manifestName = "manifest.json"

// Item is a file to move.
type Item struct {
	Path string
	Size int64
}

// Entry records one moved file.
type Entry struct {
	Original string    `json:"original"`
	Stored   string    `json:"stored"` // path inside the trash folder
	Size     int64     `json:"size"`
	Batch    string    `json:"batch"`
	MovedAt  time.Time `json:"movedAt"`
}

// Bin is one TidyDesk Trash folder.
type Bin struct {
	Dir     string
	mu      sync.Mutex
	entries []Entry
}

var (
	batchMu   sync.Mutex
	lastBatch string
)

// NewBatchID returns a sortable ID; every Remove gets its own batch so Undo
// can put back exactly that move.
func NewBatchID() string {
	batchMu.Lock()
	defer batchMu.Unlock()
	id := time.Now().Format("20060102-150405.000")
	if id <= lastBatch {
		id = lastBatch + "1"
	}
	lastBatch = id
	return id
}

// Open loads a trash folder's manifest. The folder need not exist yet.
func Open(dir string) (*Bin, error) {
	b := &Bin{Dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	var m struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the TidyDesk Trash manifest in %s is damaged: %w", dir, err)
	}
	b.entries = m.Entries
	return b, nil
}

// save writes the manifest atomically, or removes it when the trash is empty.
func (b *Bin) save() error {
	path := filepath.Join(b.Dir, manifestName)
	if len(b.entries) == 0 {
		err := os.Remove(path)
		os.Remove(b.Dir) // only succeeds if the folder is empty
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}{1, b.entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Move moves files into the trash under one batch. Each chunk is written to
// the manifest before the files move, so a crash can never lose track of a
// moved file.
func (b *Bin) Move(items []Item, batch string) (moved []Item, failed []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for start := 0; start < len(items); start += 100 {
		chunk := items[start:min(start+100, len(items))]
		base := len(b.entries)
		for i, it := range chunk {
			b.entries = append(b.entries, Entry{
				Original: it.Path, Size: it.Size, Batch: batch, MovedAt: time.Now(),
				Stored: filepath.Join(batch, fmt.Sprintf("%d_%s", start+i+1, filepath.Base(it.Path))),
			})
		}
		if err := b.save(); err != nil {
			b.entries = b.entries[:base]
			for _, it := range chunk {
				failed = append(failed, it.Path+": couldn't write the TidyDesk Trash manifest ("+err.Error()+")")
			}
			continue
		}
		keep := b.entries[:base]
		for i, it := range chunk {
			e := b.entries[base+i]
			dst := filepath.Join(b.Dir, e.Stored)
			err := os.MkdirAll(filepath.Dir(dst), 0o755)
			if err == nil {
				err = os.Rename(it.Path, dst)
			}
			if err != nil {
				failed = append(failed, it.Path+": "+err.Error())
				continue
			}
			keep = append(keep, e)
			moved = append(moved, it)
		}
		b.entries = keep
		if err := b.save(); err != nil {
			failed = append(failed, "couldn't update the TidyDesk Trash manifest: "+err.Error())
		}
	}
	return moved, failed
}

// Restore puts every file of a batch back at its original path. It never
// overwrites a file that has appeared there since.
func (b *Bin) Restore(batch string) (restored []Entry, failed []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keep []Entry
	for _, e := range b.entries {
		if e.Batch != batch {
			keep = append(keep, e)
			continue
		}
		if !filepath.IsLocal(e.Stored) || !filepath.IsAbs(e.Original) {
			failed = append(failed, e.Original+": invalid manifest entry")
			keep = append(keep, e)
			continue
		}
		src := filepath.Join(b.Dir, e.Stored)
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, e.Original+": missing from TidyDesk Trash")
			continue
		}
		if _, err := os.Lstat(e.Original); err == nil {
			failed = append(failed, e.Original+": a file already exists there, so it was left in TidyDesk Trash")
			keep = append(keep, e)
			continue
		}
		err := os.MkdirAll(filepath.Dir(e.Original), 0o755)
		if err == nil {
			err = os.Rename(src, e.Original)
		}
		if err != nil {
			failed = append(failed, e.Original+": "+err.Error())
			keep = append(keep, e)
			continue
		}
		restored = append(restored, e)
	}
	b.entries = keep
	os.Remove(filepath.Join(b.Dir, batch)) // only if empty
	if err := b.save(); err != nil {
		failed = append(failed, "couldn't update the TidyDesk Trash manifest: "+err.Error())
	}
	return restored, failed
}

// Empty permanently deletes the files listed in the manifest.
func (b *Bin) Empty() (deleted int, bytes int64, failed []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keep []Entry
	batches := map[string]bool{}
	for _, e := range b.entries {
		batches[e.Batch] = true
		if !filepath.IsLocal(e.Stored) {
			keep = append(keep, e)
			continue
		}
		err := os.Remove(filepath.Join(b.Dir, e.Stored))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, e.Original+": "+err.Error())
			keep = append(keep, e)
			continue
		}
		deleted++
		bytes += e.Size
	}
	for batch := range batches {
		os.Remove(filepath.Join(b.Dir, batch))
	}
	b.entries = keep
	if err := b.save(); err != nil {
		failed = append(failed, "couldn't update the TidyDesk Trash manifest: "+err.Error())
	}
	return deleted, bytes, failed
}

// Status returns how many files and bytes are in the trash.
func (b *Bin) Status() (files int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entries {
		files++
		bytes += e.Size
	}
	return files, bytes
}

// LastBatch returns the newest batch ID, or "" when empty.
func (b *Bin) LastBatch() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	last := ""
	for _, e := range b.entries {
		last = max(last, e.Batch)
	}
	return last
}

// BatchStatus returns the files and bytes in one batch.
func (b *Bin) BatchStatus(batch string) (files int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entries {
		if e.Batch == batch {
			files++
			bytes += e.Size
		}
	}
	return files, bytes
}
