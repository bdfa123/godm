package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPort = 16801
	// How long a task waits for the user to click its download link again.
	refreshWindow = 15 * time.Minute
	maxParallel   = 16
)

// TaskState is the lifecycle of one job in the daemon.
type TaskState string

const (
	StateQueued  TaskState = "queued"
	StateRunning TaskState = "running"
	StatePaused  TaskState = "paused"
	StateDone    TaskState = "done"
	StateError   TaskState = "error"
	// The link stopped working but the partial file is intact.
	StateNeedsRefresh TaskState = "needs_refresh"
	// The user asked to refresh; the next matching download adopts this task.
	StateAwaitingRefresh TaskState = "awaiting_refresh"
)

// TaskView is what the UI and the extension see.
type TaskView struct {
	ID        string        `json:"id"`
	URL       string        `json:"url"`
	Referrer  string        `json:"referrer,omitempty"`
	Filename  string        `json:"filename"`
	Path      string        `json:"path"`
	State     TaskState     `json:"state"`
	Error     string        `json:"error,omitempty"`
	Size      int64         `json:"size"`
	Received  int64         `json:"received"`
	Speed     int64         `json:"speed"`
	Conns     int           `json:"conns"`
	Active    int           `json:"active"`
	Resumable bool          `json:"resumable"`
	Segments  []SegmentView `json:"segments,omitempty"`
	// SpeedLimit is this download's own limit in bytes per second, 0 for
	// none. The limit shared by all downloads applies on top of it.
	SpeedLimit int64 `json:"speed_limit,omitempty"`
	// Kind is empty for an ordinary file. "hls" means a segmented stream,
	// where progress is counted in segments because the final size is only
	// ever an estimate until the last one lands.
	Kind     string  `json:"kind,omitempty"`
	Quality  string  `json:"quality,omitempty"`
	SegDone  int     `json:"segments_done,omitempty"`
	SegTotal int     `json:"segments_total,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	// Stage says which half of a merged video is arriving, for the tools that
	// fetch picture and sound separately and join them at the end. A torrent
	// ("bt") uses it for "metadata" and "seeding".
	Stage string `json:"stage,omitempty"`
	// A torrent counts what it has sent to other peers, and names the file
	// Play opens, its largest video, relative to the download folder.
	Uploaded     int64     `json:"uploaded,omitempty"`
	Media        string    `json:"media,omitempty"`
	AddedAt      time.Time `json:"added_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	RefreshUntil time.Time `json:"refresh_until,omitempty"`
}

type jobRequest struct {
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	Referrer    string            `json:"referrer,omitempty"`
	Headers     map[string]string `json:"headers"`
	Connections int               `json:"connections"`
	OutDir      string            `json:"out_dir"`
	// SpeedLimit caps this download in bytes per second; 0 leaves it to the
	// limit shared by all downloads.
	SpeedLimit int64 `json:"speed_limit,omitempty"`
	// Kind lets the caller say outright that this is a stream playlist. When
	// it is empty the URL decides, which covers links pasted by hand.
	Kind string `json:"kind,omitempty"`
	// Variant picks a quality from a master playlist; -1 takes the best.
	Variant int `json:"variant,omitempty"`
}

// isYTDLPJob is true only when the caller asks for it by name. A site needing
// yt-dlp cannot be told from its URL, and guessing would send ordinary links
// off to an external program for no reason.
func isYTDLPJob(req jobRequest) bool {
	return strings.EqualFold(req.Kind, "yt-dlp")
}

// isHLSJob decides which engine runs the job.
func isHLSJob(req jobRequest) bool {
	if req.Kind != "" {
		return strings.EqualFold(req.Kind, "hls")
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u")
}

type managedTask struct {
	mu   sync.Mutex
	view TaskView
	req  jobRequest // updated when the link is refreshed

	outDir string
	cancel context.CancelFunc
	// connLimit is read live by the engine, so changing it adds or releases
	// connections on a running download within a fraction of a second.
	connLimit atomic.Int32
	// speed is this download's own limit, read live the same way.
	speed RateLimiter
	// gen increments on every run so a superseded run cannot overwrite the
	// state of the one that replaced it.
	gen         int
	waitingSlot bool
	matching    bool // probing whether this new download is a refreshed link
	inFlight    bool // Download holds the file open
	deleteAfter bool // remove the file once the run lets go of it
	removed     bool

	lastRecv int64
	lastAt   time.Time
	speedEMA float64

	// play lets a media player read the file before it has finished.
	play Playback
}

func (t *managedTask) snapshot() TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view
}

func (t *managedTask) onStart(gen int, si StartInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.view.Path = si.Path
	t.view.Filename = filepath.Base(si.Path)
	if si.Size > 0 {
		t.view.Size = si.Size
	}
	t.view.Resumable = si.Resumable
}

func (t *managedTask) ytStart(gen int, si YTStart) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen || si.Title == "" {
		return
	}
	if t.view.Path == "" {
		t.view.Filename = si.Title
	}
}

// ytProgress takes yt-dlp's own speed rather than working one out from the
// byte count. A merged video arrives as two streams, so the count starts over
// part way through, and anything derived from it would read as a large
// negative at the join.
func (t *managedTask) ytProgress(gen int, p YTProgress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.view.Received = p.Downloaded
	t.view.Size = p.Total
	t.view.Speed = p.Speed
	t.view.Stage = p.Stage
	t.view.Kind = "yt-dlp"
	// Neither a connection count nor a segment map means anything here: the
	// external tool decides how it fetches.
	t.view.Resumable = false
	t.view.Active, t.view.Conns = 0, 0
}

