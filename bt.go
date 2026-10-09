package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	_ "godm/internal/torrentio" // before storage initialises; see the package
)

// Torrents run inside the daemon on github.com/anacrolix/torrent. A torrent is
// one more kind of task: it waits its turn in the same queue, pauses and
// resumes like the rest, and its files land in the task's download folder.
// What it does not share with the HTTP engine is the transport, so none of the
// segment machinery applies: pieces come from whichever peers have them.
//
// One client serves every torrent, and it is only started by the first torrent
// task, because starting it opens a listening port the rest of godm never
// needs. On Windows that first listen is what makes the firewall ask whether
// godm may accept connections from other computers. Saying no still lets
// downloads work, with fewer peers: godm can always connect out.

const (
	// btListenPort is fixed so that someone forwarding a port by hand can rely
	// on it. godm never asks the router to forward one (no UPnP, no NAT-PMP).
	btListenPort = 16881
	// maxTorrentFile bounds a .torrent read from the network or the UI. The
	// largest real ones are a few megabytes of piece hashes.
	maxTorrentFile = 16 << 20
	btTick         = 500 * time.Millisecond
	// torrentReadahead is how far ahead of a player the pieces are pulled
	// forward: enough to ride out a slow peer, not so much that it starves the
	// spot being watched.
	torrentReadahead = 16 << 20
)

// SeedPolicy says what a finished torrent does next. The default is to stop at
// once: uploading to strangers is something the user should choose, not
// discover.
type SeedPolicy string

const (
	SeedStop  SeedPolicy = "stop"  // stop uploading the moment the download completes
	SeedRatio SeedPolicy = "ratio" // keep going until as much has gone out as came in
	SeedKeep  SeedPolicy = "keep"  // keep going until the task is removed or godm quits
)

// seedChoices is what a settings page offers, in the order it offers them.
var seedChoices = []map[string]string{
	{"value": string(SeedStop), "label": "Stop when done"},
	{"value": string(SeedRatio), "label": "Until ratio 1.0"},
	{"value": string(SeedKeep), "label": "Keep seeding"},
}

func parseSeedPolicy(s string) (SeedPolicy, bool) {
	switch p := SeedPolicy(strings.ToLower(strings.TrimSpace(s))); p {
	case SeedStop, SeedRatio, SeedKeep:
		return p, true
	}
	return "", false
}

// wantSeed decides whether a finished torrent keeps uploading.
func wantSeed(p SeedPolicy, uploaded, size int64) bool {
	switch p {
	case SeedKeep:
		return true
	case SeedRatio:
		return uploaded < size
	}
	return false
}

// ---------- telling a torrent from a file ----------

// isBTJob is true for a magnet link, a link to a .torrent file, or a job that
// says outright it is a torrent.
func isBTJob(req jobRequest) bool {
	if req.Kind != "" {
		return strings.EqualFold(req.Kind, "bt")
	}
	return isTorrentURL(req.URL)
}

func isTorrentURL(raw string) bool {
	if isMagnet(raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".torrent")
}

func isMagnet(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "magnet:")
}

// magnetInfoHash reads the v1 info hash out of a magnet link. That hash is what
// the client knows a torrent by, so a link without one cannot be used.
func magnetInfoHash(link string) (metainfo.Hash, bool) {
	if !isMagnet(link) {
		return metainfo.Hash{}, false
	}
	m, err := metainfo.ParseMagnetV2Uri(link)
	if err != nil || !m.InfoHash.Ok {
		return metainfo.Hash{}, false
	}
	return m.InfoHash.Value, true
}

func validateTorrentURL(link string) error {
	if isMagnet(link) {
		if _, ok := magnetInfoHash(link); !ok {
			return errors.New("this magnet link has no BitTorrent v1 info hash (urn:btih)")
		}
		return nil
	}
	return validateURL(link)
}

// torrentDisplayName is what the list shows before the torrent's own details
// arrive: the name the magnet link carries, if any.
func torrentDisplayName(link string) string {
	if !isMagnet(link) {
		return ""
	}
	m, err := metainfo.ParseMagnetV2Uri(link)
	if err != nil {
		return ""
	}
	if n := sanitize(m.DisplayName); n != "" {
		return n
	}
	if m.InfoHash.Ok {
		return "magnet " + m.InfoHash.Value.HexString()[:12]
	}
	return ""
}

// sameTorrent is true when a new job names a torrent this task already has.
// Unlike a file, a torrent can be in the client only once, whatever state the
// existing task is in, so the duplicate is folded into it.
func sameTorrent(t *managedTask, req jobRequest) bool {
	if !isBTJob(req) {
		return false
	}
	t.mu.Lock()
	link, bt := t.req.URL, isBTJob(t.req)
	t.mu.Unlock()
	if !bt {
		return false
	}
	a, okA := magnetInfoHash(link)
	b, okB := magnetInfoHash(req.URL)
	if okA && okB {
		return a == b
	}
	return link == req.URL
}

func (t *managedTask) isTorrent() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return isBTJob(t.req)
}

func (t *managedTask) isRemoved() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removed
}

