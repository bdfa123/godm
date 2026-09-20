package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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

	DefaultConnections = 8
	// MaxConnections matches IDM's ceiling. Past this, hosts start refusing or
	// throttling harder than the extra connections gain.
	MaxConnections = 32
)

// Options is one download job.
type Options struct {
	URL      string
	Headers  map[string]string
	OutDir   string
	Filename string

	// Connections is how many connections the download may use at once.
	Connections int
	// ConnLimit, when set, is read continuously so the connection count can be
	// changed while the download runs. Connections is used when it is nil.
	ConnLimit func() int
	// MinSplit is the smallest piece worth its own connection. A range is only
	// split while both halves would be at least this big.
	MinSplit   int64
	MaxRetries int

	// Refresh resumes an interrupted download from a different URL, the way a
	// download manager continues after the user fetches a fresh link. Saved
	// progress is reused as long as the file size still matches; if it does
	// not, the download is refused instead of silently starting over.
	Refresh bool

	OnStart    func(StartInfo)
	OnProgress func(Progress)
}

// defaultRetries is how many times a request is retried before a download is
// called off, unless the caller says otherwise.
const defaultRetries = 5

func clampConnections(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxConnections {
		return MaxConnections
	}
	return n
}

func (o *Options) applyDefaults() {
	if o.Connections <= 0 {
		o.Connections = DefaultConnections
	}
	o.Connections = clampConnections(o.Connections)
	if o.MinSplit <= 0 {
		// Small enough that the last megabyte of a file is still shared out,
		// large enough that opening a connection is not most of the work.
		o.MinSplit = 512 << 10
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = defaultRetries
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

// Segment is one contiguous byte range. Ranges are not fixed: when a
// connection frees up, the largest unfinished range is cut in half and the
// back half becomes a new segment, so every connection stays busy to the end.
type Segment struct {
	Start int64
	end   atomic.Int64 // inclusive; -1 means "until the stream ends"
	done  atomic.Int64
	state atomic.Int32
	note  atomic.Value // string: why it is retrying or failed
	eof   atomic.Bool  // a stream of unknown length has ended

	// mu is held while a chunk is written and counted, so a split sees the
	// exact offset and the worker never writes past a shortened end.
	mu   sync.Mutex
	busy bool // owned by a worker; guarded by task.segMu
}

func newSegment(start, end int64) *Segment {
	s := &Segment{Start: start}
	s.end.Store(end)
	s.note.Store("")
	return s
}

// End is the inclusive last byte, or -1 for a stream of unknown length.
func (s *Segment) End() int64 { return s.end.Load() }

func (s *Segment) complete() bool {
	e := s.end.Load()
	if e < 0 {
		// No length to compare against: done once the server closed the stream.
		return s.eof.Load()
	}
	return s.Start+s.done.Load() > e
}

func (s *Segment) remaining() int64 {
	e := s.end.Load()
	if e < 0 {
		return math.MaxInt64
	}
	return e + 1 - s.Start - s.done.Load()
}

func (s *Segment) set(st SegmentState, note string) {
	s.state.Store(int32(st))
	s.note.Store(note)
}

// splitInHalf gives the back half of the unfinished part of s to a new
// segment. Holding s.mu means its worker is between writes, so the offset is
// exact and the worker will stop at the new end.
func (s *Segment) splitInHalf(minPiece int64) *Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.end.Load()
	if end < 0 {
		return nil
	}
	cur := s.Start + s.done.Load()
	rem := end - cur + 1
	if rem < 2*minPiece {
		return nil
	}
	mid := cur + rem/2
	ns := newSegment(mid, end)
	s.end.Store(mid - 1)
	return ns
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
	Limit    int // connections allowed right now
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

// errYield stops a connection because the connection limit was lowered.
var errYield = errors.New("connection released: the connection limit was lowered")

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

	var plan *targetPlan
	var f *os.File
	// Retry the choice if another program grabs the name between our check
	// and our exclusive create.
	for attempt := 0; ; attempt++ {
		plan, err = planTarget(pr, o)
		if err != nil {
			return nil, err
		}
		flags := os.O_WRONLY
		switch {
		case plan.resumed:
		case plan.overwrite:
			flags |= os.O_CREATE
		default:
			flags |= os.O_CREATE | os.O_EXCL
		}
		f, err = os.OpenFile(plan.path, flags, 0o644)
		if err == nil {
			break
		}
		plan.release()
		if !errors.Is(err, os.ErrExist) || attempt >= 3 {
			return nil, err
		}
	}
	defer plan.release()
	defer f.Close()

	// Preallocate so parallel WriteAt calls never race to extend the file, and
	// so a full disk fails here instead of at 90 percent.
	//
	// Only do this when the source is genuinely resumable. Without Range
	// support the probe's Content-Length is just a hint from a *different*
	// request: dynamic endpoints happily return a different body length each
	// time, and preallocating to the stale number leaves the file zero-padded.
	if pr.Size > 0 && pr.Resumable && !plan.resumed {
		if err := f.Truncate(pr.Size); err != nil {
			return nil, fmt.Errorf("preallocate %d bytes: %w", pr.Size, err)
		}
	}

	t := &task{
		opts: o, probe: pr, segs: plan.segs, file: f, target: plan.path, client: client,
		changed: make(chan struct{}, 1),
	}
	for _, s := range plan.segs {
		t.received.Add(s.done.Load())
	}

	if o.OnStart != nil {
		o.OnStart(StartInfo{
			Path: plan.path, Size: pr.Size, Resumable: pr.Resumable,
			Resumed: plan.resumed, Segments: len(plan.segs),
		})
	}

	start := time.Now()
	if err := t.run(ctx); err != nil {
		t.persist()
		return nil, classifyLinkError(err)
	}

	clearState(plan.path)
	t.segMu.Lock()
	nsegs := len(t.segs)
	t.segMu.Unlock()
	return &Result{
		Path:      plan.path,
		Size:      t.received.Load(),
		Elapsed:   time.Since(start),
		Segments:  nsegs,
		Resumable: pr.Resumable,
		Resumed:   plan.resumed,
	}, nil
}

// ---------- choosing the file on disk ----------

type targetPlan struct {
	path      string
	segs      []*Segment
	resumed   bool
	overwrite bool // our own earlier, unresumable attempt at this same link
	release   func()
}

// planTarget decides which file this download writes to. It continues a
// matching partial download when there is one. Otherwise it never touches a
// file it does not own: a finished file, or another link's partial file, gets
// a numbered sibling name instead, the way browsers do.
func planTarget(pr *ProbeResult, o Options) (*targetPlan, error) {
	base := filepath.Join(o.OutDir, pr.Filename)

	st, hasState := loadState(base)
	if hasState && st.reusable(pr, o.URL, o.Refresh) {
		if fi, err := os.Stat(base); err == nil && fi.Size() == st.Size {
			if release, ok := reserveTarget(base); ok {
				return &targetPlan{path: base, segs: restoreSegments(st), resumed: true, release: release}, nil
			}
			if o.Refresh {
				return nil, fmt.Errorf("%s is already being downloaded by another task", filepath.Base(base))
			}
		}
	}

	if o.Refresh {
		switch {
		case !hasState:
			return nil, &RefreshMismatchError{Reason: "no saved progress was found for " + filepath.Base(base)}
		case !pr.Resumable:
			return nil, &RefreshMismatchError{Reason: "the new link does not support resuming"}
		default:
			return nil, &RefreshMismatchError{Reason: fmt.Sprintf(
				"it points to a different file (%d bytes, the partial download is %d)", pr.Size, st.Size)}
		}
	}

	// Our own earlier attempt at this very link that can no longer be resumed
	// (the file changed on the server, or it never supported ranges): start it
	// over in place rather than leaving a dead partial file behind.
	if hasState && st.URL == o.URL {
		if release, ok := reserveTarget(base); ok {
			clearState(base)
			return &targetPlan{path: base, segs: freshSegments(pr, o), overwrite: true, release: release}, nil
		}
	}

	for n := 0; n < 10000; n++ {
		cand := numberedName(base, n)
		if pathExists(cand) || pathExists(statePath(cand)) {
			continue
		}
		if release, ok := reserveTarget(cand); ok {
			return &targetPlan{path: cand, segs: freshSegments(pr, o), release: release}, nil
		}
	}
	return nil, fmt.Errorf("no free file name for %s", pr.Filename)
}

func restoreSegments(st *State) []*Segment {
	segs := make([]*Segment, len(st.Segments))
	for i, s := range st.Segments {
		seg := newSegment(s.Start, s.End)
		seg.done.Store(s.Done)
		if seg.complete() {
			seg.set(SegDone, "")
		}
		segs[i] = seg
	}
	return segs
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// numberedName returns "name (n).ext", keeping double extensions like .tar.gz
// together as browsers do.
func numberedName(path string, n int) string {
	if n == 0 {
		return path
	}
	dir, name := filepath.Split(path)
	stem, ext := splitExt(name)
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
}

func splitExt(name string) (string, string) {
	lower := strings.ToLower(name)
	for _, double := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, double) && len(name) > len(double) {
			return name[:len(name)-len(double)], name[len(name)-len(double):]
		}
	}
	ext := filepath.Ext(name)
	if ext == name { // ".bashrc"
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

// Files this process is writing right now. Checking the disk alone is not
// enough: two downloads of the same name can both see it free before either
// creates it.
var (
	targetsMu sync.Mutex
	targets   = map[string]bool{}
)

func reserveTarget(path string) (release func(), ok bool) {
	// Windows file names are case-insensitive; on other systems this only
	// costs an occasional unnecessary "(1)".
	key := strings.ToLower(filepath.Clean(path))
	targetsMu.Lock()
	defer targetsMu.Unlock()
	if targets[key] {
		return nil, false
	}
	targets[key] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			targetsMu.Lock()
			delete(targets, key)
			targetsMu.Unlock()
		})
	}, true
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