func (t *managedTask) hlsStart(gen int, si HLSStart) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.view.Path = si.Target
	t.view.Filename = filepath.Base(si.Target)
	t.view.Kind = "hls"
	t.view.SegTotal = si.Segments
	t.view.Duration = si.Duration
	t.view.Resumable = true // a segment stream always picks up where it stopped
	if si.Chosen >= 0 && si.Chosen < len(si.Variants) {
		t.view.Quality = si.Variants[si.Chosen].Label()
	}
}

// hlsProgress reuses the byte-range smoothing for speed, then adds the counts
// that only a segment stream has.
func (t *managedTask) hlsProgress(gen int, p HLSProgress) {
	t.progress(gen, Progress{
		Received: p.Bytes,
		Total:    p.Estimate,
		Active:   p.Active,
		Limit:    p.Limit,
	})
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.view.SegDone = p.Segments
	t.view.SegTotal = p.TotalSegments
	t.view.Duration = p.Duration
}

// progress folds raw byte counts into an exponentially smoothed rate so the UI
// does not flicker between 0 and 40 MB/s on every tick.
func (t *managedTask) progress(gen int, p Progress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	now := time.Now()
	if !t.lastAt.IsZero() {
		if dt := now.Sub(t.lastAt).Seconds(); dt > 0.05 {
			inst := float64(p.Received-t.lastRecv) / dt
			if t.speedEMA == 0 {
				t.speedEMA = inst
			} else {
				t.speedEMA = 0.7*t.speedEMA + 0.3*inst
			}
			t.lastRecv, t.lastAt = p.Received, now
		}
	} else {
		// First report of a run: a resumed task already has gigabytes on disk,
		// which must not count as bytes received in this instant.
		t.lastRecv, t.lastAt = p.Received, now
	}
	t.view.Received = p.Received
	if p.Total > 0 {
		t.view.Size = p.Total
	}
	t.view.Speed = int64(t.speedEMA)
	t.view.Active = p.Active
	t.view.Conns = p.Limit
	t.view.Segments = p.Segments
}

// requestedConnections normalises a job's connection count, defaulting to 8.
func requestedConnections(n int) int {
	if n <= 0 {
		return DefaultConnections
	}
	return clampConnections(n)
}

// Manager owns every task, runs them as a first-in-first-out queue, and keeps
// the list on disk so it survives the daemon restarting.
type Manager struct {
	mu      sync.Mutex
	tasks   map[string]*managedTask
	order   []string // queue order: insertion
	limit   int
	running int
	wake    chan struct{} // closed and replaced whenever scheduling state changes

	outDir string
	seq    atomic.Uint64

	store   string // tasks.json; empty disables persistence
	dirty   atomic.Bool
	openURL func(string) error

	notifyOn atomic.Bool
	// announce is what tells the user a download finished or needs them. The
	// tray provides it; tests replace it.
	announce func(title, text, open string)

	settings Settings // guarded by mu
	// power reaches the computer's own power state. It stays empty until
	// RunDaemon installs the real calls.
	power powerOps
	awake *awakeKeeper
	// after is what to do when the downloads finish; see afterAction. It and
	// afterSeen, whether a download has finished since it was set, are guarded
	// by mu.
	after     afterAction
	afterSeen bool
	// streamPiece is the engine's piece size while a player reads; zero keeps
	// the engine default. Tests shrink it to fit their small files.
	streamPiece int64
	// speed is the limit all downloads share between them.
	speed RateLimiter

	bt btEngine // torrents, in bt.go
}

func NewManager(outDir string, parallel int) *Manager {
	if parallel <= 0 {
		parallel = 3
	}
	m := &Manager{
		tasks:    map[string]*managedTask{},
		limit:    clampLimit(parallel),
		wake:     make(chan struct{}),
		outDir:   outDir,
		openURL:  openInBrowser,
		announce: trayNotify,
		settings: defaultSettings(),
		after:    afterNothing,
	}
	m.notifyOn.Store(true)
	return m
}

func (m *Manager) Notifications() bool { return m.notifyOn.Load() }

func (m *Manager) SetNotifications(on bool) {
	m.notifyOn.Store(on)
	m.dirty.Store(true)
}

func (m *Manager) OutDir() string { return m.outDir }

// announceFinish tells the user what happened to a download once it stops
// moving. Pausing is their own doing, so it stays quiet.
func (m *Manager) announceFinish(v TaskView, st TaskState) {
	if m.announce == nil || !m.notifyOn.Load() {
		return
	}
	name := v.Filename
	if name == "" {
		name = v.URL
	}
	switch st {
	case StateDone:
		text := name
		if v.Size > 0 {
			size := humanBytes(v.Size)
			if !v.StartedAt.IsZero() && !v.EndedAt.IsZero() {
				if secs := v.EndedAt.Sub(v.StartedAt).Seconds(); secs > 0 {
					size = m.tr("notify.sizeIn", "size", size, "time", span(m.Lang(), int64(math.Round(secs))))
				}
			}
			text += "\n" + size
		}
		m.announce(m.tr("notify.complete"), text, v.Path)
	case StateNeedsRefresh:
		m.announce(m.tr("notify.expired"), m.tr("notify.expiredAt", "name", name), "app")
	case StateError:
		m.announce(m.tr("notify.failed"), name+"\n"+v.Error, "app")
	}
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxParallel {
		return maxParallel
	}
	return n
}

// broadcastLocked wakes every task waiting for a slot. Callers hold m.mu.
func (m *Manager) broadcastLocked() {
	close(m.wake)
	m.wake = make(chan struct{})
}

func (m *Manager) get(id string) *managedTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

func validateURL(u string) error {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("only http and https URLs are supported")
	}
	return nil
}

func sameActiveDownload(t *managedTask, req jobRequest, outDir string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.view.State {
	case StateQueued, StateRunning:
	default:
		return false
	}
	return t.req.URL == req.URL &&
		strings.EqualFold(t.req.Kind, req.Kind) &&
		t.req.Variant == req.Variant &&
		strings.EqualFold(filepath.Clean(t.outDir), filepath.Clean(outDir))
}

