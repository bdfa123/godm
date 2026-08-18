package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	readBufSize = 256 << 10
)

// Options is one download job.
type Options struct {
	URL         string
	Headers     map[string]string
	OutDir      string
	Filename    string
	Connections int
	MinSplit    int64 // never split a file smaller than this
	MaxRetries  int
	OnProgress  func(received, total int64)
}

func (o *Options) applyDefaults() {
	if o.Connections <= 0 {
		o.Connections = 8
	}
	if o.Connections > 32 {
		o.Connections = 32
	}
	if o.MinSplit <= 0 {
		// Small enough that a 2 MiB file still gets real parallelism, large
		// enough that per-connection setup does not dominate the transfer.
		o.MinSplit = 512 << 10
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = 5
	}
	if o.OutDir == "" {
		o.OutDir = "."
	}
}

// Segment is one contiguous byte range owned by exactly one goroutine.
// done is atomic because the progress ticker and state writer read it live.
type Segment struct {
	Start int64
	End   int64 // inclusive; -1 means "until the stream ends"
	done  atomic.Int64
}

// Result describes a finished download.
type Result struct {
	Path      string
	Size      int64
	Elapsed   time.Duration
	Segments  int
	Resumable bool
	Resumed   bool
}

// Download runs a job to completion, resuming from a sidecar state file if one
// matches. It is safe to cancel through ctx; progress survives in the sidecar.
func Download(ctx context.Context, o Options) (*Result, error) {
	o.applyDefaults()
	client := newClient()

	pr, err := probeWithRetry(ctx, client, o)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(o.OutDir, pr.Filename)

	segs, resumed := planSegments(target, pr, o)

	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Preallocate so parallel WriteAt calls never race to extend the file, and
	// so a full disk fails here instead of at 90 percent.
	//
	// Only do this when the source is genuinely resumable. Without Range
	// support the probe's Content-Length is just a hint from a *different*
	// request: dynamic endpoints happily return a different body length each
	// time, and preallocating to the stale number leaves the file zero-padded.
	if pr.Size > 0 && pr.Resumable && !resumed {
		if err := f.Truncate(pr.Size); err != nil {
			return nil, fmt.Errorf("preallocate %d bytes: %w", pr.Size, err)
		}
	}

	t := &task{opts: o, probe: pr, segs: segs, file: f, target: target, client: client}
	for _, s := range segs {
		t.received.Add(s.done.Load())
	}

	start := time.Now()
	if err := t.run(ctx); err != nil {
		t.persist()
		return nil, err
	}

	clearState(target)
	return &Result{
		Path:      target,
		Size:      t.received.Load(),
		Elapsed:   time.Since(start),
		Segments:  len(segs),
		Resumable: pr.Resumable,
		Resumed:   resumed,
	}, nil
}

// probeWithRetry gives the opening request the same retry policy as the
// segments. A busy server that answers the very first request with 429 or 503
// would otherwise kill the job before a single byte moved.
func probeWithRetry(ctx context.Context, c *http.Client, o Options) (*ProbeResult, error) {
	var last error
	for attempt := 0; attempt <= o.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt, last)):
			}
		}
		pr, err := Probe(ctx, c, o)
		if err == nil {
			return pr, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, err
		}
		last = err
	}
	return nil, fmt.Errorf("probe gave up after %d retries: %w", o.MaxRetries, last)
}

type task struct {
	opts     Options
	probe    *ProbeResult
	segs     []*Segment
	file     *os.File
	target   string
	client   *http.Client
	received atomic.Int64
	writeMu  sync.Mutex // only used when the length is unknown
}

func (t *task) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := make(chan struct{})
	var tickWG sync.WaitGroup
	tickWG.Add(1)
	go func() {
		defer tickWG.Done()
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if t.opts.OnProgress != nil {
					t.opts.OnProgress(t.received.Load(), t.probe.Size)
				}
				t.persist()
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, len(t.segs))
	for i := range t.segs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := t.runSegment(ctx, i); err != nil {
				errs[i] = err
				cancel() // one dead segment means the file is unusable; stop the rest
			}
		}(i)
	}
	wg.Wait()

	close(stop)
	tickWG.Wait()

	if err := firstRealError(errs); err != nil {
		return err
	}
	if t.opts.OnProgress != nil {
		t.opts.OnProgress(t.received.Load(), t.probe.Size)
	}
	got := t.received.Load()
	if t.probe.Resumable {
		// Here the length came from Content-Range on the same object, so a
		// short total means a truncated file, not success.
		if t.probe.Size > 0 && got != t.probe.Size {
			return fmt.Errorf("size mismatch: got %d of %d bytes", got, t.probe.Size)
		}
	} else if err := t.file.Truncate(got); err != nil {
		// Single stream: whatever arrived *is* the file. Cut any leftover tail
		// from an earlier, longer attempt at the same path.
		return fmt.Errorf("trim to %d bytes: %w", got, err)
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	return nil
}

// runSegment retries one range with exponential backoff. Each attempt re-reads
// seg.done, so a retry resumes mid-segment instead of refetching bytes already
// on disk.
func (t *task) runSegment(ctx context.Context, i int) error {
	var last error
	for attempt := 0; attempt <= t.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt, last)):
			}
		}
		err := t.trySegment(ctx, i)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
		last = err
	}
	return fmt.Errorf("segment %d gave up after %d retries: %w", i, t.opts.MaxRetries, last)
}

