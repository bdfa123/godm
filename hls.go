package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HLS streams are a playlist of small segments rather than one long response,
// so none of the byte-range machinery in engine.go applies: there is no total
// size to split and no Range to resume from. What they share is the HTTP
// plumbing — the client, the retry classification and the backoff.

// maxSegmentBytes caps a single segment read. A playlist is attacker-supplied
// input; without a cap one entry pointing at an endless response would eat all
// the memory we have.
const maxSegmentBytes = 256 << 20

// maxBufferedBytes bounds how far finished segments may pile up waiting for
// their turn to be written. Segments are appended in playlist order, so a slow
// early segment holds back everything behind it.
const maxBufferedBytes = 128 << 20

// DRMError reports a stream we deliberately will not touch. Plain HLS AES-128
// hands the key to any client that asks, which makes it transport encryption
// and every player's job to undo. A KEYFORMAT naming a DRM system means the
// key is deliberately withheld, and prising it out is not something godm does.
type DRMError struct{ Scheme string }

func (e *DRMError) Error() string {
	return "this stream is protected by DRM (" + e.Scheme + "), which godm does not decrypt"
}

// LiveStreamError reports a playlist with no end. Recording one is a different
// feature from downloading a file: it finishes when the user says so, not when
// the last segment arrives.
type LiveStreamError struct{}

func (e *LiveStreamError) Error() string {
	return "this is a live stream with no end marker; recording live streams is not supported yet"
}

type hlsKey struct {
	Method string
	URI    string
	IV     []byte // nil means derive it from the segment's sequence number
}

type hlsSegment struct {
	URI      string
	Duration float64
	Seq      int64
	Key      *hlsKey
	Offset   int64 // EXT-X-BYTERANGE, zero length means the whole resource
	Length   int64
}

type hlsMedia struct {
	TargetDuration float64
	TotalDuration  float64
	Segments       []hlsSegment
	Init           *hlsSegment // EXT-X-MAP, present for fragmented MP4
	Complete       bool        // saw EXT-X-ENDLIST
}

type hlsVariant struct {
	URI        string
	Bandwidth  int
	Resolution string
	Codecs     string
	Name       string
	AudioGroup string
}

// Label is what the user picks from: "1080p · 5.2 Mbps".
func (v hlsVariant) Label() string {
	var parts []string
	if v.Name != "" {
		parts = append(parts, v.Name)
	} else if h := v.Height(); h > 0 {
		parts = append(parts, strconv.Itoa(h)+"p")
	} else if v.Resolution != "" {
		parts = append(parts, v.Resolution)
	}
	if v.Bandwidth > 0 {
		parts = append(parts, fmt.Sprintf("%.1f Mbps", float64(v.Bandwidth)/1e6))
	}
	if len(parts) == 0 {
		return "stream"
	}
	return strings.Join(parts, " · ")
}

func (v hlsVariant) Height() int {
	if _, h, ok := strings.Cut(v.Resolution, "x"); ok {
		n, _ := strconv.Atoi(h)
		return n
	}
	return 0
}

type hlsMaster struct {
	Variants []hlsVariant
}

// best returns the highest-bandwidth variant, which is what a player settles on
// when the connection allows it.
func (m *hlsMaster) best() int {
	best, bw := 0, -1
	for i, v := range m.Variants {
		if v.Bandwidth > bw {
			best, bw = i, v.Bandwidth
		}
	}
	return best
}

// ---------- parsing ----------

// parseAttrs splits an EXT-X tag's attribute list. Splitting on commas is the
// obvious approach and it is wrong: CODECS="avc1.4d401f,mp4a.40.2" carries a
// comma inside its quotes.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for i := 0; i < len(s); {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.ToUpper(strings.TrimSpace(s[i : i+eq]))
		i += eq + 1
		var val string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				break
			}
			val = s[i+1 : i+1+end]
			i += end + 2
		} else if end := strings.IndexByte(s[i:], ','); end < 0 {
			val, i = s[i:], len(s)
		} else {
			val, i = s[i:i+end], i+end
		}
		attrs[key] = val
		for i < len(s) && (s[i] == ',' || s[i] == ' ') {
			i++
		}
	}
	return attrs
}

