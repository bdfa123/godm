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

	// Refresh resumes an interrupted download from a different URL, the way a
	// download manager continues after the user fetches a fresh link. Saved
	// progress is reused as long as the file size still matches; if it does
	// not, the download is refused instead of silently starting over.
	Refresh bool

	OnStart    func(StartInfo)
	OnProgress func(Progress)
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

// SegmentState is what one connection is doing right now.
type SegmentState int32

const (
	SegWaiting SegmentState = iota
	SegConnecting
	SegActive
	SegRetrying
	SegDone
	SegFailed
)

func (s SegmentState) String() string {
	switch s {
	case SegConnecting:
		return "connecting"
	case SegActive:
		return "active"
	case SegRetrying:
		return "retrying"
	case SegDone:
		return "done"
	case SegFailed:
		return "failed"
	}
	return "waiting"
}

// Segment is one contiguous byte range owned by exactly one goroutine.
// Its counters are atomic because the progress ticker reads them live.
type Segment struct {
	Start int64
	End   int64 // inclusive; -1 means "until the stream ends"
	done  atomic.Int64
	state atomic.Int32
	note  atomic.Value // string: why it is retrying or failed
}

func (s *Segment) set(st SegmentState, note string) {
	s.state.Store(int32(st))
	s.note.Store(note)
}

