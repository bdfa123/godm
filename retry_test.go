package main

import (
	"context"
	"errors"
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

// throttlingHandler answers 429 for the first n ranged requests, then serves
// the bytes normally.
type throttlingHandler struct {
	payload    []byte
	throttles  atomic.Int32
	retryAfter string
	total      atomic.Int32
	got429     atomic.Int32
}

func (h *throttlingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.total.Add(1)
	w.Header().Set("ETag", `"t"`)

	if h.throttles.Add(-1) >= 0 {
		h.got429.Add(1)
		if h.retryAfter != "" {
			w.Header().Set("Retry-After", h.retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(h.payload)))
		w.Write(h.payload)
		return
	}
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, _ := strconv.ParseInt(parts[0], 10, 64)
	end := int64(len(h.payload)) - 1
	if len(parts) > 1 && parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	if end >= int64(len(h.payload)) {
		end = int64(len(h.payload)) - 1
	}
	body := h.payload[start : end+1]
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+
		strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(h.payload)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body)
}

func TestThrottledSegmentsRecover(t *testing.T) {
	payload := makePayload(512 << 10)
	h := &throttlingHandler{payload: payload, retryAfter: "1"}
	h.throttles.Store(3) // first three ranged requests get rate limited
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	start := time.Now()
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/t.bin", OutDir: dir, Connections: 4,
		MinSplit: 4 << 10, MaxRetries: 6,
	})
	if err != nil {
		t.Fatalf("a 429 must not be fatal: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch after recovering from throttling")
	}
	if h.got429.Load() == 0 {
		t.Fatal("test did not actually exercise the 429 path")
	}
	// Retry-After: 1 must be honoured, so this cannot finish instantly.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("Retry-After was ignored: finished in %s", elapsed)
	}
}

func TestPermanentStatusFailsFast(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/gone.bin", OutDir: t.TempDir(), Connections: 4, MaxRetries: 5,
	})
	if err == nil {
		t.Fatal("expected an error for 403")
	}
	// The probe sees the 403 and aborts; no retry storm against a dead URL.
	if n := hits.Load(); n > 1 {
		t.Errorf("403 was retried %d times, expected to fail fast", n)
	}
}

func TestExpiredSignedURLMidTransferIsNotRetried(t *testing.T) {
	payload := makePayload(256 << 10)
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqs.Add(1)
		if n == 1 { // the probe succeeds
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", "bytes 0-0/"+strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[:1])
			return
		}
		w.WriteHeader(http.StatusForbidden) // token expired
	}))
	defer srv.Close()

	start := time.Now()
	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/signed.bin", OutDir: t.TempDir(), Connections: 4,
		MinSplit: 4 << 10, MaxRetries: 5,
	})
	if err == nil {
		t.Fatal("expected failure once the URL expired")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status, got: %v", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Errorf("cancellation noise leaked into the reported error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a 403 should abort immediately, took %s", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Errorf("delta seconds: got %s", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty: got %s", d)
	}
	if d := parseRetryAfter("-3"); d != 0 {
		t.Errorf("negative: got %s", d)
	}
	if d := parseRetryAfter("not-a-date"); d != 0 {
		t.Errorf("garbage: got %s", d)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d < 25*time.Second || d > 31*time.Second {
		t.Errorf("http date: got %s", d)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Errorf("past date should be 0, got %s", d)
	}
}