func (m *Manager) Add(req jobRequest) (string, error) {
	if isBTJob(req) {
		req.Kind = "bt"
		if err := validateTorrentURL(req.URL); err != nil {
			return "", err
		}
		if req.Filename == "" {
			req.Filename = torrentDisplayName(req.URL)
		}
	} else if err := validateURL(req.URL); err != nil {
		return "", err
	}
	id := fmt.Sprintf("t%d-%d", time.Now().UnixMilli(), m.seq.Add(1))
	outDir := m.downloadDir(req)
	req.Connections = requestedConnections(req.Connections)
	req.SpeedLimit = max(req.SpeedLimit, 0)
	mt := &managedTask{
		req:    req,
		outDir: outDir,
		view: TaskView{
			ID: id, URL: req.URL, Referrer: req.Referrer, Filename: req.Filename,
			State: StateQueued, Size: -1, Conns: req.Connections, AddedAt: time.Now(),
			SpeedLimit: req.SpeedLimit,
		},
	}
	mt.connLimit.Store(int32(req.Connections))
	mt.speed.SetRate(req.SpeedLimit)
	if req.Kind == "bt" {
		// Shown as a torrent while it waits in line; it counts peers, not
		// connections.
		mt.view.Kind, mt.view.Conns = "bt", 0
	}

	m.mu.Lock()
	for _, existingID := range m.order {
		if existing := m.tasks[existingID]; existing != nil &&
			(sameActiveDownload(existing, req, outDir) || sameTorrent(existing, req)) {
			m.mu.Unlock()
			return existingID, nil
		}
	}
	m.tasks[id] = mt
	m.order = append(m.order, id)
	m.broadcastLocked()
	m.mu.Unlock()
	m.dirty.Store(true)

	go m.start(mt, true, false)
	return id, nil
}

// AddBatch queues many downloads at once, keeping their order.
func (m *Manager) AddBatch(reqs []jobRequest) (ids []string, errs []string) {
	for _, r := range reqs {
		id, err := m.Add(r)
		if err != nil {
			errs = append(errs, r.URL+": "+err.Error())
			continue
		}
		ids = append(ids, id)
	}
	return ids, errs
}

// start runs one attempt of a task. allowMatch lets a brand-new download be
// absorbed by a task that is waiting for a refreshed link; refresh tells the
// engine to continue existing progress from a URL that differs from before.
func (m *Manager) start(mt *managedTask, allowMatch, refresh bool) {
	mt.mu.Lock()
	// Resume, refresh and address changes reserve StateQueued before starting
	// their goroutine. If Pause won the race in between, do not resurrect it.
	if mt.removed || mt.view.State != StateQueued {
		mt.mu.Unlock()
		return
	}
	mt.gen++
	gen := mt.gen
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mt.cancel = cancel
	mt.view.State = StateQueued
	mt.view.Error = ""
	mt.view.Speed, mt.view.Active = 0, 0
	mt.view.RefreshUntil = time.Time{}
	mt.lastAt, mt.lastRecv, mt.speedEMA = time.Time{}, 0, 0
	req, outDir := mt.req, mt.outDir
	filename := req.Filename
	if mt.view.Path != "" {
		// Later runs must land on the same partial file, whatever name the
		// server suggests this time.
		filename = filepath.Base(mt.view.Path)
	}
	mt.mu.Unlock()
	m.dirty.Store(true)

	if allowMatch && !isBTJob(req) && m.adoptAsRefresh(ctx, mt) {
		return
	}

	if !m.acquire(ctx, mt, gen) {
		m.finishRun(mt, gen, StatePaused, "", "")
		return
	}
	defer m.release()

	mt.mu.Lock()
	mt.inFlight = true
	mt.mu.Unlock()

	var donePath string
	var err error
	switch {
	case isBTJob(req):
		donePath, err = m.runTorrent(ctx, mt, gen)
	case isYTDLPJob(req):
		donePath, err = RunYTDLP(ctx, YTDLPOptions{
			URL:     req.URL,
			OutDir:  outDir,
			Headers: req.Headers,
			// For yt-dlp the variant is a picture height, not a position in a
			// list of streams.
			Format: ytdlpFormatFor(req.Variant),
			OnStart: func(si YTStart) {
				mt.ytStart(gen, si)
				m.dirty.Store(true)
			},
			OnUpdate: func(p YTProgress) {
				mt.ytProgress(gen, p)
				m.dirty.Store(true)
			},
		})
		if err != nil {
			donePath = ""
		}
	case isHLSJob(req):
		donePath, err = DownloadHLS(ctx, HLSOptions{
			URL:         req.URL,
			Headers:     req.Headers,
			OutDir:      outDir,
			Filename:    filename,
			Connections: req.Connections,
			ConnLimit:   func() int { return int(mt.connLimit.Load()) },
			Limiters:    []*RateLimiter{&m.speed, &mt.speed},
			Variant:     req.Variant,
			OnStart: func(si HLSStart) {
				mt.hlsStart(gen, si)
				m.dirty.Store(true)
			},
			OnProgress: func(p HLSProgress) {
				mt.hlsProgress(gen, p)
				m.dirty.Store(true)
			},
		})
		if err != nil {
			donePath = ""
		}
	default:
		var res *Result
		res, err = Download(ctx, Options{
			URL:         req.URL,
			Headers:     req.Headers,
			OutDir:      outDir,
			Filename:    filename,
			Connections: req.Connections,
			ConnLimit:   func() int { return int(mt.connLimit.Load()) },
			Limiters:    []*RateLimiter{&m.speed, &mt.speed},
			Refresh:     refresh,
			Playback:    &mt.play,
			StreamPiece: m.streamPiece,
			OnStart: func(si StartInfo) {
				mt.onStart(gen, si)
				m.dirty.Store(true)
			},
			OnProgress: func(p Progress) {
				mt.progress(gen, p)
				m.dirty.Store(true)
			},
		})
		if err == nil {
			donePath = res.Path
		}
	}
	m.afterRun(mt)

	var expired *LinkExpiredError
	var mismatch *RefreshMismatchError
	switch {
	case err == nil:
		m.finishRun(mt, gen, StateDone, "", donePath)
	case ctx.Err() != nil:
		m.finishRun(mt, gen, StatePaused, "", "")
	case errors.As(err, &expired), errors.As(err, &mismatch):
		m.finishRun(mt, gen, StateNeedsRefresh, err.Error(), "")
	default:
		m.finishRun(mt, gen, StateError, err.Error(), "")
	}
}