// torrentOwner returns the task, other than self, that already holds a torrent.
func (m *Manager) torrentOwner(ih metainfo.Hash, self *managedTask) *managedTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil || t == self {
			continue
		}
		t.mu.Lock()
		link, bt := t.req.URL, isBTJob(t.req)
		t.mu.Unlock()
		if h, ok := magnetInfoHash(link); bt && ok && h == ih {
			return t
		}
	}
	return nil
}

// ---------- the shared client ----------

// btEngine owns the one torrent client and what it keeps on disk. The zero
// value is ready to use once dir is set; nothing starts until a torrent does.
type btEngine struct {
	// dir keeps a copy of every torrent's metainfo and the database of finished
	// pieces. The daemon puts it in the config folder; tests use a temporary one.
	dir string
	// configure adjusts the client before it starts. Tests use it to keep
	// everything on 127.0.0.1 with no DHT and no trackers.
	configure func(*torrent.ClientConfig)

	mu     sync.Mutex
	cl     *torrent.Client
	pc     storage.PieceCompletion
	slog   *slog.Logger // the client's warnings, into godm's log
	fslog  *slog.Logger // the file storage's, errors only
	loaded map[metainfo.Hash]*btLoaded
	// stores is the storage each task's torrent last wrote through. It
	// outlives the torrent's time in the client, so that removing the task can
	// wait for the last writes even while a run is still dropping it.
	stores  map[*managedTask]*btStorage
	closed  bool
	policy  SeedPolicy
	changed chan struct{} // closed and replaced when the policy changes
}

// btLoaded is a torrent in the client and the task it belongs to.
type btLoaded struct {
	t  *torrent.Torrent
	mt *managedTask
}

func (e *btEngine) Policy() SeedPolicy {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.policy == "" {
		return SeedStop
	}
	return e.policy
}

func (e *btEngine) setPolicy(p SeedPolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policy = p
	if e.changed != nil {
		close(e.changed)
	}
	e.changed = make(chan struct{})
}

// watchPolicy returns the policy and a channel that closes when it changes, so
// a seeding torrent can stop the moment the user says so.
func (e *btEngine) watchPolicy() (SeedPolicy, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.changed == nil {
		e.changed = make(chan struct{})
	}
	p := e.policy
	if p == "" {
		p = SeedStop
	}
	return p, e.changed
}

// completion opens the piece database on first use. It needs no client, so a
// torrent can be removed and its records cleaned up without going online.
func (e *btEngine) completion() (storage.PieceCompletion, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("godm is shutting down")
	}
	if e.pc != nil {
		return e.pc, nil
	}
	if e.dir == "" {
		return nil, errors.New("torrent support is not set up")
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return nil, err
	}
	// Without cgo the library's default record is a bolt database. One for
	// all torrents, kept beside their metainfo, rather than one dropped into
	// every download folder.
	pc, err := storage.NewBoltPieceCompletion(e.dir)
	if err != nil {
		return nil, fmt.Errorf("opening the torrent piece database: %w", err)
	}
	e.pc = pc
	return pc, nil
}

// start returns the client, starting it on first use.
func (e *btEngine) start() (*torrent.Client, storage.PieceCompletion, error) {
	pc, err := e.completion()
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, nil, errors.New("godm is shutting down")
	}
	if e.cl != nil {
		return e.cl, pc, nil
	}
	e.slog = slog.New(quietHashes{slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{Level: slog.LevelWarn})})
	// With part files off, the storage still tries to flush a part file that
	// does not exist each time a file completes, and warns about it.
	e.fslog = slog.New(slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{Level: slog.LevelError}))
	cl, err := torrent.NewClient(e.config(btListenPort))
	if err != nil {
		// Something else holds the usual port. Any port works; it is only less
		// predictable for someone forwarding one by hand.
		var again error
		if cl, again = torrent.NewClient(e.config(0)); again != nil {
			return nil, nil, fmt.Errorf("starting the torrent client: %w", err)
		}
	}
	e.cl = cl
	e.loaded = map[metainfo.Hash]*btLoaded{}
	log.Printf("torrent client listening on %v", cl.ListenAddrs())
	return cl, pc, nil
}

func (e *btEngine) config(port int) *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = port
	// Opening a port in the user's router is not ours to do. Outgoing
	// connections reach the same peers; incoming ones need a manual forward.
	cfg.NoDefaultPortForwarding = true
	// Whether a finished torrent uploads is the seeding policy's decision,
	// carried out by dropping the torrent, so the client itself may.
	cfg.Seed = true
	// Without cgo only the pure-Go uTP is available, which costs a lot of CPU
	// for what it brings: every client that speaks uTP also speaks TCP.
	cfg.DisableUTP = true
	// WebTorrent peers are browsers reached over WebRTC through public STUN
	// servers: a lot of machinery and outside traffic for very few peers.
	cfg.DisableWebtorrent = true
	// Every torrent brings its own storage. Without this the client would open
	// a second piece database in the working directory.
	cfg.DefaultStorage = noStorage{}
	cfg.Slogger = e.slog
	if e.configure != nil {
		e.configure(cfg)
	}
	return cfg
}

// quietHashes drops the library's warning about a piece that hashed correctly.
// With plain file reads, its copy into the hasher stops at the end of the piece
// by failing with io.ErrShortWrite, which it then reports for every piece.
type quietHashes struct{ slog.Handler }

