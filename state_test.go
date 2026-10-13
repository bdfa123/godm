package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Temp files left by a process that was killed between creating one and
// renaming it are swept up when a download starts in that folder, but only
// the old ones that are named like ours: a live one exists for an instant, and
// the folder is the user's.
func TestOldTempFilesAreSweptWhenADownloadStarts(t *testing.T) {
	srv := httptest.NewServer(&rangedHandler{payload: makePayload(1 << 10)})
	defer srv.Close()

	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	for name, age := range map[string]time.Time{
		".godm-123456789":  old, // left behind
		".godm-5":          time.Now(),
		".godm-notes":      old, // not ours
		"godm-123456789":   old,
		".godm-123456789x": old,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, age, age)
	}

	if _, err := Download(context.Background(), Options{URL: srv.URL + "/a.bin", OutDir: dir, Connections: 1}); err != nil {
		t.Fatal(err)
	}
	for _, name := range dirNames(t, dir) {
		if name == ".godm-123456789" {
			t.Errorf("the old temp file is still there: %v", dirNames(t, dir))
		}
	}
	if got := len(dirNames(t, dir)); got != 5 {
		t.Errorf("the folder holds %v, want a.bin and the four files that are not stale temp files", dirNames(t, dir))
	}
}

// A write whose rename fails leaves nothing behind. On Windows it fails while
// a scanner has the sidecar open, a sidecar is rewritten every 400 ms, and the
// callers go on without the write, so a temp file left each time would fill
// the downloads folder. Here the target is a folder, which no platform lets a
// file be renamed over.
func TestAWriteThatCannotBeRenamedLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "file.bin.godm")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := writeFileAtomic(target, []byte("x")); err == nil {
			t.Fatal("renaming a file over a folder worked")
		}
	}
	if got := dirNames(t, dir); len(got) != 1 || got[0] != "file.bin.godm" {
		t.Fatalf("the folder holds %v after failed writes, want just the target", got)
	}
}