// SegmentView is a point-in-time copy of a Segment for display.
type SegmentView struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Done  int64  `json:"done"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// Progress is reported a few times a second while a download runs.
type Progress struct {
	Received int64
	Total    int64
	Active   int // connections currently receiving bytes
	Segments []SegmentView
}

// StartInfo is reported once the plan is fixed and before bytes move.
type StartInfo struct {
	Path      string
	Size      int64
	Resumable bool
	Resumed   bool
	Segments  int
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

// LinkExpiredError means the URL stopped leading to the file: the server now
// refuses it, or answers with a web page instead of the bytes. Retrying the
// same URL is pointless, but the partial download is intact and can continue
// from a fresh link to the same file.
type LinkExpiredError struct {
	Reason string
	err    error
	// consequence marks a connection that stopped only because another one
	// had already found the link dead. It is never the error worth reporting.
	consequence bool
}

func (e *LinkExpiredError) Error() string { return "download link expired: " + e.Reason }
func (e *LinkExpiredError) Unwrap() error { return e.err }

// RefreshMismatchError means a fresh link was offered for a partial download
// but it does not describe the same file, so resuming would corrupt it.
type RefreshMismatchError struct{ Reason string }

func (e *RefreshMismatchError) Error() string { return "cannot resume from this link: " + e.Reason }

// Download runs a job to completion, resuming from a sidecar state file if one
// matches. It is safe to cancel through ctx; progress survives in the sidecar.
func Download(ctx context.Context, o Options) (*Result, error) {
	o.applyDefaults()
	client := newClient()

	pr, err := probeWithRetry(ctx, client, o)
	if err != nil {
		return nil, classifyLinkError(err)
	}

	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(o.OutDir, pr.Filename)

	segs, resumed, err := planSegments(target, pr, o)
	if err != nil {
		return nil, err
	}

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

	if o.OnStart != nil {
		o.OnStart(StartInfo{
			Path: target, Size: pr.Size, Resumable: pr.Resumable,
			Resumed: resumed, Segments: len(segs),
		})
	}

	start := time.Now()
	if err := t.run(ctx); err != nil {
		t.persist()
		return nil, classifyLinkError(err)
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

// classifyLinkError turns "this URL no longer works" into LinkExpiredError so
// callers can offer a refresh instead of reporting a dead end. 401/403/404/410
// are what signed and session-bound links return once they lapse.
func classifyLinkError(err error) error {
	var le *LinkExpiredError
	if errors.As(err, &le) {
		return err
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return &LinkExpiredError{Reason: "server returned " + se.status, err: err}
		}
	}
	return err
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
	writeMu  sync.Mutex  // only used when the length is unknown
	linkDead atomic.Bool // a segment found the link expired; make no new requests
}

func (t *task) snapshot() Progress {
	p := Progress{
		Received: t.received.Load(),
		Total:    t.probe.Size,
		Segments: make([]SegmentView, len(t.segs)),
	}
	for i, s := range t.segs {
		st := SegmentState(s.state.Load())
		note, _ := s.note.Load().(string)
		p.Segments[i] = SegmentView{
			Start: s.Start, End: s.End, Done: s.done.Load(),
			State: st.String(), Note: note,
		}
		if st == SegActive {
			p.Active++
		}
	}
	return p
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
					t.opts.OnProgress(t.snapshot())
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
			err := t.runSegment(ctx, i)
			if err == nil {
				return
			}
			errs[i] = err
			var expired *LinkExpiredError
			if errors.As(classifyLinkError(err), &expired) {
				// The link is dead, but connections already holding a response
				// can still finish their ranges, and every byte they save is
				// one the refreshed link will not have to send. Stop new
				// requests instead of cutting the transfers in flight.
				t.linkDead.Store(true)
				return
			}
			cancel() // anything else makes the file unusable; stop the rest
		}(i)
	}
	wg.Wait()

	close(stop)
	tickWG.Wait()

	// Report once more whatever happened, so a failed connection's reason is
	// visible rather than lost between two ticks.
	if t.opts.OnProgress != nil {
		t.opts.OnProgress(t.snapshot())
	}
	if err := firstRealError(errs); err != nil {
		return err
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
	seg := t.segs[i]
	var last error
	for attempt := 0; attempt <= t.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if t.linkDead.Load() {
				// A retry would only collect another 403.
				seg.set(SegFailed, "link expired")
				return &LinkExpiredError{Reason: "the link expired while this connection was retrying", consequence: true}
			}
			wait := backoff(attempt, last)
			seg.set(SegRetrying, fmt.Sprintf("%s; retry %d in %s", shortErr(last), attempt, wait.Round(100*time.Millisecond)))
			select {
			case <-ctx.Done():
				seg.set(SegWaiting, "")
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := t.trySegment(ctx, i)
		if err == nil {
			seg.set(SegDone, "")
			return nil
		}
		if ctx.Err() != nil {
			seg.set(SegWaiting, "")
			return ctx.Err()
		}
		if !retryable(err) {
			seg.set(SegFailed, shortErr(err))
			return err
		}
		last = err
	}
	seg.set(SegFailed, shortErr(last))
	return fmt.Errorf("segment %d gave up after %d retries: %w", i, t.opts.MaxRetries, last)
}

func (t *task) trySegment(ctx context.Context, i int) error {
	seg := t.segs[i]
	offset := seg.Start + seg.done.Load()
	if seg.End >= 0 && offset > seg.End {
		return nil // already complete
	}
	if t.linkDead.Load() {
		return &LinkExpiredError{Reason: "the link expired before this connection started", consequence: true}
	}
	seg.set(SegConnecting, "")

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

	if err := t.checkResponse(resp, ranged, offset, seg.End); err != nil {
		return err
	}
	seg.set(SegActive, "")

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

// checkResponse decides whether a segment response really carries our bytes.
// The order matters: a throttling or overload response is worth waiting out
// even when a CDN dresses it up as an HTML error page, but any other page in
// place of a binary file is a login or "link expired" screen.
func (t *task) checkResponse(resp *http.Response, ranged bool, offset, end int64) error {
	if isRetryableStatus(resp.StatusCode) {
		return &statusError{
			code:       resp.StatusCode,
			status:     resp.Status,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if looksLikeHTML(resp.Header.Get("Content-Type")) && !looksLikeHTML(t.probe.MIME) {
		return &LinkExpiredError{Reason: "the server sent a web page instead of the file"}
	}
	want := http.StatusOK
	if ranged {
		want = http.StatusPartialContent
	}
	if resp.StatusCode != want {
		return &statusError{code: resp.StatusCode, status: resp.Status}
	}
	if ranged && t.probe.Size > 0 {
		// Bytes from a different object would silently corrupt the file.
		if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok && total != t.probe.Size {
			return &LinkExpiredError{Reason: fmt.Sprintf(
				"the file on the server changed size (%d bytes, expected %d)", total, t.probe.Size)}
		}
	}
	return nil
}

func looksLikeHTML(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml")
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

// planSegments restores a matching sidecar or carves a fresh plan. With
// Refresh set there is no fresh plan to fall back on: the caller asked to
// continue a specific partial file.
func planSegments(target string, pr *ProbeResult, o Options) ([]*Segment, bool, error) {
	st, ok := loadState(target)
	if ok && st.reusable(pr, o.URL, o.Refresh) {
		if fi, err := os.Stat(target); err == nil && fi.Size() == st.Size {
			segs := make([]*Segment, len(st.Segments))
			for i, s := range st.Segments {
				seg := &Segment{Start: s.Start, End: s.End}
				seg.done.Store(s.Done)
				if s.Start+s.Done > s.End {
					seg.set(SegDone, "")
				} else {
					seg.set(SegWaiting, "")
				}
				segs[i] = seg
			}
			return segs, true, nil
		}
	}
	if o.Refresh {
		switch {
		case !ok:
			return nil, false, &RefreshMismatchError{Reason: "no saved progress was found for " + filepath.Base(target)}
		case !pr.Resumable:
			return nil, false, &RefreshMismatchError{Reason: "the new link does not support resuming"}
		default:
			return nil, false, &RefreshMismatchError{Reason: fmt.Sprintf(
				"it points to a different file (%d bytes, the partial download is %d)", pr.Size, st.Size)}
		}
	}
	clearState(target)
	return freshSegments(pr, o), false, nil
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
// sibling segments report context.Canceled, or an expiry they only learned
// about second-hand, and either would bury the real cause (the 403 itself).
func firstRealError(errs []error) error {
	var secondHand error
	for _, err := range errs {
		if err == nil || errors.Is(err, context.Canceled) {
			continue
		}
		var le *LinkExpiredError
		if errors.As(err, &le) && le.consequence {
			if secondHand == nil {
				secondHand = err
			}
			continue
		}
		return err
	}
	if secondHand != nil {
		return secondHand
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
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

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	var le *LinkExpiredError
	if errors.As(err, &le) {
		return false
	}
	// Decide on the status code first; an expired signed URL (403) or a deleted
	// object (404) will never fix itself, but a 429 or 503 will.
	var se *statusError
	if errors.As(err, &se) {
		return isRetryableStatus(se.code)
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