func TestRetryClassification(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadGateway, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusRequestTimeout, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, false},
		{http.StatusGone, false},
	}
	for _, c := range cases {
		err := &statusError{code: c.code, status: strconv.Itoa(c.code)}
		if got := retryable(err); got != c.want {
			t.Errorf("status %d: retryable = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestBackoffRespectsRetryAfterOverExponential(t *testing.T) {
	se := &statusError{code: http.StatusTooManyRequests, retryAfter: 7 * time.Second}
	if d := backoff(1, se); d != 7*time.Second {
		t.Errorf("Retry-After should win, got %s", d)
	}
	// An absurd Retry-After must be clamped rather than parking for an hour.
	huge := &statusError{code: http.StatusServiceUnavailable, retryAfter: time.Hour}
	if d := backoff(1, huge); d != 2*time.Minute {
		t.Errorf("clamp failed, got %s", d)
	}
	// Throttling with no header still backs off harder than a socket blip.
	plain429 := &statusError{code: http.StatusTooManyRequests}
	if backoff(2, plain429) <= backoff(2, errors.New("connection reset")) {
		t.Error("429 backoff should exceed generic network backoff")
	}
}

func TestFirstRealErrorIgnoresCancellation(t *testing.T) {
	real := errors.New("server returned 403 Forbidden")
	errs := []error{context.Canceled, real, context.Canceled}
	if got := firstRealError(errs); got != real {
		t.Errorf("got %v, want the real cause", got)
	}
	if got := firstRealError([]error{context.Canceled}); got != context.Canceled {
		t.Errorf("with only cancellation it should still report it, got %v", got)
	}
	if got := firstRealError([]error{nil, nil}); got != nil {
		t.Errorf("no errors should report nil, got %v", got)
	}
}

// dynamicHandler models an endpoint whose body length depends on the request,
// which is exactly what an echo service or any templated response does.
type dynamicHandler struct{ hits atomic.Int32 }

func (h *dynamicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.hits.Add(1)
	// The probe carries a Range header and so gets a longer echo than the real
	// fetch that follows. No Range support is advertised.
	body := strings.Repeat("x", 200+20*int(n))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body))
}

func TestNonResumableBodyIsNotZeroPadded(t *testing.T) {
	h := &dynamicHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()

	dir := t.TempDir()
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/echo.json", OutDir: dir, Connections: 4, MinSplit: 64,
	})
	if err != nil {
		t.Fatalf("a shifting body length must not fail the download: %v", err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The file must be exactly what the second request returned, with no tail.
	if len(got) != 240 {
		t.Fatalf("file is %d bytes, want 240 (probe said 220)", len(got))
	}
	if strings.ContainsRune(string(got), 0) {
		t.Fatal("file contains zero padding from preallocation")
	}
	if int64(len(got)) != res.Size {
		t.Errorf("reported size %d does not match file size %d", res.Size, len(got))
	}
}

func TestSingleStreamTrimsLongerLeftoverFile(t *testing.T) {
	body := strings.Repeat("y", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Our own earlier, longer attempt at this same link sits at the target
	// path with its sidecar. Unrelated files are never overwritten, but a dead
	// partial download of the same URL is restarted in place and must be trimmed.
	target := dir + "/leftover.bin"
	if err := os.WriteFile(target, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	saveState(target, &State{
		Version: stateVersion, URL: srv.URL + "/leftover.bin", Size: -1, Filename: "leftover.bin",
		Segments: []SegSnap{{Start: 0, End: -1, Done: 4096}},
	})
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/leftover.bin", OutDir: dir, Filename: "leftover.bin", Connections: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(res.Path) != filepath.Clean(target) {
		t.Fatalf("restarted into %s, want the same path %s", res.Path, target)
	}
	got, _ := os.ReadFile(res.Path)
	if len(got) != 500 {
		t.Fatalf("stale tail survived: file is %d bytes, want 500", len(got))
	}
}

// refusingServer turns away every request beyond max at once with a 503, as
// PikPak does past eight connections.
type refusingServer struct {
	slowServer
	max      int32
	inFlight atomic.Int32
}

func (h *refusingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	if n > h.max {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.slowServer.ServeHTTP(w, r)
}

func TestRefusedConnectionsDoNotFailTheDownload(t *testing.T) {
	payload := makePayload(8 << 20)
	h := &refusingServer{slowServer: slowServer{payload: payload, chunk: 16 << 10, delay: 20 * time.Millisecond}, max: 3}
	srv := httptest.NewServer(h)
	defer srv.Close()

	var mu sync.Mutex
	var last Progress
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/big.bin", OutDir: t.TempDir(),
		Connections: 8, MinSplit: 64 << 10, MaxRetries: 2,
		OnProgress: func(p Progress) {
			mu.Lock()
			last = p
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("a server that accepts three connections failed the download: %v", err)
	}
	checkFile(t, res.Path, payload)
	mu.Lock()
	defer mu.Unlock()
	if last.Limit > 3 {
		t.Errorf("still asking for %d connections from a server that takes 3", last.Limit)
	}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(want) {
		t.Fatalf("%s: content differs from the source", path)
	}
}
