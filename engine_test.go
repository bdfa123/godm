package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makePayload(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(42))
	r.Read(b)
	return b
}

func sum(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// rangedHandler serves payload with correct 206 semantics, optionally cutting
// the connection short to exercise the retry path.
type rangedHandler struct {
	payload  []byte
	disp     string
	noRange  bool
	cutAfter int // bytes to send before hanging up; 0 disables
	cutsLeft atomic.Int32
	reqCount atomic.Int32

	mu     sync.Mutex
	ranges []string // Range headers as received
}

func (h *rangedHandler) requestedRanges() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ranges...)
}

func (h *rangedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.reqCount.Add(1)
	h.mu.Lock()
	h.ranges = append(h.ranges, r.Header.Get("Range"))
	h.mu.Unlock()
	if h.disp != "" {
		w.Header().Set("Content-Disposition", h.disp)
	}
	w.Header().Set("ETag", `"fixed-etag"`)

	rangeHdr := r.Header.Get("Range")
	if h.noRange || rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(h.payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(h.payload)
		return
	}

	var start, end int64
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, _ = strconv.ParseInt(parts[0], 10, 64)
	if len(parts) > 1 && parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	} else {
		end = int64(len(h.payload)) - 1
	}
	if end >= int64(len(h.payload)) {
		end = int64(len(h.payload)) - 1
	}
	if start > end {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	body := h.payload[start : end+1]
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)

	if h.cutAfter > 0 && h.cutsLeft.Add(-1) >= 0 && len(body) > h.cutAfter {
		w.Write(body[:h.cutAfter])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and slam the socket so the client sees a real truncated read.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	w.Write(body)
}

func runDownload(t *testing.T, srvURL, dir string, conns int) *Result {
	t.Helper()
	res, err := Download(context.Background(), Options{
		URL:         srvURL,
		OutDir:      dir,
		Connections: conns,
		MinSplit:    4 << 10,
		MaxRetries:  6,
	})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	return res
}