// ---------- the transfer ----------

type task struct {
	opts     Options
	probe    *ProbeResult
	file     *os.File
	target   string
	client   *http.Client
	received atomic.Int64
	writeMu  sync.Mutex  // only used when the length is unknown
	linkDead atomic.Bool // a segment found the link expired; make no new requests

	segMu   sync.Mutex
	segs    []*Segment // sorted by Start, covering the whole file
	workers atomic.Int32
	changed chan struct{} // nudges the supervisor when a worker exits
}

// limit is how many connections may run right now.
func (t *task) limit() int {
	if !t.probe.Resumable || t.probe.Size <= 0 {
		return 1
	}
	n := t.opts.Connections
	if t.opts.ConnLimit != nil {
		n = t.opts.ConnLimit()
	}
	return clampConnections(n)
}

// snapshot copies the segments for display, merging finished neighbours so a
// file split dozens of times still reads as a few solid runs.
func (t *task) snapshot() Progress {
	t.segMu.Lock()
	segs := append([]*Segment(nil), t.segs...)
	t.segMu.Unlock()

	p := Progress{Received: t.received.Load(), Total: t.probe.Size, Limit: t.limit()}
	for _, s := range segs {
		st := SegmentState(s.state.Load())
		if s.complete() {
			st = SegDone
		}
		note, _ := s.note.Load().(string)
		v := SegmentView{Start: s.Start, End: s.end.Load(), Done: s.done.Load(), State: st.String(), Note: note}
		if st == SegActive {
			p.Active++
		}
		if n := len(p.Segments); n > 0 && v.State == "done" && p.Segments[n-1].State == "done" {
			p.Segments[n-1].End = v.End
			p.Segments[n-1].Done += v.Done
			continue
		}
		p.Segments = append(p.Segments, v)
	}
	return p
}

