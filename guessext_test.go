package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// headOf is the start of a file: prefix, then zeros up to sniffSize, with
// mark written in at at when it is given.
func headOf(prefix []byte, at int, mark string) []byte {
	head := make([]byte, sniffSize)
	copy(head, prefix)
	if mark != "" {
		copy(head[at:], mark)
	}
	return head
}

// packets is n transport stream packets of size bytes, the sync byte at offset.
func packets(size, offset, n int) []byte {
	b := make([]byte, size*n)
	for i := range n {
		b[i*size+offset] = 0x47
	}
	return b
}

var ebml = []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0x82, 0x88}

func TestTheFirstBytesNameTheKindOfFile(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"matroska", headOf(append(append([]byte{}, ebml...), "matroska"...), 0, ""), ".mkv"},
		{"webm", headOf(append(append([]byte{}, ebml...), "webm"...), 0, ""), ".webm"},
		{"mp4", headOf([]byte("\x00\x00\x00\x20ftypisom"), 0, ""), ".mp4"},
		{"quicktime", headOf([]byte("\x00\x00\x00\x14ftypqt  "), 0, ""), ".mov"},
		{"m4a", headOf([]byte("\x00\x00\x00\x20ftypM4A "), 0, ""), ".m4a"},
		{"a picture in the same boxes", headOf([]byte("\x00\x00\x00\x18ftypheic"), 0, ""), ""},
		{"avi", headOf([]byte("RIFF\x00\x00\x00\x00AVI LIST"), 0, ""), ".avi"},
		{"wav is not a film", headOf([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), 0, ""), ""},
		{"transport stream", headOf(packets(188, 0, 12), 0, ""), ".ts"},
		{"blu-ray stream", headOf(packets(192, 4, 12), 0, ""), ".m2ts"},
		{"iso 9660", headOf(nil, 32<<10+1, "CD001"), ".iso"},
		{"udf, as a blu-ray image is", headOf(nil, 32<<10+1, "BEA01"), ".iso"},
		{"flac", headOf([]byte("fLaC"), 0, ""), ".flac"},
		{"zip", headOf([]byte("PK\x03\x04"), 0, ""), ".zip"},
		{"rar", headOf([]byte("Rar!\x1a\x07\x01\x00"), 0, ""), ".rar"},
		{"7z", headOf([]byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}, 0, ""), ".7z"},
		{"pdf", headOf([]byte("%PDF-1.7"), 0, ""), ".pdf"},
		{"a program is never named one", headOf([]byte("MZ\x90\x00"), 0, ""), ""},
		{"anything else", makePayload(sniffSize), ""},
		{"nothing at all", nil, ""},
		{"too little to tell", []byte{0x47}, ""},
		{"a disc image cut short", headOf(nil, 0, "")[:32<<10], ""},
	}
	for _, c := range cases {
		if got := sniffedExtension(c.head); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOnlyANameWithNoExtensionIsGivenOne(t *testing.T) {
	for name, want := range map[string]bool{
		"A63FC880ABF65C1D80CF37F769B2224320A7FF88": true,
		"download":         true,
		"Mr. Robot S01E01": true,
		"film.mkv":         false,
		"archive.tar.gz":   false,
		"Some.Film.2160p":  false, // reads as an extension, and is left alone
	} {
		if got := extensionless(name); got != want {
			t.Errorf("extensionless(%q) = %v, want %v", name, got, want)
		}
	}
}

// filmNamedByHash is a Matroska file served under a name with no extension.
func filmNamedByHash(t *testing.T, h http.Handler) string {
	t.Helper()
	return newTestServer(t, h).URL + "/A63FC880ABF65C1D80CF37F769B2224320A7FF88"
}

func filmPayload(n int) []byte {
	payload := makePayload(n)
	copy(payload, append(append([]byte{}, ebml...), "matroska"...))
	return payload
}

func TestANameWithNoExtensionGetsTheOneItsFileCallsFor(t *testing.T) {
	payload := filmPayload(1 << 20)
	h := &rangedHandler{payload: payload}
	dir := t.TempDir()
	res, err := Download(context.Background(), Options{URL: filmNamedByHash(t, h), OutDir: dir, Connections: 2, GuessExtension: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(res.Path); got != "A63FC880ABF65C1D80CF37F769B2224320A7FF88.mkv" {
		t.Errorf("saved as %q, want the name with .mkv", got)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, payload) {
		t.Fatal("the file is not what was served")
	}
	if !slices.Contains(h.requestedRanges(), fmt.Sprintf("bytes=0-%d", sniffSize-1)) {
		t.Errorf("the start of the file was never read on its own: %v", h.requestedRanges())
	}
}

func TestANameIsLeftAloneWhenItMayNotOrNeedNotChange(t *testing.T) {
	const hash = "A63FC880ABF65C1D80CF37F769B2224320A7FF88"
	cases := []struct {
		name   string
		h      *rangedHandler
		guess  bool
		want   string
		sniffs bool // whether the start of the file is read on its own
	}{
		{"not asked to", &rangedHandler{payload: filmPayload(256 << 10)}, false, hash, false},
		{"it has one", &rangedHandler{payload: filmPayload(256 << 10), disp: `attachment; filename="film.bin"`}, true, "film.bin", false},
		{"nothing known in it", &rangedHandler{payload: makePayload(256 << 10)}, true, hash, true},
		// Without ranges the start could only be had with all of the rest.
		{"the server takes no ranges", &rangedHandler{payload: filmPayload(256 << 10), noRange: true}, true, hash, false},
	}
	for _, c := range cases {
		res, err := Download(context.Background(), Options{URL: filmNamedByHash(t, c.h), OutDir: t.TempDir(), Connections: 2, GuessExtension: c.guess})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := filepath.Base(res.Path); got != c.want {
			t.Errorf("%s: saved as %q, want %q", c.name, got, c.want)
		}
		if got := slices.Contains(c.h.requestedRanges(), fmt.Sprintf("bytes=0-%d", sniffSize-1)); got != c.sniffs {
			t.Errorf("%s: the start of the file read on its own %v, want %v", c.name, got, c.sniffs)
		}
	}
}

// refusesFirstSniff serves a film slowly, but turns away the first request for
// the start of it on its own, as a busy server might.
type refusesFirstSniff struct {
	*slowServer
	refused atomic.Bool
}

func (h *refusesFirstSniff) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") == fmt.Sprintf("bytes=0-%d", sniffSize-1) && h.refused.CompareAndSwap(false, true) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	h.slowServer.ServeHTTP(w, r)
}

// A download the daemon continues keeps the name its partial file has, even
// when what it holds would now give it an extension: a new name would be a
// new file, and the part already fetched would be lost.
func TestAContinuedDownloadKeepsItsName(t *testing.T) {
	m := listManager(t)
	payload := filmPayload(2 << 20)
	h := &refusesFirstSniff{slowServer: &slowServer{payload: payload, chunk: 8 << 10, delay: 10 * time.Millisecond}}
	id, err := m.Add(jobRequest{URL: filmNamedByHash(t, h), Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to start", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Received > 0
	})
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	first, _ := findTask(m, id)
	if filepath.Base(first.Path) != "A63FC880ABF65C1D80CF37F769B2224320A7FF88" {
		t.Fatalf("with the start of the file refused it is saved as %q, want the name as served", filepath.Base(first.Path))
	}
	m.Resume(id)
	waitFor(t, 60*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if v.Path != first.Path {
		t.Errorf("continued under %q, want %q", v.Path, first.Path)
	}
	if got, _ := os.ReadFile(v.Path); !bytes.Equal(got, payload) {
		t.Fatal("the file is not what was served")
	}
	entries, _ := os.ReadDir(filepath.Dir(v.Path))
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the folder holds %v, want the one file", names)
	}
}

// And a fresh one in the daemon is named by what it holds, which the manager
// then knows for a video.
func TestTheDaemonNamesAFreshDownloadByWhatItHolds(t *testing.T) {
	m := listManager(t)
	h := &rangedHandler{payload: filmPayload(512 << 10)}
	id, err := m.Add(jobRequest{URL: filmNamedByHash(t, h), Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Filename != "A63FC880ABF65C1D80CF37F769B2224320A7FF88.mkv" {
		t.Errorf("the download is called %q, want the name with .mkv", v.Filename)
	}
}
