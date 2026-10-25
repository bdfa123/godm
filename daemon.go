package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Referrer string    `json:"referrer,omitempty"`
	Filename string    `json:"filename"`
	Path     string    `json:"path"`
	State    TaskState `json:"state"`
	Error    string    `json:"error,omitempty"`
	Size     int64     `json:"size"`
	Received int64     `json:"received"`
	Speed    int64     `json:"speed"`
	// Conns is how many connections the download may use right now: the
	// user's choice, held down to what the server takes once it has turned
	// connections away. A torrent counts its seeds here instead.
	Conns int `json:"conns"`
	// ConnsWanted is the user's choice itself, which the manager's menu
	// shows, and ServerCap the most connections the server has been seen to
	// take at once while this run lasts, 0 while it has turned none away. The
	// cap only holds the download back when it is the smaller of the two.
	ConnsWanted int           `json:"conns_wanted,omitempty"`
	ServerCap   int           `json:"server_cap,omitempty"`
	Active      int           `json:"active"`
	Resumable   bool          `json:"resumable"`
	Segments    []SegmentView `json:"segments,omitempty"`
	// PlayAt is the byte a media player is reading the download from, while
	// one reads an ordinary download as it arrives (see Playback), and nil
	// otherwise. It is the player's own read position, which runs ahead of
	// what is on screen by as much as the player has buffered. It is never
	// part of the view as it is kept, only of the copies snapshot hands out.
	PlayAt *int64 `json:"play_at,omitempty"`
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
	Uploaded int64     `json:"uploaded,omitempty"`
	Media    string    `json:"media,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	// StartedAt is when the run under way got its slot, set back by the time
	// the runs before it took, so that the time since is the time the
	// download has spent downloading however often it was paused, retried or
	// met a restart of godm in between. Once it is done, EndedAt less
	// StartedAt is that time in all, which is what "finished in" on the card
	// and in the notice divide the size by. Spent is the same time as of the
	// end of the last run, kept for the next run to be set back by.
	StartedAt    time.Time     `json:"started_at,omitempty"`
	EndedAt      time.Time     `json:"ended_at,omitempty"`
	Spent        time.Duration `json:"spent_ns,omitempty"`
	RefreshUntil time.Time     `json:"refresh_until,omitempty"`
	// Later says the download is on the download list, and TrySpeed how fast
	// its try fetched, in bytes per second; see downloadlist.go.
	Later    bool  `json:"later,omitempty"`
	TrySpeed int64 `json:"try_speed,omitempty"`
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
	// Variant picks a quality from a master playlist; -1 takes the best. It is
	// written out whatever it is, 0 too, which is the first one listed: a
	// request with no variant in it asks for the best, see addRequest. A list
	// saved before that has none for 0, and still reads as 0.
	Variant int `json:"variant"`
	// Later puts the download on the download list: it is tried, then waits
	// to be started. See downloadlist.go.
	Later bool `json:"later,omitempty"`
}

// addRequest is a jobRequest as a caller hands one in. The one difference is
// a quality that was not named. Inside godm that cannot be told from the
// first one listed, both being 0, so it is settled here, on the way in: a
// caller that says nothing about quality gets the best, as the browser
// extension's does. The manager's Add links dialog is such a caller.
type addRequest struct {
	jobRequest
	Variant *int `json:"variant"` // takes the place of jobRequest's when read
}

func (r addRequest) job() jobRequest {
	req := r.jobRequest
	req.Variant = -1
	if r.Variant != nil {
		req.Variant = *r.Variant
	}
	return req
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
	// state of the one that replaced it. It moves when the run is queued, not
	// when its goroutine gets going; see queueRun.
	gen         int
	waitingSlot bool
	matching    bool // probing whether this new download is a refreshed link
	inFlight    bool // Download holds the file open
	deleteAfter bool // remove the file once the run lets go of it
	removed     bool
	// relinked says the address has changed since the partial file's sidecar
	// was last written, so the next run to get as far as the file has to be
	// let continue it from a URL the sidecar does not name. It belongs to the
	// task and not to one run, and is saved with it: a run that is paused
	// while it waits in the queue, or whose first request fails, never gets
	// that far, and the run after it has to know all the same.
	relinked bool
	// tryHeld is the generation of the run that watchTry stopped because the
	// try had done its part, so that the run ends on the list.
	tryHeld int

	lastRecv int64
	lastAt   time.Time
	speedEMA float64
	// ytAt is when yt-dlp last said anything about its progress. It says nothing
	// while it extracts, between two streams or while it merges, and a speed
	// that stays on the screen through all of that is not true.
	ytAt time.Time

	// play lets a media player read the file before it has finished.
	play Playback
	// players are the stream requests holding the file open, each with what
	// ends it. Windows will not delete a file that is open, so deleting the
	// download ends them and waits for the last to let go, as it waits for
	// the run.
	players   map[uint64]func()
	playerSeq uint64
	// mapRun keeps the file mpv's map of the download is drawn from, for as
	// long as players stream it; nil when none does. See playermap.go.
	// mapMu is held over each write and removal of that file, so that one
	// writer handing over to the next never deletes what the next has written.
	mapRun *mapRun
	mapMu  sync.Mutex
}

// ytdlpSpeedStale is how long yt-dlp may go without a word about its progress
// before the speed it last gave is taken to be over. While it downloads it
// reports many times a second. It is a variable so that a test need not sit
// through it.
var ytdlpSpeedStale = 4 * time.Second

func (t *managedTask) snapshot() TaskView {
	t.mu.Lock()
	v := t.view
	if v.Speed > 0 && v.Kind == "yt-dlp" && !t.ytAt.IsZero() && time.Since(t.ytAt) > ytdlpSpeedStale {
		v.Speed = 0
	}
	t.mu.Unlock()
	// Where a player reads is the engine's to know, from one read to the next,
	// so it is looked up for each copy rather than kept in the view.
	if at, ok := t.play.focusAt(); ok {
		v.PlayAt = &at
	}
	return v
}

// queueRun marks the task as waiting to run and returns the generation of the
// run that is to do it, which the caller then starts. Callers hold t.mu.
//
// The generation moves here, in the same step as the state, because the run
// before this one may still be on its way out. A run paused while it waited
// for a slot finds that out a moment later, and what it does then, giving up
// its wait and writing "paused", must not land on a task that has been
// resumed since. If the generation moved only once the new run's goroutine
// got going, there would be a moment in which the task was queued again under
// the old run's generation: the old run's ending would be written over the
// resume, or the old run would take the slot itself on a context already
// cancelled, and the new run would find the task not queued and leave.
func (t *managedTask) queueRun() int {
	t.view.State = StateQueued
	t.gen++
	return t.gen
}

func (t *managedTask) onStart(gen int, si StartInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	if !si.Resumed || si.Path != t.view.Path {
		// The segments are the last run's, of a file this run does not go on
		// with. Until it first reports they would say its bytes are on disk,
		// and the map a player is shown would have the film play on where
		// there is nothing to play. A file started over has nothing yet.
		t.view.Segments = nil
		if !si.Resumed {
			t.view.Received = 0
		}
	}
	t.view.Path = si.Path
	t.view.Filename = filepath.Base(si.Path)
	if si.Size > 0 {
		t.view.Size = si.Size
	}
	t.view.Resumable = si.Resumable
}

// ytStart names the task after the video yt-dlp has begun on.
func (t *managedTask) ytStart(gen int, si YTStart) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	if si.Title != "" && t.view.Path == "" {
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
	t.ytAt = time.Now()
	t.view.Kind = "yt-dlp"
	// Neither a connection count nor a segment map means anything here: the
	// external tool decides how it fetches.
	t.view.Resumable = false
	t.view.Active, t.view.Conns, t.view.ConnsWanted = 0, 0, 0
	if p.Stage == ytProcessing {
		// Everything is in and yt-dlp is putting it together. The bar stays where
		// the last stream left it, and nothing is arriving.
		t.view.Stage = p.Stage
		t.view.Speed = 0
		return
	}
	t.view.Received = p.Downloaded
	t.view.Size = p.Total
	t.view.Stage = p.Stage
	t.view.Speed = p.Speed
	if p.Status == "finished" {
		// The speed that comes with the last line of a stream is its average
		// over the whole of it, and nothing is moving until the next begins.
		t.view.Speed = 0
	}
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
	t.view.ServerCap = p.ServerCap
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
	// trying counts the tries of the download list under way, which have
	// lanes of their own beside the queue; see downloadlist.go.
	trying int
	wake   chan struct{} // closed and replaced whenever scheduling state changes
	// listNext is when the download list is next started by the clock, zero
	// for never. Guarded by mu.
	listNext time.Time

	outDir string
	seq    atomic.Uint64

	store   string // tasks.json; empty disables persistence
	dirty   atomic.Bool
	saveMu  sync.Mutex // one save at a time, in the order they were asked for
	openURL func(string) error
	// loadNote is what load found wrong with the saved list, kept until there
	// is a tray to tell the user through. Guarded by mu.
	loadNote *loadNote

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
	// mapDir is where the files mpv's download map is drawn from are kept,
	// and its script; empty keeps none. mapEvery is how often a map is
	// rewritten and mapLinger how long it outlives the last player. See
	// playermap.go; tests shorten the times.
	mapDir    string
	mapEvery  time.Duration
	mapLinger time.Duration

	bt btEngine // torrents, in bt.go

	// yt is what is known about the yt-dlp on this computer: its version, the
	// newest release, and an update under way. See ytupdate.go.
	yt ytTool
}

func NewManager(outDir string, parallel int) *Manager {
	if parallel <= 0 {
		parallel = 3
	}
	m := &Manager{
		tasks:     map[string]*managedTask{},
		limit:     clampLimit(parallel),
		wake:      make(chan struct{}),
		outDir:    outDir,
		openURL:   openInBrowser,
		announce:  trayNotify,
		settings:  defaultSettings(),
		after:     afterNothing,
		mapEvery:  defaultMapEvery,
		mapLinger: defaultMapLinger,
	}
	m.notifyOn.Store(true)
	return m
}

func (m *Manager) Notifications() bool { return m.notifyOn.Load() }

func (m *Manager) SetNotifications(on bool) {
	m.notifyOn.Store(on)
	m.dirty.Store(true)
}

// OutDir is the folder a new download goes to when it names none: the one
// chosen in Settings, else the one godm was started with.
func (m *Manager) OutDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultDirLocked()
}

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

// sameActiveDownload reports whether t is the same download, to the same
// folder, waiting, running or waiting on the download list, so that asking for
// it twice does not fetch it twice.
func sameActiveDownload(t *managedTask, req jobRequest, outDir string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.view.State {
	case StateQueued, StateRunning, StateListed:
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
			State: StateQueued, Size: -1, Conns: req.Connections, ConnsWanted: req.Connections,
			AddedAt: time.Now(), SpeedLimit: req.SpeedLimit, Later: req.Later,
		},
	}
	gen := mt.queueRun() // nobody else can see the task yet
	mt.connLimit.Store(int32(req.Connections))
	mt.speed.SetRate(req.SpeedLimit)
	if req.Kind == "bt" {
		// Shown as a torrent while it waits in line; it counts peers, not
		// connections.
		mt.view.Kind, mt.view.Conns, mt.view.ConnsWanted = "bt", 0, 0
	} else if isYTDLPJob(req) {
		// And a yt-dlp download as one, from the start: until it first reports
		// progress, a few seconds in, it would be taken for a plain file and
		// offered a speed limit and a number of connections that do nothing.
		mt.view.Kind, mt.view.Conns, mt.view.ConnsWanted = "yt-dlp", 0, 0
	}

	m.mu.Lock()
	for _, existingID := range m.order {
		if existing := m.tasks[existingID]; existing != nil &&
			(sameActiveDownload(existing, req, outDir) || sameTorrent(existing, req)) {
			m.mu.Unlock()
			// The same download asked for now, while it waits on the list, is
			// the list's copy started rather than a second one.
			if !req.Later {
				m.startLater(existingID)
			}
			return existingID, nil
		}
	}
	m.tasks[id] = mt
	m.order = append(m.order, id)
	m.broadcastLocked()
	m.mu.Unlock()
	m.dirty.Store(true)

	go m.start(mt, gen, true)
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

// start runs one attempt of a task: the one queueRun gave the generation gen.
// allowMatch lets a brand-new download be absorbed by a task that is waiting
// for a refreshed link. A task whose address has changed tells the engine to
// continue existing progress from a URL that differs from before; see
// relinked.
func (m *Manager) start(mt *managedTask, gen int, allowMatch bool) {
	mt.mu.Lock()
	// Resume, refresh and address changes reserve StateQueued before starting
	// their goroutine. If Pause won the race in between, do not resurrect it;
	// and if a pause and another resume both did, that resume has a run of
	// its own and this one is not wanted.
	if mt.removed || mt.view.State != StateQueued || mt.gen != gen {
		mt.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mt.cancel = cancel
	mt.view.Error = ""
	// A server's cap on connections is learnt again by each run.
	mt.view.Speed, mt.view.Active, mt.view.ServerCap = 0, 0, 0
	mt.view.RefreshUntil = time.Time{}
	mt.lastAt, mt.lastRecv, mt.speedEMA = time.Time{}, 0, 0
	if isYTDLPJob(mt.req) {
		// Which stream it is on, or that it was putting them together, is what the
		// last run got to, and yt-dlp is some seconds in finding its way back.
		mt.view.Stage = ""
		mt.ytAt = time.Time{}
	}
	req, outDir, refresh, id := mt.req, mt.outDir, mt.relinked, mt.view.ID
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

	line, ok := m.acquire(ctx, mt, gen)
	if !ok {
		m.finishRun(mt, gen, StatePaused, "", "")
		return
	}
	defer m.release(line)

	mt.mu.Lock()
	mt.inFlight = true
	mt.mu.Unlock()
	if line == laneTry {
		go m.watchTry(ctx, mt, gen, cancel)
	}

	var donePath string
	var err error
	// planned is set once the engine has settled which file it writes to.
	planned := false
	switch {
	case isBTJob(req):
		donePath, err = m.runTorrent(ctx, mt, gen)
	case isYTDLPJob(req):
		place := ytdlpPlaceFor(outDir, id)
		donePath, err = RunYTDLP(ctx, YTDLPOptions{
			URL: req.URL,
			// yt-dlp works in a folder of its own, and what it finishes is moved
			// into the download folder after; see ytdlpPlace.
			OutDir:  place.files,
			TempDir: place.work,
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
		if err == nil {
			donePath, err = mt.settleYTDLP(place, outDir, donePath)
			if errors.Is(err, errRemovedWithFiles) {
				cancel()
				err = ctx.Err()
			}
		}
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
			Refresh:     refresh,
			OnStart: func(si HLSStart) {
				planned = true
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
				planned = true
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
	if refresh && (planned || errors.As(err, &mismatch)) {
		// The engine got as far as the partial file. Either it took it up, and
		// has by now written the new address into the sidecar or finished the
		// file, or it refused the address and said why. Both ways the change
		// of address has been dealt with. A run that ended before this, at
		// its first request or still in the queue, leaves it for the next.
		mt.mu.Lock()
		if mt.gen == gen {
			mt.relinked = false
		}
		mt.mu.Unlock()
	}
	switch {
	case err == nil:
		m.finishRun(mt, gen, StateDone, "", donePath)
	case ctx.Err() != nil:
		if m.finishRun(mt, gen, StatePaused, "", "") {
			m.Resume(id)
		}
	case errors.As(err, &expired), errors.As(err, &mismatch):
		m.finishRun(mt, gen, StateNeedsRefresh, err.Error(), "")
	default:
		reason := err.Error()
		if isYTDLPJob(req) {
			// A yt-dlp that is older than the newest release is the likeliest
			// reason a site it knew stops working, and its error does not say so.
			reason = m.ytdlpFailureText(err)
		}
		m.finishRun(mt, gen, StateError, reason, "")
	}
}

// lane is which of the two lines a run took its slot from: the queue of
// downloads, or the tries of the download list.
type lane int

const (
	laneQueue lane = iota
	laneTry
)

// acquire blocks until the task may run: a slot is free and no task added
// before it is still waiting. A download list is only predictable if it
// starts in the order it was built. A task on the download list waits in the
// line of tries instead, and one taken off the list while it waits moves over
// to the queue. It says which line the slot came from.
func (m *Manager) acquire(ctx context.Context, mt *managedTask, gen int) (lane, bool) {
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
		mt.mu.Lock()
		try := mt.req.Later
		mt.mu.Unlock()
		free := m.running < m.limit && m.nextInLineLocked() == mt
		if try {
			free = m.trying < tryLanes && m.nextTryLocked() == mt
		}
		if free {
			mt.mu.Lock()
			if mt.req.Later != try {
				// Taken off the list since it was looked at: it waits in the
				// queue's line now.
				mt.mu.Unlock()
				m.mu.Unlock()
				continue
			}
			ok := mt.gen == gen && !mt.removed
			if ok {
				mt.waitingSlot = false
				mt.view.State = StateRunning
				// Not simply now: a resumed download would then say it took
				// only as long as its last run. See StartedAt.
				mt.view.StartedAt = time.Now().Add(-mt.view.Spent)
			}
			mt.mu.Unlock()
			got := laneQueue
			if ok {
				if try {
					m.trying++
					got = laneTry
				} else {
					m.running++
				}
				m.syncAwakeLocked()
			}
			m.broadcastLocked()
			m.mu.Unlock()
			m.dirty.Store(true)
			return got, ok
		}
		ch := m.wake
		m.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			m.mu.Lock()
			m.broadcastLocked() // the head of the line may have just changed
			m.mu.Unlock()
			return laneQueue, false
		}
	}
}

// nextInLineLocked returns the oldest queued task. It goes by queue position,
// not by which goroutine happened to reach acquire first: goroutines start in
// no particular order, and a list that runs 1, 3, 2 is not a queue. A task that
// is busy checking whether it is a refreshed link is skipped so a slow probe
// cannot hold up everything behind it, and so is one waiting for its try on
// the download list, which has a line of its own.
func (m *Manager) nextInLineLocked() *managedTask {
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		eligible := t.view.State == StateQueued && !t.matching && !t.req.Later
		t.mu.Unlock()
		if eligible {
			return t
		}
	}
	return nil
}

func (m *Manager) release(l lane) {
	m.mu.Lock()
	if l == laneTry {
		m.trying--
	} else {
		m.running--
	}
	m.syncAwakeLocked()
	m.broadcastLocked()
	m.mu.Unlock()
}

func (m *Manager) afterRun(mt *managedTask) {
	mt.mu.Lock()
	mt.inFlight = false
	del, path := mt.deleteAfter && len(mt.players) == 0, mt.view.Path
	mt.mu.Unlock()
	if del {
		deleteDownload(path)
		mt.discardYTDLP()
	}
}

// errRemovedWithFiles says a download finished just as it was removed along
// with its files.
var errRemovedWithFiles = errors.New("removed along with its files")

// settleYTDLP moves what yt-dlp finished into the download folder, see
// ytdlpPlace.settle, unless the download has been removed with its files in
// the meantime. Moving the file out then would only give afterRun, which clears
// the folder, a file it does not know of.
func (t *managedTask) settleYTDLP(place ytdlpPlace, outDir, path string) (string, error) {
	t.mu.Lock()
	gone := t.deleteAfter
	t.mu.Unlock()
	if gone {
		return "", errRemovedWithFiles
	}
	final, err := place.settle(outDir, path)
	if err != nil {
		return "", err
	}
	// From here a removal with the file knows which file to delete, and one that
	// came while the file was being moved gets its way too: afterRun reads the
	// path when the run ends.
	t.mu.Lock()
	t.view.Path = final
	t.mu.Unlock()
	return final, nil
}

// discardYTDLP removes the folder a yt-dlp download works in, with the parts
// of it that are not finished. It is nothing for any other kind of download.
func (t *managedTask) discardYTDLP() {
	if isYTDLPJob(t.req) {
		ytdlpPlaceFor(t.outDir, t.view.ID).discard()
	}
}

// finishRun writes how a run ended. A run called off for the download list
// (see stoppedLocked) may end on the list rather than paused, or ask, by
// returning true, to be run again.
func (m *Manager) finishRun(mt *managedTask, gen int, st TaskState, errMsg, path string) (again bool) {
	finalSize, haveFinalSize := int64(0), false
	if st == StateDone && path != "" {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			finalSize, haveFinalSize = fi.Size(), true
		}
	}
	mt.mu.Lock()
	if mt.gen != gen {
		mt.mu.Unlock()
		return false
	}
	if st == StatePaused {
		st, again = mt.stoppedLocked(gen)
	}
	now := time.Now()
	if mt.view.State == StateRunning {
		// The run had a slot, and StartedAt is when it got it less what the
		// runs before it took. A run that ends while still in the queue
		// spent no time, and the time since StartedAt is not its own.
		mt.view.Spent = max(now.Sub(mt.view.StartedAt), 0)
	}
	mt.view.State = st
	mt.view.Error = errMsg
	if mt.view.ServerCap > 0 && mt.view.Kind != "bt" {
		// The server's cap was this run's to learn and goes with it, and the
		// next run may use what was chosen until the server says otherwise.
		mt.view.Conns = mt.req.Connections
	}
	mt.view.Speed, mt.view.Active, mt.view.ServerCap = 0, 0, 0
	if mt.view.Kind == "bt" && st != StateDone {
		// A torrent's Conns is its seeds, and a run that stops short takes
		// them with it as it does the peers. One that finished may seed on;
		// its counts are torrentStage's from then.
		mt.view.Conns = 0
	}
	if path != "" {
		mt.view.Path = path
		mt.view.Filename = filepath.Base(path)
	}
	if st == StateDone {
		if haveFinalSize {
			mt.view.Size = finalSize
		}
		// A file small enough to finish in its try is done, not waiting.
		mt.setLaterLocked(false)
		mt.view.EndedAt = now
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

	if st != StatePaused && st != StateListed {
		m.checkAfterAll(st == StateDone)
	}
	go m.announceFinish(view, st)
	return again
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

// Resume lets a stopped download run again. One on the download list is taken
// off it and runs in full: it is wanted now.
func (m *Manager) Resume(id string) bool { return m.resume(id, false) }

// resume is Resume, but with onList it leaves a download on the download list,
// where it is tried again and then waits.
func (m *Manager) resume(id string, onList bool) bool {
	mt := m.get(id)
	if mt == nil {
		return false
	}
	mt.mu.Lock()
	switch mt.view.State {
	case StatePaused, StateError, StateNeedsRefresh, StateAwaitingRefresh, StateListed:
		gen := mt.queueRun()
		mt.view.Error = ""
		mt.view.RefreshUntil = time.Time{}
		if !onList {
			mt.setLaterLocked(false)
		}
		mt.mu.Unlock()
		m.dirty.Store(true)
		m.mu.Lock()
		m.broadcastLocked()
		m.mu.Unlock()
		go m.start(mt, gen, false)
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

// ResumeAll continues what was paused or failed. What is on the download list
// stays on it: one stopped before its try is done is tried again, and one that
// waits goes on waiting for the list to be started.
func (m *Manager) ResumeAll() {
	for _, v := range m.list(true) { // oldest first, so the queue keeps its order
		if v.State == StatePaused || v.State == StateError {
			m.resume(v.ID, v.Later)
		}
	}
}

// Remove drops a task. With deleteFile the partial or finished file goes too;
// if a run or a player still has it open, deletion waits until they let go,
// and for a while longer if some other program has it; see deleteDownload.
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
	// The map a player draws goes at once, whether or not the player is cut
	// off: the download it shows is gone.
	if w := mt.mapRun; w != nil {
		w.end()
	}
	cancel, path := mt.cancel, mt.view.Path
	busy := mt.inFlight || len(mt.players) > 0
	bt := isBTJob(mt.req)
	if deleteFile {
		// A torrent deletes its own files once the client lets go of them,
		// retrying until they are free, so only a plain file waits here.
		if busy && !bt {
			mt.deleteAfter = true
		}
		// Players reading the file are cut off so it can go. This happens
		// under the lock because a request drops out under it before it
		// ends, so no stop reaches a connection gone on to another request.
		for _, stop := range mt.players {
			stop()
		}
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
		mt.discardYTDLP()
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

// deleteFor is how long deleting a removed download's file keeps trying. It is
// a variable so that a test need not sit through it.
var deleteFor = btDeleteFor

// deleteDownload deletes a download's file and then its sidecar. Callers have
// seen to it that godm itself has let go of the file, but Windows will not
// delete one that any program has open, and others do: the player that Play
// started on a finished film, a virus scan, an archive tool. So a file that
// will not go is tried again for a while in the background, as a torrent's
// files are, and named in the log if it is still there at the end.
func deleteDownload(path string) {
	if path == "" {
		return
	}
	// The name stays reserved for as long as the file is on its way out. Its
	// sidecar is still beside it, and a download of the same link added in
	// that time would take the two for its own unfinished work and write into
	// a file that is deleted under it. A name that is already taken means a
	// download is writing there now, and the file is no longer this one's.
	release, ok := reserveTarget(path)
	if !ok {
		return
	}
	keepDeleting(func() error { return removeDownload(path) }, release)
}

// removeDownload is one try at deleting a download's file. The sidecar goes
// only once the file has: a partial file left without one could not even be
// continued.
func removeDownload(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	os.Remove(statePath(path))
	return nil
}

// keepDeleting runs try, and if that fails runs it again and again in the
// background for as long as deleteFor before giving up and saying so in the
// log. after is called when it is over, either way.
func keepDeleting(try func() error, after func()) {
	if try() == nil {
		after()
		return
	}
	go func() {
		defer after()
		deadline := time.Now().Add(deleteFor)
		for wait := 50 * time.Millisecond; ; wait = min(2*wait, time.Second) {
			time.Sleep(wait)
			err := try()
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				log.Printf("removing a download: its file could not be deleted and was left where it is: %v", err)
				return
			}
		}
	}()
}

// removeTree is os.RemoveAll, repeated in the background for as long as
// deleteFor if something in the tree is still open, with a line in the log if
// it is still there at the end.
func removeTree(dir, what string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	go func() {
		deadline := time.Now().Add(deleteFor)
		for wait := 50 * time.Millisecond; ; wait = min(2*wait, time.Second) {
			time.Sleep(wait)
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				log.Printf("removing %s: could not delete %s and left it where it is: %v", what, dir, err)
				return
			}
		}
	}()
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

	// The probe took a while and the user may have removed either download
	// meanwhile. Remove takes a task off the list first and leaves its state
	// alone, so the state says nothing about it; the list does, and holding
	// the list's lock from the check to the new download leaving it means a
	// removal comes wholly before the adoption or wholly after. Without the
	// waiting task the new download is all there is and carries on as itself.
	// Without the new one there is no fresh link: the user took it back.
	m.mu.Lock()
	target.mu.Lock()
	if m.tasks[target.view.ID] != target || m.tasks[newID] != nt ||
		target.view.State != StateAwaitingRefresh { // or another download adopted it first
		target.mu.Unlock()
		m.mu.Unlock()
		return false
	}
	target.req.URL = req.URL
	if len(req.Headers) > 0 {
		target.req.Headers = req.Headers // fresh cookies belong with the fresh link
	}
	if req.Referrer != "" {
		target.req.Referrer, target.view.Referrer = req.Referrer, req.Referrer
	}
	target.view.URL = req.URL
	// Whoever asked last decides whether it waits on the download list: it
	// does only if both did.
	target.setLaterLocked(target.req.Later && req.Later)
	gen := target.queueRun()
	target.view.RefreshUntil = time.Time{}
	target.relinked = true
	name := target.view.Filename
	target.mu.Unlock()
	m.detachLocked(newID)
	m.mu.Unlock()
	m.dirty.Store(true)

	log.Printf("refreshed link for %s (%d bytes)", name, pr.Size)
	go m.start(target, gen, false)
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
	if mt.view.Path != "" && mt.view.Received > 0 && mt.view.State != StateDone {
		// There is progress to keep, and it is saved under the old address.
		mt.relinked = true
	} else {
		// Nothing of what was fetched before is kept, so the time it took is
		// no part of how long this download takes.
		mt.view.Spent = 0
	}
	gen := mt.queueRun()
	mt.view.Error = ""
	mt.mu.Unlock()
	m.dirty.Store(true)
	m.mu.Lock()
	m.broadcastLocked()
	m.mu.Unlock()
	go m.start(mt, gen, false)
	return nil
}

// SetConnections changes how many connections a task may use. A running
// download picks it up immediately: more connections split the remaining
// ranges, fewer release connections after their current chunk. A server that
// has been seen to take fewer still holds the download to that many, and the
// count it may use now says so at once rather than at the next report.
func (m *Manager) SetConnections(id string, n int) (int, error) {
	mt := m.get(id)
	if mt == nil {
		return 0, fmt.Errorf("no such task")
	}
	n = clampConnections(n)
	mt.mu.Lock()
	mt.req.Connections = n
	mt.view.ConnsWanted = n
	mt.view.Conns = n
	if c := mt.view.ServerCap; c > 0 && c < n {
		mt.view.Conns = c
	}
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
// the cap with 0. Running downloads slow down or speed up at once. Torrents
// keep to the same figure through the torrent client; see setDownloadRate.
func (m *Manager) SetSpeedLimit(n int64) int64 {
	m.speed.SetRate(n)
	m.bt.setDownloadRate(m.speed.Rate())
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
	// Relinked is the task's relinked: an address changed while godm was
	// closed, or just before it, is still a changed address afterwards.
	Relinked bool `json:"relinked,omitempty"`
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

// backupPath is where the list is kept as it was before the last save.
func backupPath(store string) string { return store + ".bak" }

// writeList is a variable so that a test can make a save fail part of the way.
var writeList = writeFileAtomic

// save writes the list atomically. It includes request headers, cookies among
// them, because resuming an authenticated download needs them; the file sits
// in the user's own profile, as IDM's equivalent does.
func (m *Manager) save() error {
	if m.store == "" {
		return nil
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
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
		v.Segments, v.Speed, v.Active, v.ServerCap = nil, 0, 0, 0
		if v.State == StateRunning {
			// A run notes the time it took when it ends, and a godm that
			// dies never sees this one end. The list says how far it had got.
			v.Spent = max(time.Since(v.StartedAt), 0)
		}
		list.Tasks = append(list.Tasks, savedTask{
			View: v, Req: t.req, OutDir: t.outDir, Relinked: t.relinked,
		})
		t.mu.Unlock()
	}
	m.mu.Unlock()

	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	// The list being replaced becomes the backup, so there is always one whole
	// generation on disk whatever happens to the other. For a moment there is
	// no tasks.json at all; load takes that to mean the backup is the list.
	// Failing to keep a backup is no reason not to save.
	if err := os.Rename(m.store, backupPath(m.store)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("could not keep the last task list as a backup: %v", err)
	}
	// writeFileAtomic flushes the file before giving it its name. Without that
	// a power cut can leave the name on NTFS with none of the data behind it,
	// and a save every two seconds while downloads run gives it every chance.
	return writeList(m.store, b)
}

// readTaskList reads one saved list. A field of the wrong type is an error,
// but encoding/json reads everything else all the same and what comes back
// holds it; partlyRead tells that case from one with nothing usable in it.
func readTaskList(path string) (savedList, error) {
	// Decoding over the defaults leaves any setting the file does not mention
	// as it would be on a first run.
	loaded := defaultSettings()
	list := savedList{Settings: &loaded}
	b, err := os.ReadFile(path)
	if err != nil {
		return list, err
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return list, fmt.Errorf("parse %s: %w", path, err)
	}
	return list, nil
}

func partlyRead(err error) bool {
	var wrongType *json.UnmarshalTypeError
	return errors.As(err, &wrongType)
}

// setAside takes a list that could not be read out of the way of the next
// save, under a name that says when, and returns where it went. It is the
// only copy of the user's queue: with it kept, somebody can still get the
// links back out of it. A file that cannot be moved is copied.
func setAside(path string) (string, error) {
	stamp := path + ".bad-" + time.Now().Format("20060102-150405")
	to := stamp
	for n := 2; ; n++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		}
		to = fmt.Sprintf("%s-%d", stamp, n)
	}
	moveErr := os.Rename(path, to)
	if moveErr == nil {
		return to, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", moveErr
	}
	if err := writeFileAtomic(to, b); err != nil {
		return "", moveErr
	}
	return to, nil
}

// loadNote is how a list that could not be read was dealt with: how names the
// words for it, and kept is where the unreadable file was set aside, empty
// when it could not be.
type loadNote struct {
	how, kept string
}

// load restores the list. Anything that was moving is paused rather than
// restarted, so a crash never silently kicks off a batch of large downloads.
//
// A list that cannot be read is never left where the next save would write
// over it, which any change at all brings on within two seconds. It is set
// aside, and what replaces it is the best there is: the backup save keeps,
// failing that whatever of the file itself could be read, failing that
// nothing. The error says which, and announceLoad tells the user.
func (m *Manager) load() error {
	if m.store == "" {
		return nil
	}
	list, err := readTaskList(m.store)
	if err == nil {
		m.restore(list)
		return nil
	}
	bak := backupPath(m.store)
	prev, prevErr := readTaskList(bak)
	damaged, why := m.store, err
	if errors.Is(err, os.ErrNotExist) {
		if errors.Is(prevErr, os.ErrNotExist) {
			return nil // nothing has been saved yet
		}
		if prevErr == nil {
			// A save was cut off after the list it was replacing had become
			// the backup and before the new one was in place. Nothing is
			// damaged and nothing but that last save is missing.
			log.Printf("%s is missing; using %s", m.store, bak)
			m.restore(prev)
			m.dirty.Store(true)
			return nil
		}
		damaged, why = bak, prevErr
	}

	how, outcome := "notify.listLost", "starting with an empty list"
	switch {
	case prevErr == nil:
		m.restore(prev)
		how, outcome = "notify.listBak", "the last backup was restored"
	case partlyRead(err):
		m.restore(list)
		how, outcome = "notify.listPart", "what could be read of it was restored"
	case partlyRead(prevErr):
		m.restore(prev)
		how, outcome = "notify.listPart", "what could be read of the last backup was restored"
	}
	kept, keepErr := setAside(damaged)
	switch {
	case keepErr == nil:
		outcome += "; the unreadable file is kept as " + kept
		m.dirty.Store(true) // put a list that can be read back in its place
	case how == "notify.listLost":
		// Not read and not set aside either, which is what a file another
		// program holds open looks like, and such a file is most likely fine.
		// Saving would put an empty list where it is, so this run does not
		// save, and the next start gets another try at reading it.
		how = "notify.listStuck"
		outcome += fmt.Sprintf("; the file could not be set aside (%v), so nothing is saved until godm is restarted", keepErr)
		m.store = ""
	default:
		// What was restored is the file's own contents or one save short of
		// them, so losing the file to a later save costs little.
		outcome += fmt.Sprintf("; the unreadable file could not be set aside (%v)", keepErr)
		m.dirty.Store(true)
	}
	m.mu.Lock()
	m.loadNote = &loadNote{how: how, kept: kept}
	m.mu.Unlock()
	return fmt.Errorf("%w; %s", why, outcome)
}

// announceLoad tells the user that the saved list could not be read as it was
// left, when load found that. It is a step of its own because load runs before
// there is a tray to say anything through. Like a warning about the computer's
// power it ignores the notifications switch, which may be among what was lost.
// A click on the notice shows the file that was set aside.
func (m *Manager) announceLoad() {
	m.mu.Lock()
	n := m.loadNote
	m.loadNote = nil
	m.mu.Unlock()
	if n == nil || m.announce == nil {
		return
	}
	text, open := m.tr(n.how), "app"
	if n.kept != "" {
		text += "\n" + m.tr("notify.listKept", "file", filepath.Base(n.kept))
		open = n.kept
	}
	m.announce(m.tr("notify.listBad"), text, open)
}

// restore takes a saved list as the manager's own.
func (m *Manager) restore(list savedList) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if list.Limit > 0 {
		m.limit = clampLimit(list.Limit)
	}
	if list.Settings != nil {
		m.settings = *list.Settings
		m.settings.Folders = folderNames(list.Settings.Folders)
		m.settings.Language = cleanLang(list.Settings.Language)
		if at, err := checkListAt(m.settings.ListAt); err == nil {
			m.settings.ListAt = at
		} else {
			m.settings.ListAt = ""
		}
		// A start due a moment ago, missed while godm was being restarted, is
		// made now rather than tomorrow.
		m.planListLocked(time.Now().Add(-listLate))
		m.syncAwakeLocked()
	}
	if list.Notify != nil {
		m.notifyOn.Store(*list.Notify)
	}
	m.speed.SetRate(list.SpeedLimit)
	m.bt.setDownloadRate(m.speed.Rate())
	if p, ok := parseSeedPolicy(list.SeedPolicy); ok {
		m.bt.setPolicy(p)
	}
	for _, st := range list.Tasks {
		v := st.View
		switch v.State {
		case StateRunning, StateQueued:
			v.State = StatePaused
			if st.Req.Later && v.Received > 0 {
				// Its try was cut short, but got far enough to be one; it waits
				// on the list as it would have. One that got nothing is
				// paused, and Resume all tries it again.
				v.State = StateListed
			}
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
		v.Conns, v.ConnsWanted, v.ServerCap = st.Req.Connections, st.Req.Connections, 0
		if v.Kind == "bt" || isBTJob(st.Req) {
			// For a torrent this is the seeds it is connected to, as in Add,
			// and nothing restored from the list is connected to anybody.
			v.Conns, v.ConnsWanted = 0, 0
		}
		if isYTDLPJob(st.Req) {
			// And a yt-dlp download has no connections of godm's at all.
			v.Kind, v.Conns, v.ConnsWanted = "yt-dlp", 0, 0
		}
		st.Req.SpeedLimit = max(st.Req.SpeedLimit, 0)
		v.SpeedLimit = st.Req.SpeedLimit
		v.Later = st.Req.Later
		mt := &managedTask{
			view: v, req: st.Req, outDir: st.OutDir,
			relinked: st.Relinked,
		}
		mt.connLimit.Store(int32(st.Req.Connections))
		mt.speed.SetRate(st.Req.SpeedLimit)
		m.tasks[v.ID] = mt
		m.order = append(m.order, v.ID)
	}
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
	mgr.mapDir = filepath.Join(configDir(), "mpv")
	defer mgr.shutdownBT()
	if err := mgr.load(); err != nil {
		log.Printf("the task list could not be read as it was saved: %v", err)
	}
	mgr.SetPower(osPower())
	go mgr.saveLoop()
	stopClock := make(chan struct{})
	defer close(stopClock)
	go mgr.listClock(stopClock)
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

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
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
	// The tray makes its window on a thread of its own and drops a notice
	// that arrives before the window exists, so this one waits for it.
	time.AfterFunc(3*time.Second, mgr.announceLoad)
	releaseStartup()
	log.Printf("godm daemon listening on http://127.0.0.1:%d/  (downloads -> %s)", actual, outDir)
	return <-serveErr
}

// routes is everything the daemon answers. Each address but the page's own
// goes through guard; the page checks its caller itself, in handleUI.
func (s *server) routes() *http.ServeMux {
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
	mux.HandleFunc("/api/list/start", s.guard(post(s.handleListStart)))
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
	mux.HandleFunc("/api/ytdlp", s.guard(s.handleYTDLP))
	mux.HandleFunc("/api/ytdlp/check", s.guard(post(s.handleYTDLPCheck)))
	mux.HandleFunc("/api/ytdlp/update", s.guard(post(s.handleYTDLPUpdate)))
	return mux
}

// addressedToUs reports whether a request names the daemon's own loopback
// address as the host it was sent to. A web page can point a name of its own
// at 127.0.0.1 (DNS rebinding), and the browser then takes the daemon for that
// page's own server and lets it read every answer; no Origin header gives it
// away on a GET. What the browser cannot change is the name it sends: it is
// the page's, not ours. The port has to be the one the request arrived on,
// which is the daemon's whichever port it ended up with.
func (s *server) addressedToUs(r *http.Request) bool {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false // no port at all: nothing reaches a daemon on port 80
	}
	if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
		return false
	}
	own := s.port
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok {
		own = a.Port
	}
	return port == strconv.Itoa(own)
}

// guard enforces the bearer token and rejects cross-origin browser callers.
// A random web page cannot read our responses thanks to CORS, but it could
// still fire off requests, so the token is the real gate.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.addressedToUs(r) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
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
// inspectBudget is as long as a page is given to be read for the qualities it
// offers. A variable so that a test need not wait it out.
var inspectBudget = 30 * time.Second

func (s *server) handleInspect(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inspectBudget)
	defer cancel()
	var info *StreamInfo
	var err error
	if isYTDLPJob(req) {
		info, err = InspectYTDLP(ctx, YTDLPOptions{URL: req.URL, Headers: req.Headers})
	} else {
		info, err = InspectHLS(ctx, HLSOptions{URL: req.URL, Headers: req.Headers})
	}
	if err != nil {
		reason := err.Error()
		if isYTDLPJob(req) && ctx.Err() == nil {
			reason = s.mgr.ytdlpFailureText(err)
		}
		writeJSON(w, map[string]any{"ok": false, "error": reason})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "info": info})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	// One link at a time comes this way, from the extension, which can still
	// hand a .torrent back to the browser if godm cannot get at it. So the
	// file is fetched before the answer; a batch leaves it to the task.
	job := req.job()
	var id string
	var err error
	if isBTJob(job) && !isMagnet(job.URL) {
		id, err = s.mgr.AddTorrentLink(r.Context(), job)
	} else {
		id, err = s.mgr.Add(job)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []addRequest `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	jobs := make([]jobRequest, 0, len(body.Items))
	for _, it := range body.Items {
		jobs = append(jobs, it.job())
	}
	ids, errs := s.mgr.AddBatch(jobs)
	writeJSON(w, map[string]any{"ok": len(ids) > 0, "ids": ids, "errors": errs})
}