func (h quietHashes) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "finished hashing piece" {
		correct := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "correct" {
				correct = a.Value.Kind() == slog.KindBool && a.Value.Bool()
				return false
			}
			return true
		})
		if correct {
			return nil
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h quietHashes) WithAttrs(as []slog.Attr) slog.Handler {
	return quietHashes{h.Handler.WithAttrs(as)}
}

func (h quietHashes) WithGroup(name string) slog.Handler {
	return quietHashes{h.Handler.WithGroup(name)}
}

// noStorage refuses a torrent that was not added for a task, so nothing can
// be written outside a download folder by accident.
type noStorage struct{}

func (noStorage) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{}, errors.New("a torrent was added without a task")
}

// add puts a task's torrent into the client. A torrent can be there only once,
// so a second task for the same one is refused rather than allowed to fight
// over the files.
func (e *btEngine) add(mt *managedTask, spec *torrent.TorrentSpec) (*torrent.Torrent, *btStorage, error) {
	cl, pc, err := e.start()
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := &btStorage{mt: mt, pc: pc, slog: e.fslog, failed: make(chan error, 1)}
	spec.Storage = st
	if e.cl != cl {
		return nil, nil, errors.New("godm is shutting down")
	}
	// Remove takes the torrent out under this lock, so checking here means a
	// removal never races a run that is about to add it back.
	if mt.isRemoved() {
		return nil, nil, context.Canceled
	}
	if l := e.loaded[spec.InfoHash]; l != nil {
		if l.mt != mt && !l.mt.isRemoved() {
			return nil, nil, errors.New("this torrent is already in the list")
		}
		l.t.Drop()
		delete(e.loaded, spec.InfoHash)
	}
	if e.stores == nil {
		e.stores = map[*managedTask]*btStorage{}
	}
	e.stores[mt] = st
	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, nil, err
	}
	e.loaded[spec.InfoHash] = &btLoaded{t: t, mt: mt}
	return t, st, nil
}

// forget takes a removed task's torrent out of the client and returns once
// nothing can change its files any more, with its info if it had any. Drop
// alone does not promise that when the run is dropping the torrent at the
// same moment: the second caller finds it gone and does not wait.
func (e *btEngine) forget(mt *managedTask) *metainfo.Info {
	info := e.drop(mt)
	e.mu.Lock()
	st := e.stores[mt]
	delete(e.stores, mt)
	e.mu.Unlock()
	if st != nil {
		if i := st.stop(); i != nil {
			info = i
		}
	}
	return info
}

// drop takes a task's torrent out of the client, closing its connections and
// files, and returns its info if it had any.
func (e *btEngine) drop(mt *managedTask) *metainfo.Info {
	e.mu.Lock()
	var t *torrent.Torrent
	for ih, l := range e.loaded {
		if l.mt == mt {
			t = l.t
			delete(e.loaded, ih)
		}
	}
	e.mu.Unlock()
	if t == nil {
		return nil
	}
	info := t.Info()
	t.Drop()
	return info
}

func (e *btEngine) loadedFor(mt *managedTask) *torrent.Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.loaded {
		if l.mt == mt {
			return l.t
		}
	}
	return nil
}

func (e *btEngine) client() *torrent.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cl
}

func (e *btEngine) close() {
	e.mu.Lock()
	cl, pc := e.cl, e.pc
	e.cl, e.pc, e.loaded, e.stores, e.closed = nil, nil, nil, nil, true
	e.mu.Unlock()
	if cl != nil {
		cl.Close()
	}
	if pc != nil {
		pc.Close()
	}
}

// shutdownBT closes the torrent client and the piece database. Torrents still
// running stop where they are; every finished piece is already recorded.
func (m *Manager) shutdownBT() { m.bt.close() }

func (m *Manager) SeedPolicy() SeedPolicy { return m.bt.Policy() }

func (m *Manager) SetSeedPolicy(p SeedPolicy) {
	m.bt.setPolicy(p)
	m.dirty.Store(true)
}

// ---------- metainfo kept in the config folder ----------

func (e *btEngine) metainfoPath(ih metainfo.Hash) string {
	return filepath.Join(e.dir, ih.HexString()+".torrent")
}

func (e *btEngine) saveMetainfo(ih metainfo.Hash, mi *metainfo.MetaInfo) error {
	if e.dir == "" {
		return errors.New("torrent support is not set up")
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return err
	}
	p := e.metainfoPath(ih)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (e *btEngine) loadMetainfo(ih metainfo.Hash) (*metainfo.MetaInfo, error) {
	if e.dir == "" {
		return nil, os.ErrNotExist
	}
	return metainfo.LoadFromFile(e.metainfoPath(ih))
}

// clearCompletion forgets which pieces of a removed torrent were finished, so a
// later download of the same torrent starts from what is really on disk.
func (e *btEngine) clearCompletion(ih metainfo.Hash, info *metainfo.Info) {
	if info == nil {
		return
	}
	pc, err := e.completion()
	if err != nil {
		return
	}
	for i := 0; i < info.NumPieces(); i++ {
		pc.Set(metainfo.PieceKey{InfoHash: ih, Index: i}, false)
	}
}

// parseTorrentFile checks that data is a usable .torrent.
func parseTorrentFile(data []byte) (*metainfo.MetaInfo, metainfo.Hash, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, metainfo.Hash{}, errors.New("that is not a .torrent file")
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, metainfo.Hash{}, fmt.Errorf("the torrent's file list is damaged: %w", err)
	}
	if !info.HasV1() {
		return nil, metainfo.Hash{}, errors.New("BitTorrent v2-only torrents are not supported")
	}
	return mi, mi.HashInfoBytes(), nil
}