// parsePlaylist returns either a master playlist or a media playlist, telling
// them apart by the presence of EXT-X-STREAM-INF.
func parsePlaylist(body []byte, base *url.URL) (*hlsMaster, *hlsMedia, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(body, "\xef\xbb\xbf \t\r\n"), []byte("#EXTM3U")) {
		return nil, nil, errors.New("not an m3u8 playlist (no #EXTM3U header)")
	}
	if bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		m, err := parseMaster(body, base)
		return m, nil, err
	}
	m, err := parseMedia(body, base)
	return nil, m, err
}

func parseMaster(body []byte, base *url.URL) (*hlsMaster, error) {
	master := &hlsMaster{}
	var pending *hlsVariant
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			bw, _ := strconv.Atoi(a["BANDWIDTH"])
			if avg, err := strconv.Atoi(a["AVERAGE-BANDWIDTH"]); err == nil && bw == 0 {
				bw = avg
			}
			pending = &hlsVariant{
				Bandwidth:  bw,
				Resolution: a["RESOLUTION"],
				Codecs:     a["CODECS"],
				Name:       a["NAME"],
				AudioGroup: a["AUDIO"],
			}
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			if err := checkKeyFormat(parseAttrs(strings.TrimPrefix(line, "#EXT-X-SESSION-KEY:"))); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "#"):
		default:
			if pending == nil {
				continue
			}
			u, err := base.Parse(line)
			if err != nil {
				return nil, fmt.Errorf("bad variant URI %q: %w", line, err)
			}
			pending.URI = u.String()
			master.Variants = append(master.Variants, *pending)
			pending = nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(master.Variants) == 0 {
		return nil, errors.New("master playlist lists no streams")
	}
	return master, nil
}

func parseMedia(body []byte, base *url.URL) (*hlsMedia, error) {
	m := &hlsMedia{}
	var (
		key      *hlsKey
		dur      float64
		haveDur  bool
		rangeLen int64
		rangeOff int64
		haveRng  bool
		prevEnd  int64
		seq      int64
	)
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			m.TargetDuration, _ = strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64)
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			k, err := parseKey(strings.TrimPrefix(line, "#EXT-X-KEY:"), base)
			if err != nil {
				return nil, err
			}
			key = k
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			u, err := base.Parse(a["URI"])
			if err != nil {
				return nil, fmt.Errorf("bad EXT-X-MAP URI: %w", err)
			}
			init := hlsSegment{URI: u.String(), Key: key}
			if br := a["BYTERANGE"]; br != "" {
				init.Length, init.Offset, _ = parseByteRange(br, 0)
			}
			m.Init = &init
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			dur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			haveDur = true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			rangeLen, rangeOff, haveRng = parseByteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"), prevEnd)
		case line == "#EXT-X-ENDLIST":
			m.Complete = true
		case strings.HasPrefix(line, "#"):
		default:
			if !haveDur {
				continue // a URI with no EXTINF is not a segment
			}
			u, err := base.Parse(line)
			if err != nil {
				return nil, fmt.Errorf("bad segment URI %q: %w", line, err)
			}
			s := hlsSegment{URI: u.String(), Duration: dur, Seq: seq, Key: key}
			if haveRng {
				s.Offset, s.Length = rangeOff, rangeLen
				prevEnd = rangeOff + rangeLen
			}
			m.Segments = append(m.Segments, s)
			m.TotalDuration += dur
			seq++
			haveDur, haveRng = false, false
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(m.Segments) == 0 {
		return nil, errors.New("playlist lists no segments")
	}
	return m, nil
}