// run keeps up to limit() connections working until every byte is on disk.
func (t *task) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	spawn := func() {
		t.workers.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := t.worker(ctx)
			select {
			case t.changed <- struct{}{}:
			default:
			}
			if err == nil {
				return
			}
			errMu.Lock()
			errs = append(errs, err)
			errMu.Unlock()
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
		}()
	}

	for i := 0; i < t.limit(); i++ {
		spawn()
	}

	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for t.workers.Load() > 0 {
		select {
		case <-tick.C:
			if t.opts.OnProgress != nil {
				t.opts.OnProgress(t.snapshot())
			}
			t.persist()
		case <-t.changed:
		}
		// Top up to the limit: it may have been raised, or a connection just
		// finished and there is still a range big enough to share.
		if ctx.Err() == nil && !t.linkDead.Load() {
			for int(t.workers.Load()) < t.limit() && t.hasClaimableWork() {
				spawn()
			}
		}
	}
	wg.Wait()

	// Report once more whatever happened, so a failed connection's reason is
	// visible rather than lost between two ticks.
	if t.opts.OnProgress != nil {
		t.opts.OnProgress(t.snapshot())
	}
	if err := firstRealError(errs); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
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
	return t.file.Sync()
}

// worker takes pieces of the file one after another until there is nothing
// left it may do.
func (t *task) worker(ctx context.Context) error {
	yielded := false
	defer func() {
		if !yielded {
			t.workers.Add(-1)
		}
	}()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.linkDead.Load() {
			return nil
		}
		if t.yieldSlot() {
			yielded = true
			return nil
		}
		seg := t.claim()
		if seg == nil {
			return nil
		}
		err := t.runSegment(ctx, seg)
		t.unclaim(seg)
		if errors.Is(err, errYield) {
			yielded = true
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// yieldSlot gives up this worker's connection if the limit was lowered below
// the number running. The compare-and-swap stops exactly as many as needed.
func (t *task) yieldSlot() bool {
	for {
		w := t.workers.Load()
		if int(w) <= t.limit() {
			return false
		}
		if t.workers.CompareAndSwap(w, w-1) {
			return true
		}
	}
}

// claim hands a worker its next piece: an unowned unfinished segment if there
// is one, otherwise the back half of the largest range another connection is
// still working through. The second case is what stops a single slow
// connection from being left alone with the end of the file.
func (t *task) claim() *Segment {
	t.segMu.Lock()
	defer t.segMu.Unlock()
	for _, s := range t.segs {
		if !s.busy && !s.complete() {
			s.busy = true
			return s
		}
	}
	big, ok := t.splitCandidateLocked()
	if !ok {
		return nil
	}
	ns := big.splitInHalf(t.opts.MinSplit)
	if ns == nil {
		return nil
	}
	ns.busy = true
	for i, s := range t.segs {
		if s == big {
			t.segs = append(t.segs, nil)
			copy(t.segs[i+2:], t.segs[i+1:])
			t.segs[i+1] = ns
			break
		}
	}
	return ns
}

func (t *task) unclaim(s *Segment) {
	t.segMu.Lock()
	s.busy = false
	t.segMu.Unlock()
}

func (t *task) hasClaimableWork() bool {
	t.segMu.Lock()
	defer t.segMu.Unlock()
	for _, s := range t.segs {
		if !s.busy && !s.complete() {
			return true
		}
	}
	_, ok := t.splitCandidateLocked()
	return ok
}

func (t *task) splitCandidateLocked() (*Segment, bool) {
	if !t.probe.Resumable || t.probe.Size <= 0 {
		return nil, false
	}
	var best *Segment
	var bestRem int64
	for _, s := range t.segs {
		if !s.busy || s.complete() {
			continue
		}
		if r := s.remaining(); r > bestRem {
			best, bestRem = s, r
		}
	}
	return best, best != nil && bestRem >= 2*t.opts.MinSplit
}

// runSegment retries one range with exponential backoff. Each attempt re-reads
// seg.done, so a retry resumes mid-segment instead of refetching bytes already
// on disk.
func (t *task) runSegment(ctx context.Context, seg *Segment) error {
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
		err := t.trySegment(ctx, seg)
		switch {
		case err == nil:
			if seg.complete() {
				seg.set(SegDone, "")
			} else {
				seg.set(SegWaiting, "")
			}
			return nil
		case errors.Is(err, errYield):
			seg.set(SegWaiting, "")
			return err
		case ctx.Err() != nil:
			seg.set(SegWaiting, "")
			return ctx.Err()
		case !retryable(err):
			seg.set(SegFailed, shortErr(err))
			return err
		}
		last = err
	}
	seg.set(SegFailed, shortErr(last))
	return fmt.Errorf("segment at byte %d gave up after %d retries: %w", seg.Start, t.opts.MaxRetries, last)
}

func (t *task) trySegment(ctx context.Context, seg *Segment) error {
	offset := seg.Start + seg.done.Load()
	end := seg.end.Load()
	if end >= 0 && offset > end {
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
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	} else if offset > 0 {
		// Server never supported Range, so a mid-stream retry cannot resume.
		return fmt.Errorf("connection lost at byte %d and server does not support resume", offset)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := t.checkResponse(resp, ranged); err != nil {
		return err
	}
	seg.set(SegActive, "")

	buf := make([]byte, readBufSize)
	for {
		if t.yieldSlot() {
			return errYield
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			reached, werr := t.store(seg, buf[:n], &offset)
			if werr != nil {
				return werr
			}
			if reached {
				// Also the normal exit when a split shortened this range: the
				// rest of the response belongs to another connection now.
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
	e := seg.end.Load()
	if e < 0 {
		seg.eof.Store(true)
		return nil
	}
	if offset <= e {
		return fmt.Errorf("short read: segment stopped at %d, wanted %d", offset, e)
	}
	return nil
}

// store writes one chunk into the segment's range. It runs under seg.mu, so a
// concurrent split lands either before (the chunk is clipped to the new end)
// or after (the split sees the exact offset). reached reports the range is
// complete.
func (t *task) store(seg *Segment, b []byte, offset *int64) (reached bool, err error) {
	seg.mu.Lock()
	defer seg.mu.Unlock()
	end := seg.end.Load()
	if end >= 0 {
		if *offset > end {
			return true, nil
		}
		if *offset+int64(len(b)) > end+1 {
			b = b[:end+1-*offset] // server overshot, or the range was split
		}
	}
	wn, err := t.writeAt(b, *offset)
	*offset += int64(wn)
	seg.done.Add(int64(wn))
	t.received.Add(int64(wn))
	if err != nil {
		return false, err
	}
	return end >= 0 && *offset > end, nil
}

// checkResponse decides whether a segment response really carries our bytes.
// The order matters: a throttling or overload response is worth waiting out
// even when a CDN dresses it up as an HTML error page, but any other page in
// place of a binary file is a login or "link expired" screen.
func (t *task) checkResponse(resp *http.Response, ranged bool) error {
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

// persist saves progress, merging finished neighbours so the sidecar stays
// small however many times the ranges were split.
func (t *task) persist() {
	t.segMu.Lock()
	var snap []SegSnap
	for _, s := range t.segs {
		ss := SegSnap{Start: s.Start, End: s.end.Load(), Done: s.done.Load()}
		if n := len(snap); n > 0 && ss.End >= 0 {
			prev := &snap[n-1]
			if prev.End >= 0 && prev.Start+prev.Done > prev.End && ss.Start+ss.Done > ss.End {
				prev.End, prev.Done = ss.End, prev.Done+ss.Done
				continue
			}
		}
		snap = append(snap, ss)
	}
	t.segMu.Unlock()

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

func freshSegments(pr *ProbeResult, o Options) []*Segment {
	if pr.Size <= 0 || !pr.Resumable {
		return []*Segment{newSegment(0, -1)} // one stream, length unknown
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
		segs[i] = newSegment(start, end)
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
	if err == nil || errors.Is(err, errYield) {
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