func (t *task) trySegment(ctx context.Context, i int) error {
	seg := t.segs[i]
	offset := seg.Start + seg.done.Load()
	if seg.End >= 0 && offset > seg.End {
		return nil // already complete
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.probe.FinalURL, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, t.opts.Headers)

	ranged := t.probe.Resumable
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, seg.End))
	} else if offset > 0 {
		// Server never supported Range, so a mid-stream retry cannot resume.
		return fmt.Errorf("connection lost at byte %d and server does not support resume", offset)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	want := http.StatusOK
	if ranged {
		want = http.StatusPartialContent
	}
	if resp.StatusCode != want {
		return &statusError{
			code:       resp.StatusCode,
			status:     resp.Status,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	buf := make([]byte, readBufSize)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if seg.End >= 0 && offset+int64(n) > seg.End+1 {
				n = int(seg.End + 1 - offset) // server overshot the requested range
			}
			if n > 0 {
				wn, werr := t.writeAt(buf[:n], offset)
				offset += int64(wn)
				seg.done.Add(int64(wn))
				t.received.Add(int64(wn))
				if werr != nil {
					return werr
				}
			}
			if seg.End >= 0 && offset > seg.End {
				return nil
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if seg.End >= 0 && offset <= seg.End {
		return fmt.Errorf("short read: segment %d stopped at %d, wanted %d", i, offset, seg.End)
	}
	return nil
}

// writeAt is a positional write (pwrite), so every segment writes straight into
// its own slice of the final file. There is no merge step and no .part files.
func (t *task) writeAt(b []byte, off int64) (int, error) {
	if t.probe.Size <= 0 {
		// Unknown length means a single segment, but guard a torn extend anyway.
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
	}
	return t.file.WriteAt(b, off)
}

func (t *task) persist() {
	snap := make([]SegSnap, len(t.segs))
	for i, s := range t.segs {
		snap[i] = SegSnap{Start: s.Start, End: s.End, Done: s.done.Load()}
	}
	saveState(t.target, &State{
		Version:  stateVersion,
		URL:      t.opts.URL,
		FinalURL: t.probe.FinalURL,
		Size:     t.probe.Size,
		ETag:     t.probe.ETag,
		Filename: filepath.Base(t.target),
		Segments: snap,
	})
}

// planSegments either restores a matching sidecar or carves a fresh plan.
func planSegments(target string, pr *ProbeResult, o Options) ([]*Segment, bool) {
	if st, ok := loadState(target); ok && st.reusable(pr, o.URL) {
		if fi, err := os.Stat(target); err == nil && fi.Size() == st.Size {
			segs := make([]*Segment, len(st.Segments))
			for i, s := range st.Segments {
				seg := &Segment{Start: s.Start, End: s.End}
				seg.done.Store(s.Done)
				segs[i] = seg
			}
			return segs, true
		}
	}
	clearState(target)
	return freshSegments(pr, o), false
}

func freshSegments(pr *ProbeResult, o Options) []*Segment {
	if pr.Size <= 0 || !pr.Resumable {
		return []*Segment{{Start: 0, End: -1}} // one stream, length unknown
	}
	n := o.Connections
	if maxSplit := pr.Size / o.MinSplit; maxSplit < int64(n) {
		n = int(maxSplit)
	}
	if n < 1 {
		n = 1
	}
	segs := make([]*Segment, n)
	chunk := pr.Size / int64(n)
	for i := 0; i < n; i++ {
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = pr.Size - 1
		}
		segs[i] = &Segment{Start: start, End: end}
	}
	return segs
}

func newClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 2 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		// No client-level Timeout: it would kill long transfers mid-flight.
	}
}

// firstRealError reports the failure that actually caused the abort. The
// sibling segments all report context.Canceled because we cancelled them, and
// joining those in would bury the real cause behind four lines of noise.
func firstRealError(errs []error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// statusError carries the HTTP status so retry policy can be decided on the
// code rather than on substring matching.
type statusError struct {
	code       int
	status     string
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("server returned %s (Retry-After %s)", e.status, e.retryAfter)
	}
	return "server returned " + e.status
}

// parseRetryAfter understands both forms of the header: delta seconds and an
// HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// backoff honours Retry-After when the server sent one, and otherwise grows
// exponentially. Throttling responses start from a much higher floor: hammering
// a 429 every 500ms is how you get an IP ban instead of a file.
func backoff(attempt int, last error) time.Duration {
	var se *statusError
	if errors.As(last, &se) {
		if se.retryAfter > 0 {
			if se.retryAfter > 2*time.Minute {
				return 2 * time.Minute
			}
			return se.retryAfter
		}
		if se.code == http.StatusTooManyRequests {
			d := time.Duration(1<<uint(attempt-1)) * 3 * time.Second
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			return d
		}
	}
	d := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	// Decide on the status code first; an expired signed URL (403) or a deleted
	// object (404) will never fix itself, but a 429 or 503 will.
	var se *statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
			http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	for _, frag := range []string{
		"connection reset", "broken pipe", "unexpected EOF", "short read",
		"EOF", "server closed", "stream error", "timeout",
		"connection was forcibly closed", "wsarecv",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}