// acquire blocks until the task may run: a slot is free and no task added
// before it is still waiting. A download list is only predictable if it
// starts in the order it was built.
func (m *Manager) acquire(ctx context.Context, mt *managedTask, gen int) bool {
	mt.mu.Lock()
	mt.waitingSlot = true
	mt.mu.Unlock()
	defer func() {
		mt.mu.Lock()
		mt.waitingSlot = false
		mt.mu.Unlock()
	}()

	for {
		m.mu.Lock()
		if m.running < m.limit && m.nextInLineLocked() == mt {
			mt.mu.Lock()
			ok := mt.gen == gen && !mt.removed
			if ok {
				mt.waitingSlot = false
				mt.view.State = StateRunning
				mt.view.StartedAt = time.Now()
			}
			mt.mu.Unlock()
			if ok {
				m.running++
				m.syncAwakeLocked()
			}
			m.broadcastLocked()
			m.mu.Unlock()
			m.dirty.Store(true)
			return ok
		}
		ch := m.wake
		m.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			m.mu.Lock()
			m.broadcastLocked() // the head of the line may have just changed
			m.mu.Unlock()
			return false
		}
	}
}

// nextInLineLocked returns the oldest queued task. It goes by queue position,
// not by which goroutine happened to reach acquire first: goroutines start in
// no particular order, and a list that runs 1, 3, 2 is not a queue. A task that
// is busy checking whether it is a refreshed link is skipped so a slow probe
// cannot hold up everything behind it.
func (m *Manager) nextInLineLocked() *managedTask {
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		eligible := t.view.State == StateQueued && !t.matching
		t.mu.Unlock()
		if eligible {
			return t
		}
	}
	return nil
}

func (m *Manager) release() {
	m.mu.Lock()
	m.running--
	m.syncAwakeLocked()
	m.broadcastLocked()
	m.mu.Unlock()
}

func (m *Manager) afterRun(mt *managedTask) {
	mt.mu.Lock()
	mt.inFlight = false
	del, path := mt.deleteAfter, mt.view.Path
	mt.mu.Unlock()
	if del {
		deleteDownload(path)
	}
}

func (m *Manager) finishRun(mt *managedTask, gen int, st TaskState, errMsg, path string) {
	finalSize, haveFinalSize := int64(0), false
	if st == StateDone && path != "" {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			finalSize, haveFinalSize = fi.Size(), true
		}
	}
	mt.mu.Lock()
	if mt.gen != gen {
		mt.mu.Unlock()
		return
	}
	mt.view.State = st
	mt.view.Error = errMsg
	mt.view.Speed, mt.view.Active = 0, 0
	if path != "" {
		mt.view.Path = path
		mt.view.Filename = filepath.Base(path)
	}
	if st == StateDone {
		if haveFinalSize {
			mt.view.Size = finalSize
		}
		mt.view.EndedAt = time.Now()
		mt.view.Received = mt.view.Size
		mt.view.Segments = nil
	} else {
		// Keep the segment map so the UI shows where it stopped, but nothing is
		// connecting any more.
		segs := make([]SegmentView, len(mt.view.Segments))
		for i, s := range mt.view.Segments {
			moving := s.State == "active" || s.State == "connecting" || s.State == "retrying"
			// After a pause or an expired link the saved bytes are intact, and
			// painting those segments red would suggest otherwise.
			if moving || (s.State == "failed" && st != StateError) {
				s.State, s.Note = "waiting", ""
			}
			segs[i] = s
		}
		mt.view.Segments = segs
	}
	view := mt.view
	mt.mu.Unlock()
	m.dirty.Store(true)

	m.mu.Lock()
	m.broadcastLocked()
	m.mu.Unlock()

	if st != StatePaused {
		m.checkAfterAll(st == StateDone)
	}
	go m.announceFinish(view, st)
}

func (m *Manager) Pause(id string) bool {
	mt := m.get(id)
	if mt == nil {
		return false
	}
	mt.mu.Lock()
	st, cancel := mt.view.State, mt.cancel
	if st == StateQueued {
		// Mark it synchronously so a resume goroutine that has not begun yet
		// observes the pause instead of creating a fresh context afterwards.
		mt.view.State = StatePaused
	} else if st == StateAwaitingRefresh {
		mt.view.State, mt.view.RefreshUntil = StateNeedsRefresh, time.Time{}
	}
	mt.mu.Unlock()
	if (st == StateQueued || st == StateRunning) && cancel != nil {
		cancel()
	}
	if st == StateQueued {
		m.mu.Lock()
		m.broadcastLocked()
		m.mu.Unlock()
	}
	m.dirty.Store(true)
	return true
}

func (m *Manager) Resume(id string) bool {
	mt := m.get(id)
	if mt == nil {
		return false
	}
	mt.mu.Lock()
	switch mt.view.State {
	case StatePaused, StateError, StateNeedsRefresh, StateAwaitingRefresh:
		mt.view.State = StateQueued
		mt.view.Error = ""
		mt.view.RefreshUntil = time.Time{}
		mt.mu.Unlock()
		m.dirty.Store(true)
		m.mu.Lock()
		m.broadcastLocked()
		m.mu.Unlock()
		go m.start(mt, false, false)
		return true
	}
	mt.mu.Unlock()
	return false
}