// magnetFor is the link a task made from a .torrent is known by. It names the
// torrent exactly, and the metainfo itself waits in the config folder.
func magnetFor(mi *metainfo.MetaInfo) (string, error) {
	m, err := mi.MagnetV2()
	if err != nil {
		return "", err
	}
	if !m.InfoHash.Ok {
		return "", errors.New("BitTorrent v2-only torrents are not supported")
	}
	return m.String(), nil
}

// AddTorrentFile queues the torrent a .torrent file describes. A copy is kept
// in the config folder, so the task does not depend on wherever the user
// picked the file from.
func (m *Manager) AddTorrentFile(data []byte, outDir string) (string, error) {
	mi, ih, err := parseTorrentFile(data)
	if err != nil {
		return "", err
	}
	link, err := magnetFor(mi)
	if err != nil {
		return "", err
	}
	if err := m.bt.saveMetainfo(ih, mi); err != nil {
		return "", err
	}
	return m.Add(jobRequest{URL: link, Kind: "bt", OutDir: outDir})
}

// fetchTorrentFile downloads a .torrent from the web and keeps it, returning
// the magnet link the task goes by from then on.
func (m *Manager) fetchTorrentFile(ctx context.Context, req jobRequest) (string, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := newClient().Do(hreq)
	if err != nil {
		return "", fmt.Errorf("fetching the torrent file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("fetching the torrent file: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFile+1))
	if err != nil {
		return "", fmt.Errorf("fetching the torrent file: %w", err)
	}
	if len(data) > maxTorrentFile {
		return "", errors.New("that link is far too large to be a .torrent file")
	}
	mi, ih, err := parseTorrentFile(data)
	if err != nil {
		return "", err
	}
	link, err := magnetFor(mi)
	if err != nil {
		return "", err
	}
	if err := m.bt.saveMetainfo(ih, mi); err != nil {
		return "", err
	}
	return link, nil
}

// torrentSpec works out what to hand the client for a task. A magnet link is
// enough on its own, but once a torrent's metainfo has been seen it is kept: a
// restart then neither waits for peers to send it again nor loses track of
// where the files are.
func (m *Manager) torrentSpec(ctx context.Context, mt *managedTask) (*torrent.TorrentSpec, error) {
	mt.mu.Lock()
	req := mt.req
	mt.mu.Unlock()
	if !isMagnet(req.URL) {
		link, err := m.fetchTorrentFile(ctx, req)
		if err != nil {
			return nil, err
		}
		// The list keeps showing the link the user gave; the job remembers
		// the torrent, so a restart does not need that link again.
		mt.mu.Lock()
		mt.req.URL = link
		mt.mu.Unlock()
		m.dirty.Store(true)
		req.URL = link
	}
	spec, err := torrent.TorrentSpecFromMagnetUri(req.URL)
	if err != nil {
		return nil, fmt.Errorf("not a usable magnet link: %w", err)
	}
	if spec.InfoHash == (metainfo.Hash{}) {
		return nil, errors.New("this magnet link has no BitTorrent v1 info hash (urn:btih)")
	}
	if m.torrentOwner(spec.InfoHash, mt) != nil {
		return nil, errors.New("this torrent is already in the list")
	}
	if mi, err := m.bt.loadMetainfo(spec.InfoHash); err == nil {
		if saved, err := torrent.TorrentSpecFromMetaInfoErr(mi); err == nil && saved.InfoHash == spec.InfoHash {
			spec.InfoBytes = saved.InfoBytes
			spec.PieceLayers = saved.PieceLayers
			spec.Trackers = append(spec.Trackers, saved.Trackers...)
			spec.Webseeds = append(spec.Webseeds, saved.Webseeds...)
			spec.DhtNodes = append(spec.DhtNodes, saved.DhtNodes...)
		}
	}
	return spec, nil
}

// ---------- where a torrent's files go ----------

// btStorage writes one task's torrent into its download folder, under the name
// the task claimed, and records finished pieces in the shared database.
type btStorage struct {
	mt     *managedTask
	pc     storage.PieceCompletion
	slog   *slog.Logger
	failed chan error // the files could not be placed; the run reports it

	// gate is held shared by every change to the files or the piece records,
	// and exclusively by stop. The client writes chunks with its own lock let
	// go, and dropping a torrent does not wait for those writes, so without
	// this a removal could delete a file that a write then recreates, or that
	// a write still holds open, which Windows refuses to delete.
	gate    sync.RWMutex
	stopped bool
	info    *metainfo.Info // once the storage is open
}

// errStorageStopped is what a write gets once its torrent has let go.
var errStorageStopped = errors.New("the torrent has stopped writing")

// stop waits for the changes in progress and refuses any after them. It
// returns the torrent's info if the storage was ever opened.
func (s *btStorage) stop() *metainfo.Info {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.stopped = true
	return s.info
}

