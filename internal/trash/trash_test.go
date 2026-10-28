package trash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveUndoEmpty(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "docs", "report.txt")
	os.MkdirAll(filepath.Dir(orig), 0o755)
	os.WriteFile(orig, []byte("hello"), 0o644)

	b, _ := Open(filepath.Join(dir, DirName))
	batch := NewBatchID()
	moved, failed := b.Move([]Item{{Path: orig, Size: 5}}, batch)
	if len(moved) != 1 || len(failed) != 0 {
		t.Fatalf("move: %v %v", moved, failed)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Fatal("file should be gone from its original place")
	}

	// a reopened bin must remember the move (manifest on disk)
	b2, err := Open(filepath.Join(dir, DirName))
	if err != nil || b2.LastBatch() != batch {
		t.Fatalf("manifest not reloaded: %v", err)
	}
	restored, failed := b2.Restore(batch)
	if len(restored) != 1 || len(failed) != 0 {
		t.Fatalf("restore: %v %v", restored, failed)
	}
	if data, _ := os.ReadFile(orig); string(data) != "hello" {
		t.Fatal("restored file content changed")
	}
	if _, err := os.Stat(filepath.Join(dir, DirName)); !os.IsNotExist(err) {
		t.Error("empty trash folder should be removed")
	}

	b2.Move([]Item{{Path: orig, Size: 5}}, NewBatchID())
	if n, bytes, _ := b2.Empty(); n != 1 || bytes != 5 {
		t.Errorf("empty: deleted %d files, %d bytes", n, bytes)
	}
	if files, _ := b2.Status(); files != 0 {
		t.Error("trash should be empty")
	}
}

func TestRestoreNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "a.txt")
	os.WriteFile(orig, []byte("old"), 0o644)
	b, _ := Open(filepath.Join(dir, DirName))
	batch := NewBatchID()
	b.Move([]Item{{Path: orig, Size: 3}}, batch)
	os.WriteFile(orig, []byte("new file in the same place"), 0o644)

	restored, failed := b.Restore(batch)
	if len(restored) != 0 || len(failed) != 1 {
		t.Fatalf("want refusal, got %v %v", restored, failed)
	}
	if data, _ := os.ReadFile(orig); string(data) != "new file in the same place" {
		t.Error("restore overwrote a newer file")
	}
}