// parseByteRange reads "length[@offset]". Without an offset the range starts
// where the previous one ended, which is how a playlist walks through one file.
func parseByteRange(s string, prevEnd int64) (length, offset int64, ok bool) {
	s = strings.TrimSpace(s)
	lenStr, offStr, hasOff := strings.Cut(s, "@")
	length, err := strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil || length <= 0 {
		return 0, 0, false
	}
	if hasOff {
		offset, err = strconv.ParseInt(strings.TrimSpace(offStr), 10, 64)
		if err != nil || offset < 0 {
			return 0, 0, false
		}
	} else {
		offset = prevEnd
	}
	return length, offset, true
}

func parseKey(attrs string, base *url.URL) (*hlsKey, error) {
	a := parseAttrs(attrs)
	method := strings.ToUpper(a["METHOD"])
	if method == "NONE" || method == "" {
		return nil, nil
	}
	if err := checkKeyFormat(a); err != nil {
		return nil, err
	}
	if method != "AES-128" {
		// SAMPLE-AES and SAMPLE-AES-CTR only ever appear with a DRM system
		// holding the key.
		return nil, &DRMError{Scheme: method}
	}
	u, err := base.Parse(a["URI"])
	if err != nil {
		return nil, fmt.Errorf("bad key URI: %w", err)
	}
	k := &hlsKey{Method: method, URI: u.String()}
	if iv := a["IV"]; iv != "" {
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X"))
		if err != nil || len(raw) != aes.BlockSize {
			return nil, fmt.Errorf("bad IV %q", iv)
		}
		k.IV = raw
	}
	return k, nil
}

// checkKeyFormat rejects anything but the plain "identity" key format. A
// KEYFORMAT naming Widevine, PlayReady or FairPlay is the stream saying the key
// is not ours to have.
func checkKeyFormat(a map[string]string) error {
	format := a["KEYFORMAT"]
	if format == "" || strings.EqualFold(format, "identity") {
		if m := strings.ToUpper(a["METHOD"]); m != "" && m != "NONE" && m != "AES-128" {
			return &DRMError{Scheme: m}
		}
		return nil
	}
	switch {
	case strings.Contains(strings.ToLower(format), "edef8ba9"):
		return &DRMError{Scheme: "Widevine"}
	case strings.Contains(strings.ToLower(format), "9a04f079"):
		return &DRMError{Scheme: "PlayReady"}
	case strings.HasPrefix(strings.ToLower(a["URI"]), "skd:"):
		return &DRMError{Scheme: "FairPlay"}
	}
	return &DRMError{Scheme: format}
}

// ---------- downloading ----------

// HLSOptions describes one stream download. Connections, headers and retries
// mean the same as they do for a byte-range download.
type HLSOptions struct {
	URL         string
	Headers     map[string]string
	OutDir      string
	Filename    string
	Connections int
	ConnLimit   func() int
	MaxRetries  int

	// Variant picks a stream from a master playlist. A negative value takes the
	// highest bandwidth on offer, which is what a player on a fast link does.
	Variant int

	OnStart    func(HLSStart)
	OnProgress func(HLSProgress)
}

type HLSStart struct {
	Target   string
	Variants []hlsVariant
	Chosen   int
	Segments int
	Duration float64
}

type HLSProgress struct {
	Segments      int   // written so far
	TotalSegments int   // in the playlist
	Bytes         int64 // written so far
	Estimate      int64 // projected final size, refined as segments land
	Duration      float64
	Active        int // connections currently fetching a segment
	Limit         int // connections allowed right now
}

// hlsState is the sidecar for a segment download. Segments are appended in
// order, so resuming needs nothing more than "how many went in, and how long
// was the file when they did".
type hlsState struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind"` // always "hls"; loadState refuses these
	Playlist string `json:"playlist"`
	Variant  int    `json:"variant"`
	Total    int    `json:"total_segments"`
	Next     int    `json:"next_segment"`
	Written  int64  `json:"written"`
	InitDone bool   `json:"init_done,omitempty"`
}

func loadHLSState(target string) (*hlsState, bool) {
	b, err := os.ReadFile(statePath(target))
	if err != nil {
		return nil, false
	}
	var s hlsState
	if err := json.Unmarshal(b, &s); err != nil || s.Kind != "hls" || s.Version != stateVersion {
		return nil, false
	}
	return &s, true
}