func (s *server) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ok": true, "tasks": s.mgr.List(), "limit": s.mgr.Limit(),
		"after_all": s.mgr.AfterAll(), "speed_limit": s.mgr.SpeedLimit(),
		// Carried here so that a page left open in another window follows a
		// change of language made in this one.
		"language": s.mgr.LanguageSetting(),
		// And the download list's start by the clock, which its tab shows.
		"list_at": s.mgr.listAt(), "list_daily": s.mgr.listDaily(), "list_next": s.mgr.ListNext(),
	})
}

// handleListStart starts everything on the download list.
func (s *server) handleListStart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "started": s.mgr.StartList()})
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
	id := r.URL.Query().Get("id")
	got, err := s.mgr.SetConnections(id, n)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// With the server's cap, so that the answer can say when the server takes
	// fewer than were just asked for. Without it the page shows the choice
	// saved and, a moment later, a smaller number in use, which reads as the
	// choice not having been taken.
	serverCap := 0
	if mt := s.mgr.get(id); mt != nil {
		serverCap = mt.snapshot().ServerCap
	}
	writeJSON(w, map[string]any{"ok": true, "connections": got, "server_cap": serverCap})
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

// lockedHTML is what / answers to a caller that showed no token. There is no
// secret in it. The manager page takes the token out of its own address once
// it has loaded, so reloading it arrives here; the page left the token where
// only this tab can read it, and this sends the tab back with it. Anybody else
// is told how the manager is opened.
const lockedHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="color-scheme" content="light dark">
<title>godm</title>
</head>
<body style="font:14px system-ui,sans-serif;margin:3em">
<p id="why" hidden>__WHY__</p>
<script>
var kept = null;
try { kept = sessionStorage.getItem("godm-token"); } catch (e) {}
if (kept) location.replace("/?token=" + encodeURIComponent(kept));
else document.getElementById("why").hidden = false;
</script>
<noscript><p>__WHY__</p></noscript>
</body>
</html>
`

// handleUI serves the manager page. The page holds the token, so being given
// the page is being given the whole API: it goes only to a caller that already
// has the token, which godm app, godm ui and the tray put in the address.
// Anything else on this computer can reach loopback, another account's
// programs included, and they cannot read the token file.
func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.addressedToUs(r) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.authorized(r) {
		// A token that is wrong gets no second try from the stand-in page:
		// the one it would send is the one that was just refused.
		if r.URL.Query().Has("token") || r.Header.Get("Authorization") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, strings.ReplaceAll(lockedHTML, "__WHY__", html.EscapeString(s.mgr.tr("page.locked"))))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
