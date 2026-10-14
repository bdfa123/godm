package main

import (
	"bytes"
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const fileAttributeSparse = 0x200 // FILE_ATTRIBUTE_SPARSE_FILE

var procGetCompressedFileSize = kernel32.NewProc("GetCompressedFileSizeW")

// sparseAt reports whether the file at p is marked sparse.
func sparseAt(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.Sys().(*syscall.Win32FileAttributeData).FileAttributes&fileAttributeSparse != 0, nil
}

func isSparse(t *testing.T, p string) bool {
	t.Helper()
	sparse, err := sparseAt(p)
	if err != nil {
		t.Fatal(err)
	}
	return sparse
}

// onDisk is how much of the disk the file at p takes up, which for a sparse
// file is only what has been written to it.
func onDisk(t *testing.T, p string) int64 {
	t.Helper()
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	var hi uint32
	lo, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&hi)))
	if uint32(lo) == math.MaxUint32 && callErr != syscall.Errno(0) {
		t.Fatalf("GetCompressedFileSize %s: %v", p, callErr)
	}
	return int64(hi)<<32 | int64(uint32(lo))
}

// needSparseFiles skips a test where dir is on a file system that has no
// sparse files. The tests run on NTFS, here and on GitHub's runners.
func needSparseFiles(t *testing.T, dir string) {
	t.Helper()
	f, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := setSparse(f, true); err != nil {
		t.Skipf("%s has no sparse files: %v", dir, err)
	}
}

// plentyOfRoom stands in for the free space, for a test that is not about it.
func plentyOfRoom(t *testing.T) {
	old := freeSpace
	freeSpace = func(string) (int64, bool) { return math.MaxInt64, true }
	t.Cleanup(func() { freeSpace = old })
}

// The connections of a download write all over its file from the start. On
// NTFS a write far into an ordinary file first has the file filled with zeros
// up to there, which on a hard disk held a download of 85 GB at 124 KB for
// many minutes, so the file is sparse while it downloads. Finished, it is an
// ordinary file again.
func TestADownloadIsSparseUntilItIsDone(t *testing.T) {
	dir := t.TempDir()
	needSparseFiles(t, dir)
	payload := makePayload(4 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 20 * time.Millisecond})
	defer srv.Close()

	var mu sync.Mutex
	var path string
	var lookErr error
	looked, sparse := 0, 0
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/film.mkv", OutDir: dir, Connections: 4, MinSplit: 64 << 10,
		OnStart: func(si StartInfo) {
			mu.Lock()
			path = si.Path
			mu.Unlock()
		},
		OnProgress: func(Progress) {
			mu.Lock()
			defer mu.Unlock()
			s, err := sparseAt(path)
			if err != nil {
				lookErr = err
				return
			}
			looked++
			if s {
				sparse++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookErr != nil {
		t.Fatal(lookErr)
	}
	if looked == 0 {
		t.Fatal("the download never reported progress, so nothing was looked at")
	}
	if sparse != looked {
		t.Errorf("the file was sparse at %d of %d looks while it downloaded, want all", sparse, looked)
	}
	checkFile(t, res.Path, payload)
	if isSparse(t, res.Path) {
		t.Error("the finished file is still sparse")
	}
	if got := onDisk(t, res.Path); got < int64(len(payload)) {
		t.Errorf("the finished file takes %d bytes of disk for %d bytes of data", got, len(payload))
	}
}

// A file prepared for a download of several gigabytes takes a write near its
// end at once and gives it only the space it needs: nothing is filled in
// ahead of it.
func TestAWriteFarIntoAPreparedFileWaitsForNoZeros(t *testing.T) {
	dir := t.TempDir()
	needSparseFiles(t, dir)
	plentyOfRoom(t)
	const size = 4 << 30
	f, err := os.OpenFile(filepath.Join(dir, "big.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := prepareFile(f, dir, size, true); err != nil {
		t.Fatal(err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != size {
		t.Fatalf("the prepared file is %v bytes (%v), want %d", fi.Size(), err, size)
	}
	chunk := bytes.Repeat([]byte{7}, 1<<20)
	start := time.Now()
	if _, err := f.WriteAt(chunk, size-int64(len(chunk))); err != nil {
		t.Fatal(err)
	}
	// Only once the write has reached the disk does the space show.
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if got := onDisk(t, f.Name()); got > 16<<20 {
		t.Errorf("after one megabyte written near the end, the file takes %d MB of disk", got>>20)
	}
	if took > 5*time.Second {
		t.Errorf("the write near the end took %v", took)
	}
	t.Logf("a megabyte written %d GB into the file took %v", size>>30, took)
}

// Sparse files take their space only as they are written, so whether a
// download fits is checked before it starts rather than found out at 90
// percent. What a partial file already takes up counts as there.
func TestADownloadThatDoesNotFitIsRefused(t *testing.T) {
	dir := t.TempDir()
	needSparseFiles(t, dir)
	payload := makePayload(4 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 20 * time.Millisecond})
	defer srv.Close()
	old := freeSpace
	defer func() { freeSpace = old }()
	var free int64
	var asked []string
	freeSpace = func(d string) (int64, bool) {
		asked = append(asked, d)
		return free, true
	}
	opts := Options{URL: srv.URL + "/film.mkv", OutDir: dir, Connections: 2, MinSplit: 64 << 10}

	free = 3 << 20
	_, err := Download(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("a 4 MB download with 3 MB free: %v, want it refused for want of space", err)
	}
	if len(asked) == 0 || asked[0] != dir {
		t.Errorf("the free space was asked of %q, want the download folder %q", asked, dir)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("a download refused before it began left %v behind", names)
	}

	// Half of it, stopped: the partial file holds what came.
	free = math.MaxInt64
	ctx, cancel := context.WithCancel(context.Background())
	half := opts
	half.OnProgress = func(p Progress) {
		if p.Received >= 2<<20 {
			cancel()
		}
	}
	if _, err := Download(ctx, half); err == nil {
		t.Fatal("the download was not interrupted")
	}
	st, ok := loadState(filepath.Join(dir, "film.mkv"))
	if !ok {
		t.Fatal("no sidecar after the interruption")
	}
	var done int64
	for _, s := range st.Segments {
		done += s.Done
	}

	// What is still to come fits, though the whole file would not.
	free = int64(len(payload)) - done
	res, err := Download(context.Background(), opts)
	if err != nil {
		t.Fatalf("resuming with room for the %d bytes still to come: %v", free, err)
	}
	checkFile(t, res.Path, payload)
}

