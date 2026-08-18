package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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