// enter admits one change; leave with s.gate.RUnlock.
func (s *btStorage) enter() bool {
	s.gate.RLock()
	if s.stopped {
		s.gate.RUnlock()
		return false
	}
	return true
}

func (s *btStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	if !s.enter() {
		return storage.TorrentImpl{}, errStorageStopped
	}
	defer s.gate.RUnlock()
	root, err := s.mt.claimTorrentRoot(info, ih)
	if err != nil {
		select {
		case s.failed <- err:
		default:
		}
		return storage.TorrentImpl{}, err
	}
	name := filepath.Base(root)
	files := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: filepath.Dir(root),
		FilePathMaker: func(o storage.FilePathMakerOpts) string {
			return filepath.Join(append([]string{name}, torrentRelPath(o.File)...)...)
		},
		PieceCompletion: s.pc,
		// With part files the library renames each file once it is whole and,
		// at startup, judges completion from the names alone: every finished
		// piece of a file not yet whole is forgotten and fetched again. Writing
		// in place leaves that record to the database, which keeps it.
		UsePartFiles: g.Some(false),
		Logger:       s.slog,
	})
	impl, err := files.OpenTorrent(ctx, info, ih)
	if err != nil {
		return impl, err
	}
	s.info = info // stop reads it only once this has let go of the gate
	out := impl
	out.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return btPiece{PieceImpl: impl.Piece(p), s: s, length: p.Length()}
	}
	if impl.PieceWithHash != nil {
		out.PieceWithHash = func(p metainfo.Piece, h g.Option[[]byte]) storage.PieceImpl {
			return btPiece{PieceImpl: impl.PieceWithHash(p, h), s: s, length: p.Length()}
		}
	}
	// The client closes the storage once it has dropped the torrent and the
	// last piece check is over. Waiting here for the writes makes the drop
	// wait for them too, so a paused torrent has let go of its files.
	out.Close = func() error {
		s.stop()
		if impl.Close != nil {
			return impl.Close()
		}
		return nil
	}
	return out, nil
}

// btPiece passes one piece through to the file storage, but only while its
// torrent's storage has not stopped.
type btPiece struct {
	storage.PieceImpl
	s      *btStorage
	length int64
}

func (p btPiece) WriteAt(b []byte, off int64) (int, error) {
	if !p.s.enter() {
		return 0, errStorageStopped
	}
	defer p.s.gate.RUnlock()
	return p.PieceImpl.WriteAt(b, off)
}

func (p btPiece) MarkComplete() error {
	if !p.s.enter() {
		return errStorageStopped
	}
	defer p.s.gate.RUnlock()
	return p.PieceImpl.MarkComplete()
}

func (p btPiece) MarkNotComplete() error {
	if !p.s.enter() {
		return errStorageStopped
	}
	defer p.s.gate.RUnlock()
	return p.PieceImpl.MarkNotComplete()
}

// WriteTo keeps the file storage's own way of reading a whole piece, which is
// what checking it uses, rather than one ReadAt after another.
func (p btPiece) WriteTo(w io.Writer) (int64, error) {
	if wt, ok := p.PieceImpl.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	return io.Copy(w, io.NewSectionReader(p.PieceImpl, 0, p.length))
}

// claimTorrentRoot decides where a torrent goes: one file, or one folder
// holding all of them, in the task's download folder. A name already taken
// gets a number, as for any other download, because godm never writes into a
// file it did not create. The name is claimed on disk at once: a paused
// torrent may not write anything for days, and nothing else may take it
// meanwhile.
func (t *managedTask) claimTorrentRoot(info *metainfo.Info, ih metainfo.Hash) (string, error) {
	t.mu.Lock()
	if p := t.view.Path; p != "" {
		t.mu.Unlock()
		return p, nil
	}
	dir := t.outDir
	t.mu.Unlock()

	name := sanitize(info.BestName())
	if name == "" || name == metainfo.NoName {
		name = "torrent " + ih.HexString()[:12]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for n := 0; n < 10000; n++ {
		cand := filepath.Join(dir, name)
		if n > 0 && info.IsDir() {
			cand = fmt.Sprintf("%s (%d)", cand, n)
		} else if n > 0 {
			cand = numberedName(cand, n)
		}
		if pathExists(cand) {
			continue
		}
		release, ok := reserveTarget(cand)
		if !ok {
			continue
		}
		var err error
		if info.IsDir() {
			err = os.Mkdir(cand, 0o755)
		} else {
			var f *os.File
			if f, err = os.OpenFile(cand, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
				err = f.Close()
			}
		}
		release()
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		t.mu.Lock()
		t.view.Path, t.view.Filename = cand, filepath.Base(cand)
		t.mu.Unlock()
		return cand, nil
	}
	return "", fmt.Errorf("no free name for %s in %s", name, dir)
}

// torrentRelPath is where a file sits inside the torrent's folder, made safe
// for NTFS one component at a time: the names come from strangers, and ".."
// must never climb out.
func torrentRelPath(fi *metainfo.FileInfo) []string {
	parts := fi.BestPath()
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := sanitize(p)
		if s == "" {
			s = "_"
		}
		out = append(out, s)
	}
	return out
}

