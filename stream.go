package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Playback lets a player read a file while it is still downloading. The
// engine attaches the running transfer; a reader asks how much is on disk from
// a given byte and says where it is reading, and while it reads, the
// connections gather just ahead of it.
//
// The zero value is ready to use and outlives any one run of the download.
type Playback struct {
	mu      sync.Mutex
	t       *task
	readers map[uint64]int64 // open requests and the byte each wants next
	seq     uint64

	// focus is 1 + the byte the newest reader wants, or 0 when nobody reads.
	// The engine reads it on every claim, so it is kept outside the mutex.
	focus atomic.Int64
}

func (p *Playback) attach(t *task) {
	p.mu.Lock()
	p.t = t
	p.mu.Unlock()
}

func (p *Playback) detach(t *task) {
	p.mu.Lock()
	if p.t == t {
		p.t = nil
	}
	p.mu.Unlock()
}

func (p *Playback) focusAt() (int64, bool) {
	v := p.focus.Load()
	return v - 1, v > 0
}

// Available reports how many bytes from off are on disk in one unbroken run.
// ok is false when no transfer is running, so nothing is known.
func (p *Playback) Available(off int64) (n int64, ok bool) {
	p.mu.Lock()
	t := p.t
	p.mu.Unlock()
	if t == nil {
		return 0, false
	}
	t.segMu.Lock()
	n = t.firstMissingLocked(off) - off
	t.segMu.Unlock()
	if n < 0 {
		n = 0
	}
	return n, true
}

// PlaybackReader is one open request from a player.
type PlaybackReader struct {
	p  *Playback
	id uint64
}

func (p *Playback) NewReader() *PlaybackReader {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readers == nil {
		p.readers = map[uint64]int64{}
	}
	p.seq++
	return &PlaybackReader{p: p, id: p.seq}
}

// Want says where this reader is about to read.
func (r *PlaybackReader) Want(off int64) {
	r.p.mu.Lock()
	r.p.readers[r.id] = off
	r.p.refocusLocked()
	r.p.mu.Unlock()
}

func (r *PlaybackReader) Close() {
	r.p.mu.Lock()
	delete(r.p.readers, r.id)
	r.p.refocusLocked()
	r.p.mu.Unlock()
}

// refocusLocked hands the connections to the newest reader. A player that
// seeks opens a new request, and whatever the old one still wants is stale.
func (p *Playback) refocusLocked() {
	var newest uint64
	var at int64
	for id, off := range p.readers {
		if id > newest {
			newest, at = id, off
		}
	}
	if newest == 0 {
		p.focus.Store(0)
		return
	}
	p.focus.Store(at + 1)
}

// ---------- serving a download to a player ----------

const (
	// streamGrace is how long a request waits on bytes from a download that
	// has stopped, in case it is about to start again.
	streamGrace = 5 * time.Second
	// streamStall gives up on a running download that has not delivered the
	// bytes a player is waiting for in this long.
	streamStall = 2 * time.Minute
)

// available is how many bytes from off can be read now.
func (t *managedTask) available(off int64) int64 {
	if n, ok := t.play.Available(off); ok {
		return n
	}
	v := t.snapshot()
	if v.State == StateDone && v.Size > off {
		return v.Size - off
	}
	return 0
}

// playsWhileArriving reports whether a player can read the file before it
// is finished. A stream playlist or a yt-dlp job only becomes one playable
// file at the end. The request says which engine runs the job from the
// start; the view only says once that engine has begun.
func (t *managedTask) playsWhileArriving() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !isHLSJob(t.req) && !isYTDLPJob(t.req)
}

// errNotWhileArriving is why such a download cannot be played yet.
var errNotWhileArriving = errors.New("this kind of download can only be played once it finishes")

func (t *managedTask) stillArriving() bool {
	switch t.snapshot().State {
	case StateQueued, StateRunning:
		return true
	}
	return false
}