func (m *Manager) PauseAll() {
	for _, v := range m.List() {
		if v.State == StateQueued || v.State == StateRunning {
			m.Pause(v.ID)
		}
	}
}

func (m *Manager) ResumeAll() {
	for _, v := range m.list(true) { // oldest first, so the queue keeps its order
		if v.State == StatePaused || v.State == StateError {
			m.Resume(v.ID)
		}
	}
}

// Remove drops a task. With deleteFile the partial or finished file goes too;
// if a run still has it open, deletion waits until that run lets go.
func (m *Manager) Remove(id string, deleteFile bool) bool {
	m.mu.Lock()
	mt, ok := m.tasks[id]
	if ok {
		m.detachLocked(id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}

	mt.mu.Lock()
	mt.removed = true
	cancel, path, busy := mt.cancel, mt.view.Path, mt.inFlight
	bt := isBTJob(mt.req)
	if deleteFile && busy && !bt {
		mt.deleteAfter = true
	}
	mt.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if bt {
		// A torrent is many files the client holds open; it lets go of them
		// before any are deleted.
		go m.forgetTorrent(mt, deleteFile)
	} else if deleteFile && !busy {
		deleteDownload(path)
	}
	m.dirty.Store(true)
	return true
}

func (m *Manager) detachLocked(id string) {
	delete(m.tasks, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.broadcastLocked()
}

func deleteDownload(path string) {
	if path == "" {
		return
	}
	os.Remove(path)
	os.Remove(statePath(path))
}

// RequestRefresh parks a task until the user fetches a new link for it, and
// opens the page the download originally came from so they can click it again.
func (m *Manager) RequestRefresh(id string) (string, error) {
	mt := m.get(id)
	if mt == nil {
		return "", fmt.Errorf("no such task")
	}
	if mt.isTorrent() {
		return "", fmt.Errorf("a torrent has no link that expires")
	}
	mt.mu.Lock()
	switch mt.view.State {
	case StateRunning, StateQueued, StateDone:
		st := mt.view.State
		mt.mu.Unlock()
		return "", fmt.Errorf("a %s download does not need a new link", st)
	}
	if mt.view.Path == "" || mt.view.Size <= 0 {
		mt.mu.Unlock()
		return "", fmt.Errorf("nothing was downloaded yet, so there is no progress to keep; use Change address instead")
	}
	mt.view.State = StateAwaitingRefresh
	mt.view.RefreshUntil = time.Now().Add(refreshWindow)
	ref := mt.req.Referrer
	mt.mu.Unlock()
	m.dirty.Store(true)

	if ref != "" && m.openURL != nil {
		go m.openURL(ref)
	}
	return ref, nil
}

// refreshCandidates lists tasks waiting for a new link, expiring stale waits.
func (m *Manager) refreshCandidates() []*managedTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []*managedTask
	for _, id := range m.order {
		t := m.tasks[id]
		t.mu.Lock()
		if t.view.State == StateAwaitingRefresh {
			if now.After(t.view.RefreshUntil) {
				t.view.State, t.view.RefreshUntil = StateNeedsRefresh, time.Time{}
			} else {
				out = append(out, t)
			}
		}
		t.mu.Unlock()
	}
	return out
}

// adoptAsRefresh checks whether a newly captured download is really a fresh
// link for a task that is waiting for one. The deciding fact is the file size:
// signed links change on every click, the bytes behind them do not. If it
// matches, the waiting task takes the new URL and cookies and continues from
// its saved segments, and the new task disappears.
func (m *Manager) adoptAsRefresh(ctx context.Context, nt *managedTask) bool {
	cands := m.refreshCandidates()
	if len(cands) == 0 {
		return false
	}
	nt.mu.Lock()
	req, newID := nt.req, nt.view.ID
	nt.matching = true
	nt.mu.Unlock()
	defer func() {
		nt.mu.Lock()
		nt.matching = false
		nt.mu.Unlock()
		m.mu.Lock()
		m.broadcastLocked()
		m.mu.Unlock()
	}()

	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, err := Probe(pctx, newClient(), Options{URL: req.URL, Headers: req.Headers, Filename: req.Filename})
	if err != nil || pr.Size <= 0 {
		return false
	}
	target := pickRefreshTarget(cands, pr)
	if target == nil {
		return false
	}

	target.mu.Lock()
	if target.view.State != StateAwaitingRefresh {
		target.mu.Unlock()
		return false // another download adopted it first
	}
	target.req.URL = req.URL
	if len(req.Headers) > 0 {
		target.req.Headers = req.Headers // fresh cookies belong with the fresh link
	}
	if req.Referrer != "" {
		target.req.Referrer, target.view.Referrer = req.Referrer, req.Referrer
	}
	target.view.URL = req.URL
	target.view.State = StateQueued
	target.view.RefreshUntil = time.Time{}
	name := target.view.Filename
	target.mu.Unlock()

	m.mu.Lock()
	m.detachLocked(newID)
	m.mu.Unlock()
	m.dirty.Store(true)

	log.Printf("refreshed link for %s (%d bytes)", name, pr.Size)
	go m.start(target, false, true)
	return true
}

// pickRefreshTarget prefers a waiting task with the same size and name, and
// accepts a size-only match when exactly one task could be meant.
func pickRefreshTarget(cands []*managedTask, pr *ProbeResult) *managedTask {
	var sameSize []*managedTask
	var named *managedTask
	var namedUntil time.Time
	for _, c := range cands {
		c.mu.Lock()
		size, name, until := c.view.Size, c.view.Filename, c.view.RefreshUntil
		c.mu.Unlock()
		if size != pr.Size {
			continue
		}
		sameSize = append(sameSize, c)
		if strings.EqualFold(name, pr.Filename) && until.After(namedUntil) {
			named, namedUntil = c, until
		}
	}
	if named != nil {
		return named
	}
	if len(sameSize) == 1 {
		return sameSize[0]
	}
	return nil
}

// ChangeAddress points a task at a URL the user pasted. If the task already
// has progress, the engine verifies the new URL is the same file before
// continuing.
func (m *Manager) ChangeAddress(id, url string) error {
	if err := validateURL(url); err != nil {
		return err
	}
	mt := m.get(id)
	if mt == nil {
		return fmt.Errorf("no such task")
	}
	if mt.isTorrent() {
		return fmt.Errorf("a torrent is found by its info hash, not an address")
	}
	mt.mu.Lock()
	if st := mt.view.State; st == StateRunning || st == StateQueued {
		mt.mu.Unlock()
		return fmt.Errorf("pause the download before changing its address")
	}
	mt.req.URL, mt.view.URL = url, url
	hasProgress := mt.view.Path != "" && mt.view.Received > 0 && mt.view.State != StateDone
	mt.view.State = StateQueued
	mt.view.Error = ""
	mt.mu.Unlock()
	m.dirty.Store(true)
	m.mu.Lock()
	m.broadcastLocked()
	m.mu.Unlock()
	go m.start(mt, false, hasProgress)
	return nil
}

// SetConnections changes how many connections a task may use. A running
// download picks it up immediately: more connections split the remaining
// ranges, fewer release connections after their current chunk.
func (m *Manager) SetConnections(id string, n int) (int, error) {
	mt := m.get(id)
	if mt == nil {
		return 0, fmt.Errorf("no such task")
	}
	n = clampConnections(n)
	mt.mu.Lock()
	mt.req.Connections = n
	mt.view.Conns = n
	mt.mu.Unlock()
	mt.connLimit.Store(int32(n))
	m.dirty.Store(true)
	return n, nil
}

func (m *Manager) SetLimit(n int) int {
	m.mu.Lock()
	m.limit = clampLimit(n)
	l := m.limit
	m.broadcastLocked()
	m.mu.Unlock()
	m.dirty.Store(true)
	return l
}

func (m *Manager) Limit() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit
}