func saveHLSState(target string, s *hlsState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(target), b)
}

type hlsRun struct {
	opts    HLSOptions
	client  *http.Client
	media   *hlsMedia
	target  string
	variant int // resolved index, so a resume compares like with like

	// initDone records that EXT-X-MAP has been written. It is not implied by
	// "no segments yet": a crash between the two would otherwise write it twice.
	initDone bool

	fetching atomic.Int32 // connections with a segment request in flight

	mu     sync.Mutex
	cond   *sync.Cond
	ready  map[int][]byte // fetched, waiting for its turn to be written
	held   int64          // bytes sitting in ready
	next   int            // next segment index to claim
	want   int            // next segment index to write
	failed error

	keysMu sync.Mutex
	keys   map[string]*keyFetch

	bytes int64
}

// DownloadHLS fetches a playlist and writes its segments, in order, to one
// file. The result is the concatenation a player would feed its demuxer.
func DownloadHLS(ctx context.Context, o HLSOptions) (string, error) {
	if o.MaxRetries <= 0 {
		o.MaxRetries = defaultRetries
	}
	client := newClient()

	base, err := url.Parse(o.URL)
	if err != nil {
		return "", fmt.Errorf("bad playlist URL: %w", err)
	}
	body, err := fetchPlaylist(ctx, client, o, o.URL)
	if err != nil {
		return "", err
	}
	master, media, err := parsePlaylist(body, base)
	if err != nil {
		return "", err
	}

	var variants []hlsVariant
	chosen := -1
	if master != nil {
		variants = master.Variants
		chosen = o.Variant
		if chosen < 0 || chosen >= len(variants) {
			chosen = master.best()
		}
		vURL := variants[chosen].URI
		if base, err = url.Parse(vURL); err != nil {
			return "", fmt.Errorf("bad variant URL: %w", err)
		}
		if body, err = fetchPlaylist(ctx, client, o, vURL); err != nil {
			return "", err
		}
		if inner, m2, err := parsePlaylist(body, base); err != nil {
			return "", err
		} else if m2 == nil {
			_ = inner
			return "", errors.New("the chosen stream points at another master playlist")
		} else {
			media = m2
		}
	}
	if !media.Complete {
		return "", &LiveStreamError{}
	}

	target, resume, release, err := planHLSTarget(o, media, chosen)
	if err != nil {
		return "", err
	}
	defer release()
	r := &hlsRun{
		opts:    o,
		client:  client,
		media:   media,
		target:  target,
		variant: chosen,
		ready:   map[int][]byte{},
		keys:    map[string]*keyFetch{},
	}
	r.cond = sync.NewCond(&r.mu)

	if o.OnStart != nil {
		o.OnStart(HLSStart{
			Target:   target,
			Variants: variants,
			Chosen:   chosen,
			Segments: len(media.Segments),
			Duration: media.TotalDuration,
		})
	}
	return target, r.run(ctx, resume)
}

// hlsTarget names the output. The container follows the segments: an EXT-X-MAP
// means fragmented MP4, everything else in the wild is MPEG-TS.
func hlsName(o HLSOptions, media *hlsMedia) string {
	name := o.Filename
	if name == "" {
		if u, err := url.Parse(o.URL); err == nil {
			name = path.Base(u.Path)
		}
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".m3u8"), ".m3u")
		if name == "" || name == "." || name == "/" || name == "index" || name == "playlist" {
			name = "video"
		}
	}
	if filepath.Ext(name) == "" {
		if media.Init != nil {
			name += ".mp4"
		} else {
			name += ".ts"
		}
	}
	return sanitize(name)
}

