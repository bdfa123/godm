package main

import (
	"bytes"
	"context"
	"fmt"
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
	n, err := diskUse(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// diskUse is onDisk for where a test cannot stop: off its own goroutine.
func diskUse(p string) (int64, error) {
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	var hi uint32
	lo, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&hi)))
	if uint32(lo) == math.MaxUint32 && callErr != syscall.Errno(0) {
		return 0, fmt.Errorf("GetCompressedFileSize %s: %w", p, callErr)
	}
	return int64(hi)<<32 | int64(uint32(lo)), nil
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
	const size = 4 << 30
	f, err := os.OpenFile(filepath.Join(dir, "big.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := prepareFile(f, size, true); err != nil {
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
	var free int64
	var asked []string
	diskOfItsOwn(t, func(d string) int64 {
		asked = append(asked, d)
		return free
	})
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

// A torrent's pieces arrive in any order, so its files are sparse too, from
// before the first piece is written to them until the torrent is complete.
func TestTorrentFilesAreSparseUntilTheTorrentIsDone(t *testing.T) {
	src := t.TempDir()
	a, b := makePayload(1<<20+12345), makePayload(1<<20+777)
	mi := makeTorrent(t, src, "Show", 64<<10, tfile{"e01.mkv", a}, tfile{"extras/e02.mkv", b})
	// Slow enough to be seen in the middle.
	seed := newSeeder(t, src, mi, 1<<20)

	m := newBTManager(t)
	needSparseFiles(t, m.OutDir())
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "Show", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	tor := waitLoaded(t, m, id)
	root := filepath.Join(m.OutDir(), "Show")
	files := []string{filepath.Join(root, "e01.mkv"), filepath.Join(root, "extras", "e02.mkv")}
	seen := map[string]bool{}
	waitFor(t, 60*time.Second, "the torrent to finish", func() bool {
		for _, p := range files {
			if seen[p] {
				continue
			}
			fi, err := os.Stat(p)
			if err != nil || fi.Size() == 0 {
				continue
			}
			sparse := isSparse(t, p)
			if tor.Complete().Bool() {
				// The look may have come after the files were made ordinary.
				continue
			}
			seen[p] = true
			if !sparse {
				t.Errorf("%s was written to without being made sparse first", filepath.Base(p))
			}
		}
		v, _ := findTask(m, id)
		return v.State == StateDone
	})
	for _, p := range files {
		if !seen[p] {
			t.Logf("%s finished before it could be looked at", filepath.Base(p))
		}
		if isSparse(t, p) {
			t.Errorf("%s is still sparse once the torrent is done", filepath.Base(p))
		}
	}
	if len(seen) == 0 {
		t.Fatal("no file was seen while the torrent downloaded")
	}
	for p, want := range map[string][]byte{files[0]: a, files[1]: b} {
		checkFile(t, p, want)
	}
}

// A torrent whose files would not fit is stopped once its size is known,
// before any of it is written.
func TestATorrentThatDoesNotFitIsRefused(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(2 << 20)
	mi := makeTorrent(t, src, "film.bin", 64<<10, tfile{data: payload})
	m := newBTManager(t)
	diskOfItsOwn(t, func(string) int64 { return 1 << 20 })
	var body bytes.Buffer
	if err := mi.Write(&body); err != nil {
		t.Fatal(err)
	}
	id, err := m.AddTorrentFile(body.Bytes(), "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the torrent to be refused", stateIs(m, id, StateError))
	v, _ := findTask(m, id)
	if !strings.Contains(v.Error, "not enough free space") {
		t.Errorf("the torrent stopped with %q, want it refused for want of space", v.Error)
	}
	if fi, err := os.Stat(v.Path); err == nil && fi.Size() != 0 {
		t.Errorf("%d bytes of a torrent refused for want of space were written", fi.Size())
	}
}

// An older godm gave a download's file its whole length, which took all its
// space up at once: such a file resumes even on a disk with less free than
// that. Once sparse, its first writes past what it had been written give the
// space in between back to the disk, and that space is still the download's
// to fill, not free for the next one to take.
func TestAnOrdinaryPartialFileStillNeedsTheSpaceItGivesBack(t *testing.T) {
	dir := t.TempDir()
	needSparseFiles(t, dir)
	const size = 64 << 20
	payload := makePayload(size)
	// A megabyte and a bit at the start of each range, a full write's worth.
	held := &heldServer{payload: payload, first: 1<<20 + 64<<10, hold: make(chan struct{})}
	srv := httptest.NewServer(held)
	defer srv.Close()
	defer close(held.hold)

	path := filepath.Join(dir, "film.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(size)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, path); got < size {
		t.Skipf("an ordinary file of %d bytes takes %d bytes of the disk here", size, got)
	}
	url := srv.URL + "/film.mkv"
	st := &State{Version: stateVersion, URL: url, FinalURL: url, Size: size, Filename: "film.mkv"}
	for i := int64(0); i < 4; i++ {
		st.Segments = append(st.Segments, SegSnap{Start: i * size / 4, End: (i+1)*size/4 - 1})
	}
	if err := saveState(path, st); err != nil {
		t.Fatal(err)
	}
	// 32 MB free, and whatever the file gives back.
	diskOfItsOwn(t, func(string) int64 {
		used, err := diskUse(path)
		if err != nil {
			return 32 << 20
		}
		return 32<<20 + max(size-used, 0)
	})

	stop := startHeld(t, Options{URL: url, OutDir: dir, Connections: 4, MinSplit: 1 << 20})
	defer stop()
	waitFor(t, 20*time.Second, "the resumed file to give back what it had not been written", func() bool {
		return onDisk(t, path) < size/2
	})
	gave := size - onDisk(t, path)
	other := t.TempDir()
	if release, err := claimRoom(other, needs(40<<20)); err == nil {
		release()
		t.Errorf("40 MB was given beside a download that gave back %d MB it still has to write, with 32 MB free besides",
			gave>>20)
	}
	release, err := claimRoom(other, needs(24<<20))
	if err != nil {
		t.Fatalf("24 MB of the 32 MB free besides what the other download gave back: %v", err)
	}
	release()
}

// The claims of the downloads on one disk add up whichever folder on it they
// are in, and however its name is written.
func TestEveryFolderOnADiskIsOnTheOneDisk(t *testing.T) {
	dir := t.TempDir()
	v := volumeOf(dir)
	if v == "" {
		t.Fatalf("no disk was found for %s", dir)
	}
	for _, d := range []string{
		filepath.Join(dir, "not", "made", "yet"),
		strings.ToUpper(dir),
		filepath.VolumeName(dir) + `\`,
	} {
		if got := volumeOf(d); got != v {
			t.Errorf("%s is on %q, and %s on %q", d, got, dir, v)
		}
	}
}