// SetSpeedLimit caps all downloads together at n bytes per second, or lifts
// the cap with 0. Running downloads slow down or speed up at once.
func (m *Manager) SetSpeedLimit(n int64) int64 {
	m.speed.SetRate(n)
	m.dirty.Store(true)
	return m.speed.Rate()
}

func (m *Manager) SpeedLimit() int64 { return m.speed.Rate() }

// SetTaskSpeedLimit caps one download, on top of the limit they all share;
// whichever is stricter is the one it keeps to.
func (m *Manager) SetTaskSpeedLimit(id string, n int64) (int64, error) {
	mt := m.get(id)
	if mt == nil {
		return 0, fmt.Errorf("no such task")
	}
	n = max(n, 0)
	mt.mu.Lock()
	mt.req.SpeedLimit = n
	mt.view.SpeedLimit = n
	mt.mu.Unlock()
	mt.speed.SetRate(n)
	m.dirty.Store(true)
	return n, nil
}

func (m *Manager) OpenFolder(id string) error {
	mt := m.get(id)
	if mt == nil {
		return fmt.Errorf("no such task")
	}
	mt.mu.Lock()
	path, dir := mt.view.Path, mt.outDir
	mt.mu.Unlock()
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return showInFolder(path)
		}
	}
	return showInFolder(dir)
}

// List returns tasks newest first, which is what a person scanning the list
// wants; the queue itself still runs oldest first.
func (m *Manager) List() []TaskView { return m.list(false) }

func (m *Manager) list(oldestFirst bool) []TaskView {
	m.refreshCandidates() // lazily expire stale refresh waits
	m.mu.Lock()
	tasks := make([]*managedTask, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	m.mu.Unlock()

	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.snapshot())
	}
	if !oldestFirst {
		sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.After(out[j].AddedAt) })
	}
	return out
}

// ---------- persistence ----------

type savedTask struct {
	View   TaskView   `json:"view"`
	Req    jobRequest `json:"req"`
	OutDir string     `json:"out_dir"`
}

type savedList struct {
	Version int   `json:"version"`
	Limit   int   `json:"limit"`
	Notify  *bool `json:"notify,omitempty"`
	// SeedPolicy is what a finished torrent does: "stop", "ratio" or "keep".
	SeedPolicy string      `json:"seed_policy,omitempty"`
	Tasks      []savedTask `json:"tasks"`
	// Settings is a pointer so a file written before it existed can be told
	// apart from one that holds it, and load can fill in the defaults.
	Settings *Settings `json:"settings,omitempty"`
	// SpeedLimit is the limit all downloads share, in bytes per second.
	SpeedLimit int64 `json:"speed_limit,omitempty"`
}

// save writes the list atomically. It includes request headers, cookies among
// them, because resuming an authenticated download needs them; the file sits
// in the user's own profile, as IDM's equivalent does.
func (m *Manager) save() error {
	if m.store == "" {
		return nil
	}
	m.mu.Lock()
	notify := m.notifyOn.Load()
	settings := m.settings.clone()
	list := savedList{
		Version: 1, Limit: m.limit, Notify: &notify, Settings: &settings,
		SpeedLimit: m.speed.Rate(), SeedPolicy: string(m.bt.Policy()),
	}
	for _, id := range m.order {
		t := m.tasks[id]
		t.mu.Lock()
		v := t.view
		v.Segments, v.Speed, v.Active = nil, 0, 0
		list.Tasks = append(list.Tasks, savedTask{View: v, Req: t.req, OutDir: t.outDir})
		t.mu.Unlock()
	}
	m.mu.Unlock()

	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := m.store + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.store)
}