// mediaRank says how good a file is as the thing Play opens.
func mediaRank(name string) int {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".m4v", ".ts", ".m2ts", ".flv", ".wmv", ".mpg", ".mpeg", ".ogv":
		return 2
	case ".mp3", ".flac", ".wav", ".aac", ".m4a", ".ogg", ".opus":
		return 1
	}
	return 0
}

// torrentMediaIndex picks what Play opens: the largest video, or failing that
// the largest audio file. A film travels with samples, subtitles and artwork,
// and the biggest file is the film.
func torrentMediaIndex(info *metainfo.Info) (int, bool) {
	best, bestRank, bestLen := -1, 0, int64(-1)
	for i, fi := range info.UpvertedFiles() {
		if strings.Contains(fi.Attr, "p") {
			continue // BEP 47 padding
		}
		name := info.BestName()
		if p := fi.BestPath(); len(p) > 0 {
			name = p[len(p)-1]
		}
		r := mediaRank(name)
		if r == 0 {
			continue
		}
		if r > bestRank || (r == bestRank && fi.Length > bestLen) {
			best, bestRank, bestLen = i, r, fi.Length
		}
	}
	return best, best >= 0
}

// torrentDetails fills in what is known once the metainfo has arrived.
func (t *managedTask) torrentDetails(info *metainfo.Info, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.view.Path
	if root == "" {
		return
	}
	t.view.Filename = filepath.Base(root)
	t.view.Size = size
	t.view.Media = ""
	if i, ok := torrentMediaIndex(info); ok {
		files := info.UpvertedFiles()
		t.view.Media = path.Join(append([]string{filepath.Base(root)}, torrentRelPath(&files[i])...)...)
	}
}

// torrentMediaPath is where a finished torrent's video is on disk.
func torrentMediaPath(v TaskView) (string, error) {
	if v.Path == "" || v.Media == "" {
		return "", errors.New("this torrent has no video or audio file to play")
	}
	p := filepath.Join(filepath.Dir(v.Path), filepath.FromSlash(v.Media))
	if p != v.Path && !strings.HasPrefix(p, v.Path+string(filepath.Separator)) {
		return "", errors.New("the file to play is outside the download")
	}
	return p, nil
}

// btDeleteFor is how long deleting a removed torrent's files keeps trying.
// Windows will not delete a file that something has open, and a player or a
// virus scanner may hold one for a moment after the torrent let go of it.
const btDeleteFor = 15 * time.Second

// deleteTorrentFiles removes what a torrent wrote, and the folders it made once
// they are empty. Only those files: someone may have put their own in there.
// What will not go is tried again for a while, then named in the log.
func deleteTorrentFiles(root string, info *metainfo.Info) {
	var files, dirs []string
	if info == nil {
		// Without the file list all that can go is the claimed name itself:
		// a file, or a folder that is empty if it is ours alone.
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			dirs = []string{root}
		} else {
			files = []string{root}
		}
	} else if !info.IsDir() {
		files = []string{root}
	} else {
		seen := map[string]bool{}
		for _, fi := range info.UpvertedFiles() {
			p := filepath.Join(append([]string{root}, torrentRelPath(&fi)...)...)
			files = append(files, p)
			for d := filepath.Dir(p); len(d) > len(root); d = filepath.Dir(d) {
				if !seen[d] {
					seen[d] = true
					dirs = append(dirs, d)
				}
			}
		}
		// Deepest first, so each folder is empty by the time it is reached.
		sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
		dirs = append(dirs, root)
	}
	deadline := time.Now().Add(btDeleteFor)
	for wait := 50 * time.Millisecond; ; wait = min(2*wait, time.Second) {
		left := removeTorrentPaths(files, dirs)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("removing a torrent's files: could not delete %s", strings.Join(left, "; "))
			return
		}
		time.Sleep(wait)
	}
}

// removeTorrentPaths deletes files, then dirs in order, and reports what is
// still there and should not be. A folder that is not empty is no failure of
// its own: either a file of ours in it is already reported, or what it holds
// is someone else's.
func removeTorrentPaths(files, dirs []string) (left []string) {
	for _, p := range files {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			left = append(left, err.Error())
		}
	}
	for _, d := range dirs {
		err := os.Remove(d)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if entries, rerr := os.ReadDir(d); rerr == nil && len(entries) > 0 {
			continue
		}
		left = append(left, err.Error())
	}
	return left
}

// forgetTorrent cleans up after a removed torrent task: the torrent leaves the
// client, its kept metainfo and piece records go, and with deleteFiles so do
// the files it wrote.
func (m *Manager) forgetTorrent(mt *managedTask, deleteFiles bool) {
	// Only once nothing can write to the files, or the delete could miss one.
	info := m.bt.forget(mt)
	mt.mu.Lock()
	link, root := mt.req.URL, mt.view.Path
	mt.mu.Unlock()
	// A task refused as a duplicate shares the info hash with the one that
	// holds the torrent, whose records must outlive it.
	if ih, ok := magnetInfoHash(link); ok && m.torrentOwner(ih, mt) == nil {
		if info == nil {
			if mi, err := m.bt.loadMetainfo(ih); err == nil {
				if i, err := mi.UnmarshalInfo(); err == nil {
					info = &i
				}
			}
		}
		m.bt.clearCompletion(ih, info)
		if m.bt.dir != "" {
			os.Remove(m.bt.metainfoPath(ih))
		}
	}
	if deleteFiles && root != "" {
		deleteTorrentFiles(root, info)
	}
}