// planHLSTarget applies the same rule as a byte-range download: pick up our own
// unfinished work, never write over a file that is already there. A sidecar
// naming a different playlist belongs to another video whose name merely
// collides, so that one gets a numbered name instead.
func planHLSTarget(o HLSOptions, media *hlsMedia, variant int) (string, *hlsState, func(), error) {
	base := filepath.Join(o.OutDir, hlsName(o, media))

	if st, ok := loadHLSState(base); ok && st.Playlist == o.URL {
		if st.Total == len(media.Segments) && st.Variant == variant {
			if fi, err := os.Stat(base); err == nil && fi.Size() >= st.Written {
				if release, ok := reserveTarget(base); ok {
					return base, st, release, nil
				}
			}
		}
		// Our own earlier attempt at this playlist that cannot be picked up:
		// the stream was re-encoded, or a different quality was chosen. Start
		// it over in place rather than leave a dead file behind.
		if release, ok := reserveTarget(base); ok {
			clearState(base)
			return base, nil, release, nil
		}
	}

	for n := 0; n < 10000; n++ {
		cand := numberedName(base, n)
		if pathExists(cand) || pathExists(statePath(cand)) {
			continue
		}
		if release, ok := reserveTarget(cand); ok {
			return cand, nil, release, nil
		}
	}
	return "", nil, nil, fmt.Errorf("could not find a free name for %s", filepath.Base(base))
}

func (r *hlsRun) limit() int {
	n := r.opts.Connections
	if r.opts.ConnLimit != nil {
		n = r.opts.ConnLimit()
	}
	return clampConnections(n)
}