// load restores the list. Anything that was moving is paused rather than
// restarted, so a crash never silently kicks off a batch of large downloads.
func (m *Manager) load() error {
	if m.store == "" {
		return nil
	}
	b, err := os.ReadFile(m.store)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Decoding over the defaults leaves any setting the file does not mention
	// as it would be on a first run.
	loaded := defaultSettings()
	list := savedList{Settings: &loaded}
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse %s: %w", m.store, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if list.Limit > 0 {
		m.limit = clampLimit(list.Limit)
	}
	if list.Settings != nil {
		m.settings = *list.Settings
		m.settings.Folders = folderNames(list.Settings.Folders)
		m.settings.Language = cleanLang(list.Settings.Language)
		m.syncAwakeLocked()
	}
	if list.Notify != nil {
		m.notifyOn.Store(*list.Notify)
	}
	m.speed.SetRate(list.SpeedLimit)
	if p, ok := parseSeedPolicy(list.SeedPolicy); ok {
		m.bt.setPolicy(p)
	}
	for _, st := range list.Tasks {
		v := st.View
		switch v.State {
		case StateRunning, StateQueued:
			v.State = StatePaused
		case StateAwaitingRefresh:
			v.State, v.RefreshUntil = StateNeedsRefresh, time.Time{}
		}
		if v.Kind == "bt" {
			v.Stage = "" // neither fetching metadata nor seeding until it runs again
		}
		if _, dup := m.tasks[v.ID]; dup || v.ID == "" {
			continue
		}
		st.Req.Connections = requestedConnections(st.Req.Connections)
		v.Conns = st.Req.Connections
		st.Req.SpeedLimit = max(st.Req.SpeedLimit, 0)
		v.SpeedLimit = st.Req.SpeedLimit
		mt := &managedTask{view: v, req: st.Req, outDir: st.OutDir}
		mt.connLimit.Store(int32(st.Req.Connections))
		mt.speed.SetRate(st.Req.SpeedLimit)
		m.tasks[v.ID] = mt
		m.order = append(m.order, v.ID)
	}
	return nil
}

func (m *Manager) saveLoop() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for range tick.C {
		if m.dirty.Swap(false) {
			if err := m.save(); err != nil {
				log.Printf("saving task list: %v", err)
				m.dirty.Store(true)
			}
		}
	}
}

// ---------- HTTP ----------

type server struct {
	mgr   *Manager
	token string
	port  int // where a player is sent to read a download in progress
}

