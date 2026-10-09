package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stallingServer serves a file by range, except that the first request for
// the range starting at stallAt sends all but the last held bytes and then
// goes quiet with the socket still open, the way an overloaded origin does.
// Every other range trickles, so the stalled connection has its bytes before
// the others finish and there is nothing left big enough to split.
type stallingServer struct {
	payload []byte
	stallAt int64
	held    int
	release chan struct{} // closed by the test so Close is not left waiting

	stalls atomic.Int32
	mu     sync.Mutex
	ranges []string
}

func (h *stallingServer) requested() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ranges...)
}

func (h *stallingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.ranges = append(h.ranges, r.Header.Get("Range"))
	h.mu.Unlock()
	start, end := rangeOf(r, len(h.payload))
	body := h.payload[start : end+1]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	f, _ := w.(http.Flusher)

	if start == h.stallAt && len(body) > h.held && h.stalls.Add(1) == 1 {
		w.Write(body[:len(body)-h.held])
		f.Flush()
		select {
		case <-r.Context().Done():
		case <-h.release:
		}
		return
	}
	for len(body) > 0 {
		n := min(64<<10, len(body))
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		f.Flush()
		body = body[n:]
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// The case that was reproduced: four connections, one of them stalls with
// 96 KiB of its range to go, the other three finish everything else, and the
// remainder is too small for dynamic splitting to take. Before the watchdog
// this sat at 97.7% forever.
func TestStalledConnectionIsRetriedFromWhereItStopped(t *testing.T) {
	payload := makePayload(4 << 20)
	h := &stallingServer{payload: payload, stallAt: 3 << 20, held: 96 << 10, release: make(chan struct{})}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(h.release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Download(ctx, Options{
		URL: srv.URL + "/stall.bin", OutDir: t.TempDir(), Connections: 4,
		StallTimeout: 300 * time.Millisecond,
	})
	if ctx.Err() != nil {
		t.Fatalf("the download hung on a connection that went quiet: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, res.Path, payload)
	if h.stalls.Load() == 0 {
		t.Fatal("the test never stalled a connection")
	}
	// The retry picks up at the first missing byte rather than starting the
	// range over.
	want := fmt.Sprintf("bytes=%d-%d", len(payload)-h.held, len(payload)-1)
	if got := h.requested(); !contains(got, want) {
		t.Errorf("no request for %s after the stall; requests were %v", want, got)
	}
}

func TestStalledHLSSegmentIsRetried(t *testing.T) {
	segs := makeSegments(6)
	release := make(chan struct{})
	var stalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
			for i := range segs {
				fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
			return
		}
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/seg"), ".ts"))
		if err != nil || i < 0 || i >= len(segs) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(segs[i])))
		if i == 2 && stalls.Add(1) == 1 {
			w.Write(segs[i][:len(segs[i])/2])
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Write(segs[i])
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := DownloadHLS(ctx, HLSOptions{
		URL: srv.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts",
		Connections: 2, StallTimeout: 300 * time.Millisecond,
	})
	if ctx.Err() != nil {
		t.Fatalf("the stream download hung on a segment that went quiet: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("the file is not the segments in order")
	}
	if stalls.Load() < 2 {
		t.Fatal("the stalled segment was never asked for again")
	}
}

// A server that ignores Range sends the whole file to the probe, which reads
// a little of it so the connection can be reused. It must not wait there on a
// server that stops sending.
func TestProbeDoesNotWaitOnAStalledBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1<<10))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pr, err := Probe(ctx, newClient(), Options{URL: srv.URL + "/f.bin", StallTimeout: 200 * time.Millisecond})
	if ctx.Err() != nil {
		t.Fatal("the probe hung reading the body of a server that went quiet")
	}
	if err != nil {
		t.Fatal(err)
	}
	if pr.Resumable || pr.Size != 1<<20 {
		t.Errorf("probe = %+v, want a 1 MiB file that cannot be resumed", pr)
	}
}

// trickleReader hands out one byte per Read straight away, or blocks until
// cancelled once it runs dry.
type trickleReader struct {
	left int
	stop chan struct{}
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		<-r.stop
		return 0, context.Canceled
	}
	r.left--
	p[0] = 'x'
	return 1, nil
}

// Time a connection spends waiting on a speed limit, or writing what it got,
// is not time the server kept it waiting.
func TestStallGuardOnlyCountsTimeInsideRead(t *testing.T) {
	src := &trickleReader{left: 3, stop: make(chan struct{})}
	var cancelled atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(src.stop) }) }
	g := newStallGuard(src, 100*time.Millisecond, func() {
		cancelled.Store(true)
		unblock()
	})
	defer g.stop()
	// If the guard never fires, end the blocked read anyway so the test fails
	// instead of hanging.
	defer time.AfterFunc(5*time.Second, unblock).Stop()

	buf := make([]byte, 8)
	for i := 0; i < 3; i++ {
		if _, err := g.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		time.Sleep(250 * time.Millisecond) // well past the timeout, outside Read
	}
	if cancelled.Load() {
		t.Fatal("the guard fired while nobody was reading")
	}

	start := time.Now()
	_, err := g.Read(buf)
	var se *stallError
	if !errors.As(err, &se) {
		t.Fatalf("a read the server never answered returned %v, want a stall", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > 2*time.Second {
		t.Errorf("the stall was called after %v, want about 100ms", d)
	}
	if !retryable(err) {
		t.Error("a stall must be retried")
	}
}