// ---------- running a torrent ----------

// torrentStatus is one look at a running torrent.
type torrentStatus struct {
	stage      string
	have, size int64 // size stays zero until the metainfo arrives
	got        int64 // useful bytes received this run, which the speed follows
	uploaded   int64
	peers      int
	seeds      int
}

// torrentProgress shows a torrent in the task view. Peers stand in for
// connections: Active counts the peers connected and Conns the seeds among
// them. The speed follows the bytes received rather than the bytes completed,
// which step back whenever a piece fails its check.
func (t *managedTask) torrentProgress(gen int, s torrentStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	now := time.Now()
	if !t.lastAt.IsZero() {
		if dt := now.Sub(t.lastAt).Seconds(); dt > 0.05 {
			inst := float64(s.got-t.lastRecv) / dt
			if t.speedEMA == 0 {
				t.speedEMA = inst
			} else {
				t.speedEMA = 0.7*t.speedEMA + 0.3*inst
			}
			t.lastRecv, t.lastAt = s.got, now
		}
	} else {
		t.lastRecv, t.lastAt = s.got, now
	}
	t.view.Kind = "bt"
	t.view.Stage = s.stage
	t.view.Resumable = true // a torrent always picks up where it stopped
	if s.size > 0 {
		t.view.Size, t.view.Received = s.size, s.have
	}
	t.view.Speed = int64(t.speedEMA)
	t.view.Active, t.view.Conns = s.peers, s.seeds
	t.view.Uploaded = s.uploaded
	t.view.Segments = nil
}

// torrentStage shows what a torrent does outside a download: seeding once it
// has finished, or nothing once it has let go.
func (t *managedTask) torrentStage(gen int, stage string, uploaded int64, peers int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.view.Stage, t.view.Uploaded = stage, uploaded
	if t.view.State == StateDone {
		t.view.Active = peers
	}
}

func stopped(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("the torrent was stopped")
}