func findRunningDaemon(timeout time.Duration) *daemonClient {
	deadline := time.Now().Add(timeout)
	for {
		if c, err := newDaemonClient(); err == nil {
			c.http.Timeout = 250 * time.Millisecond
			if c.ping() == nil {
				return c
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunDaemon binds loopback only and publishes its port so the native host and
// the UI can find it.
func RunDaemon(port int, outDir string, parallel int) error {
	// The ping handles normal repeat launches; the startup lease closes the
	// check-then-listen gap when two processes are launched at the same instant.
	if c := findRunningDaemon(0); c != nil {
		log.Printf("a godm daemon is already running at %s; nothing to do", c.base)
		return nil
	}
	releaseStartup, claimed, err := claimDaemonStartup()
	if err != nil {
		return fmt.Errorf("claim daemon startup: %w", err)
	}
	if !claimed {
		if c := findRunningDaemon(5 * time.Second); c != nil {
			log.Printf("a godm daemon is already running at %s; nothing to do", c.base)
			return nil
		}
		return errors.New("another godm daemon is still starting")
	}
	defer releaseStartup()
	if c := findRunningDaemon(0); c != nil {
		log.Printf("a godm daemon is already running at %s; nothing to do", c.base)
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// The port is held by something that is not us: take an ephemeral one.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	defer ln.Close()
	actual := ln.Addr().(*net.TCPAddr).Port

	token, err := loadOrCreateToken()
	if err != nil {
		return err
	}
	mgr := NewManager(outDir, parallel)
	mgr.store = filepath.Join(configDir(), "tasks.json")
	mgr.bt.dir = filepath.Join(configDir(), "bt")
	defer mgr.shutdownBT()
	if err := mgr.load(); err != nil {
		log.Printf("could not restore the task list: %v", err)
	}
	mgr.SetPower(osPower())
	go mgr.saveLoop()
	s := &server{mgr: mgr, token: token, port: actual}

	if err := writePort(actual); err != nil {
		return err
	}
	// Only clear the port file if it still points at us; a later daemon may
	// have taken over, and deleting its entry would strand every client.
	defer func() {
		if readPort() == actual {
			os.Remove(portPath())
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/ping", s.guard(s.handlePing))
	mux.HandleFunc("/api/tasks", s.guard(s.handleTasks))
	mux.HandleFunc("/api/download", s.guard(post(s.handleDownload)))
	mux.HandleFunc("/api/batch", s.guard(post(s.handleBatch)))
	mux.HandleFunc("/api/pause", s.guard(post(s.idAction(s.mgr.Pause))))
	mux.HandleFunc("/api/cancel", s.guard(post(s.idAction(s.mgr.Pause)))) // older extension builds
	mux.HandleFunc("/api/resume", s.guard(post(s.idAction(s.mgr.Resume))))
	mux.HandleFunc("/api/pause-all", s.guard(post(s.handlePauseAll)))
	mux.HandleFunc("/api/resume-all", s.guard(post(s.handleResumeAll)))
	mux.HandleFunc("/api/remove", s.guard(post(s.handleRemove)))
	mux.HandleFunc("/api/refresh", s.guard(post(s.handleRefresh)))
	mux.HandleFunc("/api/address", s.guard(post(s.handleAddress)))
	mux.HandleFunc("/api/limit", s.guard(post(s.handleLimit)))
	mux.HandleFunc("/api/connections", s.guard(post(s.handleConnections)))
	mux.HandleFunc("/api/speed", s.guard(post(s.handleSpeed)))
	mux.HandleFunc("/api/inspect", s.guard(post(s.handleInspect)))
	mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	mux.HandleFunc("/api/settings", s.guard(s.handleSettings))
	mux.HandleFunc("/api/browse", s.guard(post(s.handleBrowse)))
	mux.HandleFunc("/api/open", s.guard(post(s.handleOpen)))
	mux.HandleFunc("/api/play", s.guard(post(s.handlePlay)))
	mux.HandleFunc("/stream/", s.guard(s.handleStream))
	mux.HandleFunc("/api/torrent", s.guard(post(s.handleTorrentFile)))
	mux.HandleFunc("/api/seeding", s.guard(s.handleSeeding))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	ready := &daemonClient{
		base:  fmt.Sprintf("http://127.0.0.1:%d", actual),
		token: token,
		http:  &http.Client{Timeout: 250 * time.Millisecond},
	}
	deadline := time.Now().Add(2 * time.Second)
	for ready.ping() != nil {
		select {
		case err := <-serveErr:
			return err
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("godm daemon did not become ready")
		}
	}
	startTray(mgr)
	releaseStartup()
	log.Printf("godm daemon listening on http://127.0.0.1:%d/  (downloads -> %s)", actual, outDir)
	return <-serveErr
}

// guard enforces the bearer token and rejects cross-origin browser callers.
// A random web page cannot read our responses thanks to CORS, but it could
// still fire off requests, so the token is the real gate.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "chrome-extension://") ||
			strings.HasPrefix(origin, "moz-extension://") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		} else if origin != "" && !strings.HasPrefix(origin, "http://127.0.0.1:") &&
			!strings.HasPrefix(origin, "http://localhost:") {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// post restricts state-changing endpoints to POST.
func post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func (s *server) authorized(r *http.Request) bool {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		if subtleEqual(strings.TrimPrefix(v, "Bearer "), s.token) {
			return true
		}
	}
	return subtleEqual(r.URL.Query().Get("token"), s.token)
}

func (s *server) idAction(fn func(string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": fn(r.URL.Query().Get("id"))})
	}
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "version": version, "pid": os.Getpid()})
}

// handleInspect reports what a playlist contains without downloading it. A
// refusal (DRM, a live stream) comes back as ok:false with the reason, not as
// an HTTP error, so the caller can show the user why rather than "request
// failed".
func (s *server) handleInspect(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var info *StreamInfo
	var err error
	if isYTDLPJob(req) {
		info, err = InspectYTDLP(ctx, YTDLPOptions{URL: req.URL, Headers: req.Headers})
	} else {
		info, err = InspectHLS(ctx, HLSOptions{URL: req.URL, Headers: req.Headers})
	}
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "info": info})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.mgr.Add(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []jobRequest `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	ids, errs := s.mgr.AddBatch(body.Items)
	writeJSON(w, map[string]any{"ok": len(ids) > 0, "ids": ids, "errors": errs})
}

func (s *server) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ok": true, "tasks": s.mgr.List(), "limit": s.mgr.Limit(),
		"after_all": s.mgr.AfterAll(), "speed_limit": s.mgr.SpeedLimit(),
		// Carried here so that a page left open in another window follows a
		// change of language made in this one.
		"language": s.mgr.LanguageSetting(),
	})
}

func (s *server) handlePauseAll(w http.ResponseWriter, r *http.Request) {
	s.mgr.PauseAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleResumeAll(w http.ResponseWriter, r *http.Request) {
	s.mgr.ResumeAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleRemove(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, map[string]any{"ok": s.mgr.Remove(q.Get("id"), q.Get("delete") == "1")})
}

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ref, err := s.mgr.RequestRefresh(r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "referrer": ref})
}

func (s *server) handleAddress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.mgr.ChangeAddress(r.URL.Query().Get("id"), strings.TrimSpace(body.URL)); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleLimit(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil {
		http.Error(w, "n must be a number", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "limit": s.mgr.SetLimit(n)})
}

func (s *server) handleConnections(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil {
		http.Error(w, "n must be a number", http.StatusBadRequest)
		return
	}
	got, err := s.mgr.SetConnections(r.URL.Query().Get("id"), n)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "connections": got})
}

// handleSpeed sets a speed limit in bytes per second, 0 for none: for one
// download when an id is given, otherwise the one all downloads share.
func (s *server) handleSpeed(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	if err != nil {
		http.Error(w, "n must be a number", http.StatusBadRequest)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, map[string]any{"ok": true, "speed_limit": s.mgr.SetSpeedLimit(n)})
		return
	}
	got, err := s.mgr.SetTaskSpeedLimit(id, n)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "speed_limit": got})
}

// handleConfig tells the extension where downloads go and how godm is set up,
// so its confirmation dialog can show real defaults.
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ok":                 true,
		"out_dir":            s.mgr.OutDir(),
		"sort_by_type":       s.mgr.SortByType(),
		"connections":        DefaultConnections,
		"max_connections":    MaxConnections,
		"limit":              s.mgr.Limit(),
		"notifications":      s.mgr.Notifications(),
		"can_browse_folders": canBrowseFolders,
		"ytdlp":              YTDLPAvailable(),
	})
}

// handleBrowse opens the native folder chooser on the tray's UI thread.
func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	path, err := browseFolder(r.URL.Query().Get("current"))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": path})
}

func (s *server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.OpenFolder(r.URL.Query().Get("id")); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The token is injected here so the page can call the API without asking
	// the user to paste anything. So is the language setting, which lets the
	// first paint be in the right language; the page works out for itself what
	// auto means for the browser it is open in.
	page := strings.Replace(uiHTML, "__TOKEN__", s.token, 1)
	fmt.Fprint(w, strings.Replace(page, "__LANG__", s.mgr.LanguageSetting(), 1))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) || len(b) == 0 {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