func TestSegmentedDownloadMatchesSource(t *testing.T) {
	payload := makePayload(3 << 20) // 3 MiB
	h := &rangedHandler{payload: payload, disp: `attachment; filename="big.bin"`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	res := runDownload(t, srv.URL+"/big.bin", dir, 8)

	// Idle connections may split ranges further, so 8 is the floor, not the count.
	if res.Segments < 8 {
		t.Errorf("expected at least 8 segments, got %d", res.Segments)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(payload) {
		t.Fatalf("content mismatch: got %d bytes (%s), want %d bytes (%s)",
			len(got), sum(got)[:12], len(payload), sum(payload)[:12])
	}
	if filepath.Base(res.Path) != "big.bin" {
		t.Errorf("filename = %q, want big.bin", filepath.Base(res.Path))
	}
	if _, err := os.Stat(statePath(res.Path)); !os.IsNotExist(err) {
		t.Error("state sidecar should be removed after success")
	}
}

func TestServerWithoutRangeFallsBackToSingleStream(t *testing.T) {
	payload := makePayload(512 << 10)
	h := &rangedHandler{payload: payload, noRange: true}
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	res := runDownload(t, srv.URL+"/plain.dat", dir, 8)

	if res.Segments != 1 {
		t.Errorf("a server that ignores Range must use 1 segment, got %d", res.Segments)
	}
	if res.Resumable {
		t.Error("Resumable should be false")
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch on single-stream path")
	}
}

func TestRetryAfterMidSegmentDisconnect(t *testing.T) {
	payload := makePayload(2 << 20)
	h := &rangedHandler{payload: payload, cutAfter: 40 << 10}
	h.cutsLeft.Store(4) // kill the first four ranged responses part-way
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	res := runDownload(t, srv.URL+"/flaky.bin", dir, 4)

	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(payload) {
		t.Fatalf("flaky transfer corrupted the file: got %d bytes, want %d", len(got), len(payload))
	}
	if h.reqCount.Load() <= 5 {
		t.Errorf("expected retries beyond the initial requests, saw %d", h.reqCount.Load())
	}
}

func TestResumeFromSidecarSkipsFinishedBytes(t *testing.T) {
	payload := makePayload(1 << 20)
	h := &rangedHandler{payload: payload}
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "resume.bin")

	// Pretend a previous run wrote the first half of a two-segment plan.
	half := int64(len(payload) / 2)
	f, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(int64(len(payload)))
	f.WriteAt(payload[:half], 0)
	f.Close()

	if err := saveState(target, &State{
		Version:  stateVersion,
		URL:      srv.URL + "/resume.bin",
		FinalURL: srv.URL + "/resume.bin",
		Size:     int64(len(payload)),
		ETag:     `"fixed-etag"`,
		Filename: "resume.bin",
		Segments: []SegSnap{
			{Start: 0, End: half - 1, Done: half},
			{Start: half, End: int64(len(payload)) - 1, Done: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}

	before := h.reqCount.Load()
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/resume.bin", OutDir: dir, Filename: "resume.bin",
		Connections: 2, MinSplit: 4 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed {
		t.Fatal("expected the sidecar to be reused")
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("resumed file does not match source")
	}
	// Connections may share the missing half between them, but none of them
	// may ask for bytes from the half that was already on disk.
	_ = before
	for _, rg := range h.requestedRanges() {
		if rg == "" || rg == "bytes=0-0" {
			continue
		}
		var start int64
		fmt.Sscanf(rg, "bytes=%d-", &start)
		if start < half {
			t.Errorf("resume refetched finished bytes: %s", rg)
		}
	}
}

// A download that was given a numbered name because the plain one is someone
// else's is still the same download when it is run again: it carries on in
// that file instead of starting another beside it. The command line has no
// stored name to hand back, so the sidecar is all that says whose it is.
func TestADownloadThatGotANumberedNameResumesInIt(t *testing.T) {
	payload := makePayload(4 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 10 * time.Millisecond})
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.bin"), []byte("somebody else's"), 0o644)
	opts := Options{URL: srv.URL + "/f.bin", OutDir: dir, Connections: 1, MaxRetries: 1}

	ctx, cancel := context.WithCancel(context.Background())
	first := opts
	first.OnProgress = func(p Progress) {
		if p.Received >= 512<<10 {
			cancel()
		}
	}
	if _, err := Download(ctx, first); err == nil {
		t.Fatal("the first run was not interrupted")
	}
	numbered := filepath.Join(dir, "f (1).bin")
	if !pathExists(statePath(numbered)) {
		t.Fatalf("the interrupted run left no sidecar for %s: %v", numbered, dirNames(t, dir))
	}

	res, err := Download(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != numbered || !res.Resumed {
		t.Fatalf("second run went to %s (resumed %v), want to carry on in %s", res.Path, res.Resumed, numbered)
	}
	checkFile(t, numbered, payload)
	if got := dirNames(t, dir); len(got) != 2 {
		t.Errorf("the folder holds %v, want just f.bin and f (1).bin", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "f.bin")); string(got) != "somebody else's" {
		t.Errorf("the other file was changed to %q", got)
	}
}

// The same, when the earlier attempt can no longer be continued: it starts
// over in its own file rather than in a third.
func TestANumberedNameIsRestartedInPlaceWhenItCannotResume(t *testing.T) {
	payload := makePayload(256 << 10)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.bin"), []byte("somebody else's"), 0o644)
	numbered := filepath.Join(dir, "f (1).bin")
	os.WriteFile(numbered, make([]byte, len(payload)), 0o644)
	saveState(numbered, &State{
		Version: stateVersion, URL: srv.URL + "/f.bin", Size: int64(len(payload)),
		ETag: `"an-old-etag"`, Filename: "f (1).bin",
		Segments: []SegSnap{{Start: 0, End: int64(len(payload)) - 1, Done: int64(len(payload))}},
	})

	res, err := Download(context.Background(), Options{URL: srv.URL + "/f.bin", OutDir: dir, Connections: 2, MinSplit: 4 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != numbered || res.Resumed {
		t.Fatalf("went to %s (resumed %v), want a fresh start in %s", res.Path, res.Resumed, numbered)
	}
	checkFile(t, numbered, payload)
	if got := dirNames(t, dir); len(got) != 2 {
		t.Errorf("the folder holds %v, want just f.bin and f (1).bin", got)
	}
}

// Another link's partial download under a numbered name is not ours to touch.
func TestAnotherLinksNumberedPartialIsLeftAlone(t *testing.T) {
	payload := makePayload(64 << 10)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.bin"), []byte("somebody else's"), 0o644)
	numbered := filepath.Join(dir, "f (1).bin")
	os.WriteFile(numbered, make([]byte, len(payload)), 0o644)
	saveState(numbered, &State{
		Version: stateVersion, URL: srv.URL + "/another.bin", Size: int64(len(payload)),
		Filename: "f (1).bin", Segments: []SegSnap{{Start: 0, End: int64(len(payload)) - 1, Done: 100}},
	})

	res, err := Download(context.Background(), Options{URL: srv.URL + "/f.bin", OutDir: dir, Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "f (2).bin"); res.Path != want {
		t.Fatalf("went to %s, want %s beside the other link's partial file", res.Path, want)
	}
	if st, ok := loadState(numbered); !ok || st.URL != srv.URL+"/another.bin" {
		t.Errorf("the other link's sidecar was disturbed: %+v", st)
	}
}

// S3 and nginx answer a range request for an object with nothing in it with
// 416, where the same request without a range gets an empty 200. A browser
// saves the empty file, and so does this.
func TestAnEmptyFileIsSavedFromAServerThatAnswers416ToTheProbe(t *testing.T) {
	var ranged, plain atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
			w.Header().Set("Content-Range", "bytes */0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		plain.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := Download(context.Background(), Options{URL: srv.URL + "/empty.txt", OutDir: dir, MaxRetries: 1})
	if err != nil {
		t.Fatalf("an empty file could not be downloaded: %v", err)
	}
	if fi, err := os.Stat(res.Path); err != nil || fi.Size() != 0 || filepath.Base(res.Path) != "empty.txt" {
		t.Fatalf("saved %s (%v, %v), want an empty empty.txt", res.Path, fi, err)
	}
	if got := dirNames(t, dir); len(got) != 1 {
		t.Errorf("the folder holds %v, want just the file", got)
	}
}

// A 416 from the probe is no verdict on the file when it is not empty: asking
// again without a range gets the whole of it as one stream.
func TestA416ToTheProbeFallsBackToOneStream(t *testing.T) {
	payload := makePayload(100 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	res, err := Download(context.Background(), Options{URL: srv.URL + "/odd.bin", OutDir: t.TempDir(), MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, res.Path, payload)
	if res.Resumable {
		t.Error("a server that refused every range was taken for one that resumes")
	}
}

// When the plain request fails too, that is the answer given, not the 416.
func TestAProbeThatFailsWithAndWithoutARangeReportsTheSecondAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := Download(context.Background(), Options{URL: srv.URL + "/gone.bin", OutDir: t.TempDir(), MaxRetries: 1})
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusNotFound {
		t.Fatalf("error %v, want the 404", err)
	}
}

// The sidecar says which bytes are done, and a resume believes it. If it
// reached the disk before the bytes did, a power cut leaves ranges marked done
// that are zeros in a file of the right length, which nothing would notice. So
// whatever a sidecar claims must have been flushed before it was written.
func TestTheSidecarNeverClaimsMoreThanWasFlushed(t *testing.T) {
	defer func(f func(*os.File) error, every time.Duration) { syncData, dataSyncEvery = f, every }(syncData, dataSyncEvery)
	dataSyncEvery = 600 * time.Millisecond

	// What the file held at the last flush that completed.
	var mu sync.Mutex
	var flushed []byte
	var flushes int
	syncData = func(f *os.File) error {
		err := f.Sync()
		b, _ := os.ReadFile(f.Name())
		mu.Lock()
		flushed, flushes = b, flushes+1
		mu.Unlock()
		// A flush takes time, and the connections write on through it. What
		// they write meanwhile is not on disk yet and must not be claimed.
		time.Sleep(60 * time.Millisecond)
		return err
	}

	payload := makePayload(8 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 10 * time.Millisecond})
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "big.bin")
	var claimed int64
	var lastReceived atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Download(ctx, Options{
		URL: srv.URL + "/big.bin", OutDir: dir, Connections: 2, MinSplit: 64 << 10, MaxRetries: 1,
		// This runs just before the supervisor saves again, so the sidecar it
		// reads is the one saved a tick ago, and the last flush is no older.
		OnProgress: func(p Progress) {
			lastReceived.Store(p.Received)
			if p.Received >= 5<<20 {
				cancel()
			}
			st, ok := loadState(target)
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range st.Segments {
				claimed += s.Done
				lo, hi := s.Start, s.Start+s.Done
				if hi > int64(len(flushed)) || !bytes.Equal(flushed[lo:hi], payload[lo:hi]) {
					t.Errorf("the sidecar says bytes %d to %d are done; after the last flush the file holds %d bytes there that match",
						lo, hi, matching(flushed, payload, lo, hi))
					cancel()
					return
				}
			}
		},
	})
	if err == nil {
		t.Fatal("the download was not interrupted")
	}
	mu.Lock()
	defer mu.Unlock()
	if flushes < 2 || claimed == 0 {
		t.Fatalf("%d flushes and %d bytes claimed: the test looked at nothing", flushes, claimed)
	}

	// An interrupted download keeps all that it got: the lag is for the
	// sidecar of a running one, and ends when it stops.
	st, ok := loadState(target)
	if !ok {
		t.Fatal("no sidecar after the interruption")
	}
	var done int64
	for _, s := range st.Segments {
		done += s.Done
	}
	if done < lastReceived.Load() {
		t.Errorf("the sidecar after stopping holds %d bytes, %d were reported received", done, lastReceived.Load())
	}
}

// matching counts how many of payload[lo:hi] the file holds, for a message.
func matching(file, payload []byte, lo, hi int64) int64 {
	var n int64
	for i := lo; i < hi && i < int64(len(file)); i++ {
		if file[i] == payload[i] {
			n++
		}
	}
	return n
}

func TestStaleSidecarIsDiscardedWhenETagChanges(t *testing.T) {
	payload := makePayload(256 << 10)
	h := &rangedHandler{payload: payload}
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "stale.bin")
	os.WriteFile(target, make([]byte, len(payload)), 0o644)
	saveState(target, &State{
		Version: stateVersion, URL: srv.URL + "/stale.bin", Size: int64(len(payload)),
		ETag: `"an-old-etag"`, Filename: "stale.bin",
		Segments: []SegSnap{{Start: 0, End: int64(len(payload)) - 1, Done: int64(len(payload))}},
	})

	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/stale.bin", OutDir: dir, Filename: "stale.bin", Connections: 2, MinSplit: 4 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed {
		t.Fatal("a changed ETag must invalidate the sidecar")
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch after discarding stale state")
	}
}

func TestParseContentRangeTotal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes 0-0/1234", 1234, true},
		{"bytes 0-99/100", 100, true},
		{"bytes 0-0/*", 0, false},
		{"nonsense", 0, false},
		{"", 0, false},
		{"bytes 0-0/", 0, false},
	}
	for _, c := range cases {
		got, ok := parseContentRangeTotal(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseContentRangeTotal(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFilenameResolution(t *testing.T) {
	cases := []struct {
		name string
		disp string
		url  string
		want string
	}{
		{"rfc5987 utf8", `attachment; filename*=UTF-8''%E4%B8%AD%E6%96%87.zip`, "http://x/y", "中文.zip"},
		{"plain quoted", `attachment; filename="report final.pdf"`, "http://x/y", "report final.pdf"},
		{"prefers rfc5987", `attachment; filename="fallback.bin"; filename*=UTF-8''real.bin`, "http://x/y", "real.bin"},
		{"url basename", "", "http://x/files/setup.exe", "setup.exe"},
		{"url percent escape", "", "http://x/files/my%20app.dmg", "my app.dmg"},
		{"strips path", `attachment; filename="/etc/passwd"`, "http://x/y", "passwd"},
		{"strips traversal", `attachment; filename="..\\..\\evil.exe"`, "http://x/y", "evil.exe"},
		{"illegal chars", `attachment; filename="a:b*c?.txt"`, "http://x/y", "a_b_c_.txt"},
		{"reserved name", `attachment; filename="CON.txt"`, "http://x/y", "_CON.txt"},
		{"no hints", "", "http://x/", "download"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			resp := &http.Response{Header: http.Header{}, Request: req}
			if c.disp != "" {
				resp.Header.Set("Content-Disposition", c.disp)
			}
			if got := ResolveFilename("", resp); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestFreshSegmentsCoverExactlyOnce(t *testing.T) {
	for _, size := range []int64{1, 999, 1 << 20, (3 << 20) + 7, 1<<30 + 1} {
		pr := &ProbeResult{Size: size, Resumable: true}
		o := Options{Connections: 8, MinSplit: 1 << 16}
		o.applyDefaults()
		o.Connections, o.MinSplit = 8, 1<<16
		segs := freshSegments(pr, o)

		var total int64
		var next int64
		for i, s := range segs {
			if s.Start != next {
				t.Fatalf("size %d: segment %d starts at %d, expected %d", size, i, s.Start, next)
			}
			if s.End() < s.Start {
				t.Fatalf("size %d: segment %d is empty", size, i)
			}
			total += s.End() - s.Start + 1
			next = s.End() + 1
		}
		if total != size {
			t.Errorf("size %d: segments cover %d bytes", size, total)
		}
		if next != size {
			t.Errorf("size %d: last segment ends at %d", size, next-1)
		}
	}
}

func TestUnresumableSourceUsesOneSegment(t *testing.T) {
	o := Options{}
	o.applyDefaults()
	if n := len(freshSegments(&ProbeResult{Size: 1 << 30, Resumable: false}, o)); n != 1 {
		t.Errorf("non-resumable source split into %d segments", n)
	}
	if n := len(freshSegments(&ProbeResult{Size: -1, Resumable: true}, o)); n != 1 {
		t.Errorf("unknown size split into %d segments", n)
	}
}