// runTorrent is one run of a torrent task, from the queue slot to the last
// piece. Pausing drops the torrent from the client, which closes its files and
// peers; what was finished stays recorded for the next run.
func (m *Manager) runTorrent(ctx context.Context, mt *managedTask, gen int) (string, error) {
	spec, err := m.torrentSpec(ctx, mt)
	if err != nil {
		return "", err
	}
	tor, st, err := m.bt.add(mt, spec)
	if err != nil {
		return "", err
	}
	finished := false
	defer func() {
		if !finished {
			m.bt.drop(mt)
			mt.torrentStage(gen, "", mt.snapshot().Uploaded, 0)
		}
	}()

	mt.mu.Lock()
	base := mt.view.Uploaded // uploads from earlier runs count towards the ratio
	mt.mu.Unlock()
	sample := func(stage string) torrentStatus {
		s := tor.Stats()
		ts := torrentStatus{
			stage:    stage,
			got:      s.BytesReadUsefulData.Int64(),
			uploaded: base + s.BytesWrittenData.Int64(),
			peers:    s.ActivePeers,
			seeds:    s.ConnectedSeeders,
		}
		if tor.Info() != nil {
			ts.have, ts.size = tor.BytesCompleted(), tor.Length()
		}
		return ts
	}
	report := func(stage string) {
		mt.torrentProgress(gen, sample(stage))
		m.dirty.Store(true)
	}

	tick := time.NewTicker(btTick)
	defer tick.Stop()
	// A magnet link names the torrent but not its files: those come from the
	// first peers that answer.
	for tor.Info() == nil {
		report("metadata")
		select {
		case <-tor.GotInfo():
		case err := <-st.failed:
			return "", err
		case <-tor.Closed():
			return "", stopped(ctx)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
	info := tor.Info()
	if _, err := m.bt.loadMetainfo(tor.InfoHash()); err != nil && !mt.isRemoved() {
		mi := tor.Metainfo()
		if err := m.bt.saveMetainfo(tor.InfoHash(), &mi); err != nil {
			log.Printf("keeping the metainfo of %s: %v", tor.Name(), err)
		}
	}
	mt.torrentDetails(info, tor.Length())

	writeErr := make(chan error, 1)
	tor.SetOnWriteChunkError(func(err error) {
		select {
		case writeErr <- err:
		default:
		}
	})
	tor.DownloadAll()
	for !tor.Complete().Bool() {
		report("")
		select {
		case <-tor.Complete().On():
		case err := <-writeErr:
			return "", fmt.Errorf("could not write the download to disk: %w", err)
		case <-tor.Closed():
			return "", stopped(ctx)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
	report("")
	finished = true
	m.afterTorrentCompletes(mt, gen, tor, base)
	return mt.snapshot().Path, nil
}

// afterTorrentCompletes applies the seeding policy to a torrent that has just
// finished downloading.
func (m *Manager) afterTorrentCompletes(mt *managedTask, gen int, tor *torrent.Torrent, base int64) {
	if m.bt.Policy() == SeedStop {
		m.bt.drop(mt)
		return
	}
	s := tor.Stats()
	mt.torrentStage(gen, "seeding", base+s.BytesWrittenData.Int64(), 0)
	go m.seedTorrent(mt, gen, tor, base)
}

// seedTorrent keeps a finished torrent uploading for as long as the policy
// wants, and lets it go the moment it does not, including when the user
// changes the policy while it runs. Seeding lasts as long as the daemon: after
// a restart a finished torrent stays finished, because nothing in godm starts
// moving data again without the user asking.
func (m *Manager) seedTorrent(mt *managedTask, gen int, tor *torrent.Torrent, base int64) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := int64(-1)
	for {
		s := tor.Stats()
		up := base + s.BytesWrittenData.Int64()
		policy, changed := m.bt.watchPolicy()
		if !wantSeed(policy, up, tor.Length()) {
			m.bt.drop(mt)
			mt.torrentStage(gen, "", up, 0)
			m.dirty.Store(true)
			return
		}
		mt.torrentStage(gen, "seeding", up, s.ActivePeers)
		if up != last {
			last = up
			m.dirty.Store(true)
		}
		select {
		case <-tor.Closed():
			mt.torrentStage(gen, "", up, 0)
			m.dirty.Store(true)
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

// ---------- HTTP ----------

// handleTorrentFile takes a .torrent the user picked, sent as the raw request
// body. out_dir in the query overrides the download folder.
func (s *server) handleTorrentFile(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTorrentFile))
	if err != nil {
		http.Error(w, "that file is too large to be a .torrent", http.StatusRequestEntityTooLarge)
		return
	}
	id, err := s.mgr.AddTorrentFile(data, r.URL.Query().Get("out_dir"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

// handleSeeding reads or sets what a torrent does once it has finished:
//
//	GET  /api/seeding               {"ok":true,"policy":"stop","choices":[...]}
//	POST /api/seeding?policy=ratio  the same, after the change
//
// POST also takes {"policy":"ratio"} as a JSON body. "stop", the default,
// stops uploading the moment the download completes; "ratio" uploads until
// as much has gone out as came in; "keep" uploads until the task is removed
// or godm quits.
func (s *server) handleSeeding(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		v := r.URL.Query().Get("policy")
		if v == "" {
			var body struct {
				Policy string `json:"policy"`
			}
			json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
			v = body.Policy
		}
		p, ok := parseSeedPolicy(v)
		if !ok {
			http.Error(w, "policy must be stop, ratio or keep", http.StatusBadRequest)
			return
		}
		s.mgr.SetSeedPolicy(p)
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "policy": s.mgr.SeedPolicy(), "choices": seedChoices})
}

// waitTorrentMedia waits for a torrent that was just started to be in the
// client with its metainfo, which a player needs before the first byte.
func (m *Manager) waitTorrentMedia(ctx context.Context, mt *managedTask) (*torrent.File, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		if tor := m.bt.loadedFor(mt); tor != nil && tor.Info() != nil {
			i, ok := torrentMediaIndex(tor.Info())
			if !ok {
				return nil, errors.New("this torrent has no video or audio file to play")
			}
			return tor.Files()[i], nil
		}
		switch {
		case !mt.stillArriving():
			return nil, errors.New("the download is not running")
		case time.Now().After(deadline):
			return nil, errors.New("the torrent did not start in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// streamTorrent serves a torrent's video to a player. A finished one is read
// from disk. One still arriving is read through the torrent itself: the pieces
// just ahead of the reader move to the front of the queue, and in responsive
// mode each chunk is handed over as soon as it lands rather than once its
// whole piece has been checked. Ranges work as for any file.
func (s *server) streamTorrent(w http.ResponseWriter, r *http.Request, mt *managedTask) {
	if v := mt.snapshot(); v.State == StateDone {
		p, err := torrentMediaPath(v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
		return
	}
	file, err := s.mgr.waitTorrentMedia(r.Context(), mt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	rd := file.NewReader()
	defer rd.Close()
	rd.SetContext(r.Context())
	rd.SetResponsive()
	rd.SetReadahead(torrentReadahead)
	name := path.Base(file.DisplayPath())
	h := w.Header()
	// Set before ServeContent, which would otherwise read the start of the
	// file to guess, and so wait for a piece the player may not want at all.
	h.Set("Content-Type", contentTypeFor(name))
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, time.Time{}, rd)
}

// playTorrent opens a torrent's video in a player: straight from disk once it
// is finished, through the stream endpoint while it is still arriving.
func (s *server) playTorrent(w http.ResponseWriter, id string, mt *managedTask) {
	v := mt.snapshot()
	if v.Media == "" {
		msg := "this torrent has no video or audio file to play"
		if v.State != StateDone && v.Size <= 0 {
			msg = "the torrent's file list has not arrived yet; try again in a moment"
		}
		writeJSON(w, map[string]any{"ok": false, "error": msg})
		return
	}
	name := path.Base(v.Media)
	target := ""
	if v.State == StateDone {
		p, err := torrentMediaPath(v)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		target = p
	} else {
		if v.State == StatePaused || v.State == StateError {
			s.mgr.Resume(id)
		}
		target = fmt.Sprintf("http://127.0.0.1:%d/stream/%s/%s?token=%s",
			s.port, url.PathEscape(id), url.PathEscape(name), url.QueryEscape(s.token))
	}
	if err := launchPlayer(target, name); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