func (r *hlsRun) run(ctx context.Context, resume *hlsState) error {
	if resume != nil {
		r.next, r.want, r.bytes = resume.Next, resume.Next, resume.Written
		r.initDone = resume.InitDone
	}
	f, err := os.OpenFile(r.target, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Trim anything written past the last segment we counted: a kill mid-write
	// can leave a partial segment behind, and appending after it would corrupt
	// the stream.
	if err := f.Truncate(r.bytes); err != nil {
		return err
	}
	if _, err := f.Seek(r.bytes, io.SeekStart); err != nil {
		return err
	}

	if r.media.Init != nil && !r.initDone {
		data, err := r.fetchSegment(ctx, r.media.Init)
		if err != nil {
			return fmt.Errorf("initialisation segment: %w", err)
		}
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		r.bytes += int64(n)
		r.initDone = true
		r.persist()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A cond does not select on a context, so wake the waiters when it ends.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < r.limit(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.worker(ctx)
		}()
	}

	err = r.writeLoop(ctx, f)
	cancel()
	wg.Wait()

	if err != nil {
		r.persist()
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	clearState(r.target)
	return nil
}

// writeLoop appends segments in playlist order. Everything else exists to keep
// this loop fed.
func (r *hlsRun) writeLoop(ctx context.Context, f *os.File) error {
	last := time.Now()
	for {
		r.mu.Lock()
		for r.ready[r.want] == nil && r.failed == nil && ctx.Err() == nil {
			r.cond.Wait()
		}
		if r.failed != nil {
			err := r.failed
			r.mu.Unlock()
			return err
		}
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return err
		}
		data := r.ready[r.want]
		delete(r.ready, r.want)
		r.held -= int64(len(data))
		r.want++
		done := r.want
		r.cond.Broadcast() // a slot and its bytes just freed up
		r.mu.Unlock()

		n, err := f.Write(data)
		r.bytes += int64(n)
		if err != nil {
			r.mu.Lock()
			if r.failed == nil {
				r.failed = err
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return err
		}
		if done >= len(r.media.Segments) {
			r.report()
			return nil
		}
		if time.Since(last) >= 400*time.Millisecond {
			last = time.Now()
			r.persist()
			r.report()
		}
	}
}

func (r *hlsRun) worker(ctx context.Context) {
	for {
		r.mu.Lock()
		for r.failed == nil && ctx.Err() == nil &&
			(r.next >= len(r.media.Segments) ||
				r.next >= r.want+r.limit()*2 ||
				(r.held >= maxBufferedBytes && r.next > r.want)) {
			if r.next >= len(r.media.Segments) {
				r.mu.Unlock()
				return
			}
			r.cond.Wait()
		}
		if r.failed != nil || ctx.Err() != nil {
			r.mu.Unlock()
			return
		}
		i := r.next
		r.next++
		r.mu.Unlock()

		r.fetching.Add(1)
		data, err := r.fetchSegment(ctx, &r.media.Segments[i])
		r.fetching.Add(-1)

		r.mu.Lock()
		if err != nil {
			if r.failed == nil && ctx.Err() == nil {
				r.failed = fmt.Errorf("segment %d/%d: %w", i+1, len(r.media.Segments), err)
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return
		}
		r.ready[i] = data
		r.held += int64(len(data))
		r.cond.Broadcast()
		r.mu.Unlock()
	}
}

func (r *hlsRun) fetchSegment(ctx context.Context, seg *hlsSegment) ([]byte, error) {
	data, err := r.getWithRetry(ctx, seg.URI, seg.Offset, seg.Length)
	if err != nil {
		return nil, err
	}
	if seg.Key == nil {
		return data, nil
	}
	key, err := r.keyFor(ctx, seg.Key)
	if err != nil {
		return nil, err
	}
	iv := seg.Key.IV
	if iv == nil {
		iv = defaultIV(seg.Seq)
	}
	return decryptAES128(data, key, iv)
}

// keyFetch lets the first worker to want a key fetch it while the others wait
// for that one result.
type keyFetch struct {
	done chan struct{}
	key  []byte
	err  error
}

// keyFor caches keys by URI. A playlist normally names one key for hundreds of
// segments, so fetching per segment would multiply the requests for nothing.
// Checking the cache and then fetching is not enough on its own: several
// workers reach the first encrypted segment together, all miss, and all fetch.
func (r *hlsRun) keyFor(ctx context.Context, k *hlsKey) ([]byte, error) {
	r.keysMu.Lock()
	kf, inFlight := r.keys[k.URI]
	if !inFlight {
		kf = &keyFetch{done: make(chan struct{})}
		r.keys[k.URI] = kf
	}
	r.keysMu.Unlock()

	if !inFlight {
		kf.key, kf.err = r.fetchKey(ctx, k.URI)
		if kf.err != nil {
			// A failure must not be cached, or every later segment inherits it
			// without ever retrying.
			r.keysMu.Lock()
			delete(r.keys, k.URI)
			r.keysMu.Unlock()
		}
		close(kf.done)
	}

	select {
	case <-kf.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return kf.key, kf.err
}

func (r *hlsRun) fetchKey(ctx context.Context, uri string) ([]byte, error) {
	key, err := r.getWithRetry(ctx, uri, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("fetching the decryption key: %w", err)
	}
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("decryption key is %d bytes, expected %d", len(key), aes.BlockSize)
	}
	return key, nil
}

func (r *hlsRun) getWithRetry(ctx context.Context, u string, offset, length int64) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= r.opts.MaxRetries+1; attempt++ {
		b, err := r.getOnce(ctx, u, offset, length)
		if err == nil {
			return b, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, err
		}
		select {
		case <-time.After(backoff(attempt, err)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (r *hlsRun) getOnce(ctx context.Context, u string, offset, length int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, r.opts.Headers)
	if length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, &statusError{
			code:       resp.StatusCode,
			status:     resp.Status,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	limit := int64(maxSegmentBytes)
	if length > 0 && length < limit {
		limit = length
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if length > 0 && int64(len(data)) != length {
		return nil, fmt.Errorf("byte range asked for %d bytes, got %d", length, len(data))
	}
	return data, nil
}

func fetchPlaylist(ctx context.Context, c *http.Client, o HLSOptions, u string) ([]byte, error) {
	r := &hlsRun{opts: o, client: c}
	if r.opts.MaxRetries <= 0 {
		r.opts.MaxRetries = defaultRetries
	}
	return r.getWithRetry(ctx, u, 0, 0)
}

// defaultIV is the segment's sequence number as a 128-bit big-endian integer,
// which is what a playlist means when it names a key but no IV.
func defaultIV(seq int64) []byte {
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(seq))
	return iv
}

func decryptAES128(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment is %d bytes, not a multiple of %d", len(data), aes.BlockSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("segment did not decrypt (wrong key, or the padding is not PKCS#7)")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, errors.New("segment did not decrypt (wrong key, or the padding is not PKCS#7)")
		}
	}
	return out[:len(out)-pad], nil
}

func (r *hlsRun) persist() {
	r.mu.Lock()
	done := r.want
	r.mu.Unlock()
	saveHLSState(r.target, &hlsState{
		Version:  stateVersion,
		Kind:     "hls",
		Playlist: r.opts.URL,
		Variant:  r.variant,
		Total:    len(r.media.Segments),
		Next:     done,
		Written:  r.bytes,
		InitDone: r.initDone,
	})
}

func (r *hlsRun) report() {
	if r.opts.OnProgress == nil {
		return
	}
	r.mu.Lock()
	done := r.want
	r.mu.Unlock()
	total := len(r.media.Segments)
	est := r.bytes
	if done > 0 && done < total {
		est = r.bytes / int64(done) * int64(total)
	}
	r.opts.OnProgress(HLSProgress{
		Segments:      done,
		TotalSegments: total,
		Bytes:         r.bytes,
		Estimate:      est,
		Duration:      r.media.TotalDuration,
		Active:        int(r.fetching.Load()),
		Limit:         r.limit(),
	})
}

// ---------- inspection ----------

// StreamInfo is what a browser needs in order to offer a choice before
// anything is downloaded: whether this really is a stream, how long it runs,
// and which qualities it carries.
type StreamInfo struct {
	Kind     string        `json:"kind"`
	Duration float64       `json:"duration,omitempty"`
	Segments int           `json:"segments,omitempty"`
	Variants []VariantView `json:"variants,omitempty"`
	// Best is the variant a player on a fast link would settle on, or -1 when
	// the playlist offers no choice.
	Best int `json:"best"`
}

type VariantView struct {
	Index      int    `json:"index"`
	Label      string `json:"label"`
	Height     int    `json:"height,omitempty"`
	Bandwidth  int    `json:"bandwidth,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// InspectHLS reads a playlist without downloading it. A DRM-protected or live
// stream reports its error here, so the user finds out before a task is
// created rather than watching one fail.
func InspectHLS(ctx context.Context, o HLSOptions) (*StreamInfo, error) {
	if o.MaxRetries <= 0 {
		o.MaxRetries = defaultRetries
	}
	client := newClient()
	base, err := url.Parse(o.URL)
	if err != nil {
		return nil, fmt.Errorf("bad playlist URL: %w", err)
	}
	body, err := fetchPlaylist(ctx, client, o, o.URL)
	if err != nil {
		return nil, err
	}
	master, media, err := parsePlaylist(body, base)
	if err != nil {
		return nil, err
	}

	info := &StreamInfo{Kind: "hls", Best: -1}
	if master != nil {
		info.Best = master.best()
		for i, v := range master.Variants {
			info.Variants = append(info.Variants, VariantView{
				Index:      i,
				Label:      v.Label(),
				Height:     v.Height(),
				Bandwidth:  v.Bandwidth,
				Resolution: v.Resolution,
			})
		}
		// One more request buys the duration and segment count for the quality
		// that will actually be downloaded, which is what the user wants to see.
		chosen := master.Variants[info.Best]
		if vb, err := url.Parse(chosen.URI); err == nil {
			if body, err := fetchPlaylist(ctx, client, o, chosen.URI); err == nil {
				if _, m2, err := parsePlaylist(body, vb); err == nil && m2 != nil {
					media = m2
				}
			}
		}
	}
	if media != nil {
		info.Duration = media.TotalDuration
		info.Segments = len(media.Segments)
		if !media.Complete {
			return nil, &LiveStreamError{}
		}
	}
	return info, nil
}