// waitStreamable waits for a download that was just started to learn where
// its file is and how big it is, which a player needs before the first byte.
func waitStreamable(ctx context.Context, mt *managedTask) (TaskView, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		v := mt.snapshot()
		switch {
		case v.State == StateDone && v.Path != "":
			return v, nil
		case !mt.playsWhileArriving():
			return v, errNotWhileArriving
		case v.Path != "" && v.Size > 0:
			return v, nil
		case !mt.stillArriving():
			return v, errors.New("the download is not running")
		case time.Now().After(deadline):
			return v, errors.New("the download did not start in time")
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// handleStream serves a download to a media player while it is still
// arriving. Bytes already on disk go out at once; for the rest the request
// waits while the connections are pulled to where the player reads. The path
// is /stream/<id>/<name>; the name is only there for the player's title bar.
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/stream/"), "/")
	mt := s.mgr.get(id)
	if mt == nil {
		http.NotFound(w, r)
		return
	}
	if mt.isTorrent() {
		s.streamTorrent(w, r, mt)
		return
	}
	v, err := waitStreamable(r.Context(), mt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	f, err := os.Open(v.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	name := filepath.Base(v.Path)

	if v.State == StateDone {
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, name, fi.ModTime(), f)
		return
	}

	size := v.Size
	start, end, partial, ok := parseRangeHeader(r.Header.Get("Range"), size)
	h := w.Header()
	if !ok {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentTypeFor(name))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	h.Set("Cache-Control", "no-store")
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	streamRange(r.Context(), mt, f, w, start, end)
}

// streamRange copies bytes start..end to w as they land on disk.
func streamRange(ctx context.Context, mt *managedTask, f *os.File, w io.Writer, start, end int64) {
	rd := mt.play.NewReader()
	defer rd.Close()
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, readBufSize)
	pos := start
	var waitingSince time.Time
	for pos <= end {
		rd.Want(pos)
		n := mt.available(pos)
		if n <= 0 {
			now := time.Now()
			if waitingSince.IsZero() {
				waitingSince = now
			}
			waited := now.Sub(waitingSince)
			if waited > streamStall || (waited > streamGrace && !mt.stillArriving()) {
				return // the player sees the connection end and can retry
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		waitingSince = time.Time{}
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		if n > end-pos+1 {
			n = end - pos + 1
		}
		k, err := f.ReadAt(buf[:n], pos)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				return
			}
			pos += int64(k)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
	}
}

// parseRangeHeader understands the single ranges players send: "bytes=a-b",
// "bytes=a-" and the suffix form "bytes=-n". Anything else, or no header, is
// the whole file. ok is false only for a range that starts past the end.
func parseRangeHeader(header string, size int64) (start, end int64, partial, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, size - 1, false, true
	}
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, size - 1, false, true
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, size - 1, false, true
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 {
		return 0, size - 1, false, true
	}
	if start >= size {
		return 0, 0, false, false
	}
	end = size - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, size - 1, false, true
		}
		if e < end {
			end = e
		}
	}
	return start, end, true, true
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mkv":
		return "video/x-matroska"
	case ".ts", ".m2ts":
		return "video/mp2t"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// handlePlay opens a download in a media player. A finished file is handed
// over as it is; one still arriving is played through the stream endpoint,
// and started first if it is not running.
func (s *server) handlePlay(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	mt := s.mgr.get(id)
	if mt == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "no such task"})
		return
	}
	if mt.isTorrent() {
		s.playTorrent(w, id, mt)
		return
	}
	v := mt.snapshot()
	name := v.Filename
	if v.State == StateDone && v.Path != "" {
		if err := launchPlayer(v.Path, name); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if !mt.playsWhileArriving() {
		writeJSON(w, map[string]any{"ok": false, "error": errNotWhileArriving.Error()})
		return
	}
	// Nothing more arrives on an expired link, so a player opened now would
	// only wait for bytes that cannot come.
	switch v.State {
	case StateNeedsRefresh:
		writeJSON(w, map[string]any{"ok": false, "error": "the download link expired: refresh it so the download can continue, then play it"})
		return
	case StateAwaitingRefresh:
		writeJSON(w, map[string]any{"ok": false, "error": "the download is waiting for a fresh link: play it once it continues"})
		return
	case StatePaused, StateError:
		s.mgr.Resume(id)
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/stream/%s/%s?token=%s",
		s.port, url.PathEscape(id), url.PathEscape(name), url.QueryEscape(s.token))
	if err := launchPlayer(link, name); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// findPlayer looks for a player that can open an HTTP address and seek in it.
// mpv comes first: it copes best with a stream that pauses for data.
func findPlayer() (exe string, isMPV bool) {
	for _, name := range []string{"mpv", "vlc"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, name == "mpv"
		}
		for _, p := range playerPaths(name) {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, name == "mpv"
			}
		}
	}
	return "", false
}

// launchPlayer is a variable so tests can watch what would be opened.
var launchPlayer = func(target, title string) error {
	exe, isMPV := findPlayer()
	if exe == "" {
		return errors.New("no media player found: install mpv (recommended) or VLC")
	}
	var args []string
	if isMPV {
		args = []string{
			"--force-window=immediate",
			"--cache=yes",
			// Read well ahead: the connections fetch faster than a film plays,
			// and a deep buffer rides out a connection being moved.
			"--demuxer-max-bytes=512MiB",
			// A request can wait a while for bytes the download has not
			// reached yet; that is not a dead server.
			"--network-timeout=120",
			"--title=" + title,
			"--", target,
		}
	} else {
		args = []string{"--network-caching=10000", "--meta-title=" + title, target}
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
