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
	"math"
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
var maxSegmentBytes int64 = 256 << 20

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

// SeparateAudioError prevents a master playlist with an external audio
// rendition from being saved as a plausible-looking but silent video file.
// yt-dlp already has the demuxing and muxing support needed for these streams.
type SeparateAudioError struct{ Group string }

func (e *SeparateAudioError) Error() string {
	return "this HLS quality keeps audio in a separate rendition; use the yt-dlp page download so audio and video can be merged"
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
	Init     *hlsSegment // the EXT-X-MAP in force for this segment, nil for none
}

type hlsMedia struct {
	TargetDuration float64
	TotalDuration  float64
	Segments       []hlsSegment
	Init           *hlsSegment // the map the first segment is played with: what the file starts with
	Complete       bool        // saw EXT-X-ENDLIST
}

// fragmented reports whether the segments are fragmented MP4, which is what an
// EXT-X-MAP says they are.
func (m *hlsMedia) fragmented() bool {
	if m.Init != nil {
		return true
	}
	for i := range m.Segments {
		if m.Segments[i].Init != nil {
			return true
		}
	}
	return false
}

// initBefore returns the init segment to write in front of segment i, or nil.
// The one the file starts with is written by run() itself; a later one belongs
// where its map takes effect (after a discontinuity, an ad break, a change of
// codec or size), because the segments before it were made against the old one
// and the ones after against the new. A map named again unchanged is not a new
// map.
func (m *hlsMedia) initBefore(i int) *hlsSegment {
	if i <= 0 || i >= len(m.Segments) {
		return nil
	}
	cur, prev := m.Segments[i].Init, m.Segments[i-1].Init
	if cur == nil || (prev != nil && cur.URI == prev.URI && cur.Offset == prev.Offset && cur.Length == prev.Length) {
		return nil
	}
	return cur
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
	Variants    []hlsVariant
	AudioGroups map[string]bool // group id -> at least one external URI
}

func (m *hlsMaster) hasSeparateAudio(v hlsVariant) bool {
	return v.AudioGroup != "" && m.AudioGroups[v.AudioGroup]
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
	master := &hlsMaster{AudioGroups: map[string]bool{}}
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
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if strings.EqualFold(a["TYPE"], "AUDIO") && a["GROUP-ID"] != "" && a["URI"] != "" {
				master.AudioGroups[a["GROUP-ID"]] = true
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
		curInit  *hlsSegment // the EXT-X-MAP most recently seen
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
			curInit = &init
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
			s := hlsSegment{URI: u.String(), Duration: dur, Seq: seq, Key: key, Init: curInit}
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
	m.Init = m.Segments[0].Init
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
	// StallTimeout ends a request that has received nothing for this long and
	// retries it. Zero means 30 seconds.
	StallTimeout time.Duration
	// Limiters are the speed limits every segment fetch keeps to.
	Limiters []*RateLimiter

	// Variant picks a stream from a master playlist. A negative value takes the
	// highest bandwidth on offer, which is what a player on a fast link does.
	Variant int

	// Refresh continues an interrupted download from a playlist at another
	// address, as Options.Refresh does for a file. The segments already saved
	// are kept as long as the stream is still the one they came from; if it
	// is not, the download is refused instead of silently starting over.
	Refresh bool

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
	// Duration is how long the stream plays, in seconds: what, besides its
	// quality and its number of segments, tells one stream from another when
	// a new address is offered for it. A sidecar written before this was kept
	// has none, and is not held to it.
	Duration float64 `json:"duration,omitempty"`
}

// sameLength reports whether media plays for as long as the stream the sidecar
// was written for. The tolerance is for a server that rounds the lengths of
// its segments differently from one address to the next.
func (s *hlsState) sameLength(media *hlsMedia) bool {
	if s.Duration <= 0 {
		return true
	}
	return math.Abs(s.Duration-media.TotalDuration) <= max(1, s.Duration/200)
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

// fits reports whether the sidecar describes a point a download of media
// could have stopped at. Its numbers go straight into the run, as the length
// the file is cut to and the index of the segment fetched next, so one that
// was damaged or edited by hand has to be turned down before then: an index
// below zero is a panic in a worker, which ends the daemon and every download
// in it, and the rest resume into a file that is not the stream.
func (s *hlsState) fits(media *hlsMedia) bool {
	if s.Total != len(media.Segments) || s.Next < 0 || s.Next > s.Total || s.Written < 0 {
		return false
	}
	hasInit := media.Init != nil
	if s.InitDone && !hasInit {
		return false // the length counts an init segment the stream no longer has
	}
	if s.Next > 0 {
		// Segments are in the file, behind the init segment if there is one.
		return s.Written > 0 && s.InitDone == hasInit
	}
	return s.InitDone || s.Written == 0
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
	workers  atomic.Int32 // connections alive, however busy

	mu     sync.Mutex
	cond   *sync.Cond
	ready  map[int][]byte // fetched, waiting for its turn to be written
	held   int64          // bytes sitting in ready
	next   int            // next segment index to claim
	want   int            // next segment index to write
	failed error

	syncedAt time.Time // when persist last flushed the file; only its caller touches it

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

	if _, err := url.Parse(o.URL); err != nil {
		return "", fmt.Errorf("bad playlist URL: %w", err)
	}
	body, base, err := fetchPlaylist(ctx, client, o, o.URL)
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
		if master.hasSeparateAudio(variants[chosen]) {
			return "", &SeparateAudioError{Group: variants[chosen].AudioGroup}
		}
		vURL := variants[chosen].URI
		if _, err = url.Parse(vURL); err != nil {
			return "", fmt.Errorf("bad variant URL: %w", err)
		}
		if body, base, err = fetchPlaylist(ctx, client, o, vURL); err != nil {
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

	// A byte-range download makes its folder once it knows the link works; so
	// does this. Without it a download aimed at a folder that is not there yet
	// fails when it first opens its file.
	if o.OutDir == "" {
		o.OutDir = "."
	}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return "", err
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
	ext := ".ts"
	if media.fragmented() {
		ext = ".mp4"
	}
	// The name is usually a page title, and a title is not a path: the slash in
	// "AC/DC" is part of it, where sanitize would take it for a folder and keep
	// only what follows. Nothing here may separate a folder either, so both
	// kinds become a plain character.
	name = strings.NewReplacer("/", "_", `\`, "_").Replace(name)
	name = strings.TrimRight(name, " .")
	if name == "" {
		return "video" + ext
	}
	// Nor is a dot in a title an extension ("Node.js Crash Course", "Episode
	// 1.5"). Only a name that already ends in a media extension is left as it is.
	if !hasMediaExt(name) {
		name += ext
	}
	if n := sanitize(name); n != "" {
		return n
	}
	// Nothing in the name survives (a name of dots, or bytes that are not
	// text). An empty one would plan the download folder itself as the target
	// and save the stream beside it, as "<folder> (1)".
	return "video" + ext
}

// mediaExts are the extensions that mean a name already says what the file is.
var mediaExts = map[string]bool{
	".ts": true, ".m2ts": true, ".mts": true, ".mp4": true, ".m4v": true, ".m4a": true,
	".mkv": true, ".mov": true, ".webm": true, ".flv": true, ".avi": true,
	".mp3": true, ".aac": true,
}

func hasMediaExt(name string) bool {
	return mediaExts[strings.ToLower(filepath.Ext(name))]
}

// planHLSTarget applies the same rule as a byte-range download: pick up our own
// unfinished work, never write over a file that is already there. A sidecar
// naming a different playlist belongs to another video whose name merely
// collides, so that one gets a numbered name instead.
//
// A refresh is the exception to the last part. The address is new on purpose,
// a playlist behind a signed link having a new one every time it is asked
// for, so the sidecar is matched on what the stream is, its quality and its
// number of segments and how long it plays, and not on where it was found.
// That is as much as a file's size says about a file.
func planHLSTarget(o HLSOptions, media *hlsMedia, variant int) (string, *hlsState, func(), error) {
	base := filepath.Join(o.OutDir, hlsName(o, media))

	st, hasState := loadHLSState(base)
	if hasState && (st.Playlist == o.URL || o.Refresh) {
		if st.Variant == variant && st.fits(media) && (!o.Refresh || st.sameLength(media)) {
			if fi, err := os.Stat(base); err == nil && fi.Size() >= st.Written {
				if release, ok := reserveTarget(base); ok {
					return base, st, release, nil
				}
			}
		}
		// Our own earlier attempt at this playlist that cannot be picked up:
		// the stream was re-encoded, a different quality was chosen, or the
		// sidecar does not add up. Start it over in place rather than leave a
		// dead file behind.
		if !o.Refresh {
			if release, ok := reserveTarget(base); ok {
				clearState(base)
				return base, nil, release, nil
			}
		}
	}

	if o.Refresh {
		// The caller has segments it means to keep. Starting over, here or
		// under another name, is not what it asked for.
		name := filepath.Base(base)
		switch {
		case !hasState:
			return "", nil, nil, &RefreshMismatchError{Reason: "no saved progress was found for " + name}
		case st.Total != len(media.Segments):
			return "", nil, nil, &RefreshMismatchError{Reason: fmt.Sprintf(
				"it points to a different stream (%d segments, the partial download has %d)",
				len(media.Segments), st.Total)}
		case st.Variant != variant:
			return "", nil, nil, &RefreshMismatchError{Reason: "it points to another quality of the stream than " +
				name + " was saved in"}
		case !st.sameLength(media):
			return "", nil, nil, &RefreshMismatchError{Reason: fmt.Sprintf(
				"it points to a different stream (%s long, the partial download is %s)",
				time.Duration(media.TotalDuration*float64(time.Second)).Round(time.Second),
				time.Duration(st.Duration*float64(time.Second)).Round(time.Second))}
		default:
			// The sidecar and the file do not add up for this stream, or
			// another download has the file open.
			return "", nil, nil, &RefreshMismatchError{Reason: "what is saved of " + name + " cannot be continued from it"}
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
		if r.opts.Refresh {
			// The segments are this address's from here on. Say so before
			// anything can go wrong: the caller counts the new address as
			// taken up once the plan is made, and a run that fails to open
			// the file would otherwise leave a sidecar that names the old one.
			r.persist()
		}
	}
	// The init segment is fetched before the file exists. One that cannot be had
	// (refused, timed out, or the user pausing while it is in flight) then
	// leaves nothing behind, where an empty file would burn the name and the
	// next attempt would go to "clip (1).mp4".
	var initData []byte
	needInit := r.media.Init != nil && !r.initDone
	if needInit {
		var err error
		if initData, err = r.fetchSegment(ctx, r.media.Init); err != nil {
			return fmt.Errorf("initialisation segment: %w", err)
		}
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

	if needInit {
		n, err := f.Write(initData)
		if err != nil {
			if r.bytes == 0 {
				f.Close()
				os.Remove(r.target)
				clearState(r.target)
			}
			return err
		}
		r.bytes += int64(n)
		r.initDone = true
		r.persist(f, true)
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
	spawn := func() {
		wg.Add(1)
		r.workers.Add(1)
		go func() {
			defer wg.Done()
			if !r.worker(ctx) {
				r.workers.Add(-1)
			}
		}()
	}
	for i := 0; i < r.limit(); i++ {
		spawn()
	}

	// Reporting from the write loop alone is not enough: it waits for the next
	// segment in playlist order, so one slow segment freezes the display for as
	// long as it takes. The same tick tops up connections when the limit went
	// up, and wakes the idle ones to see whether it went down: workers retire
	// themselves, but only when they look, and nothing else announces a change
	// of limit.
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.report()
				r.persist(f, false)
				r.mu.Lock()
				short := r.limit() - int(r.workers.Load())
				work := r.next < len(r.media.Segments)
				r.cond.Broadcast()
				r.mu.Unlock()
				for i := 0; work && i < short; i++ {
					spawn()
				}
			}
		}
	}()

	err = r.writeLoop(ctx, f)
	cancel()
	<-ticked // nothing spawns after this, so waiting on the group is safe
	wg.Wait()

	if err != nil {
		if r.bytes == 0 {
			// Nothing ever reached the file. Leaving an empty one behind would
			// burn the name too: the next attempt would go to "clip (1).ts".
			f.Close()
			os.Remove(r.target)
			clearState(r.target)
			return err
		}
		r.persist(f, true)
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
	for {
		r.mu.Lock()
		// Checked before waiting, not after writing: a run that died between
		// its last segment and clearing its sidecar resumes with every segment
		// written, and no worker will ever deliver another to wake this up.
		if r.want >= len(r.media.Segments) {
			r.mu.Unlock()
			r.report()
			return nil
		}
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
		r.cond.Broadcast() // a slot and its bytes just freed up
		r.mu.Unlock()

		n, err := f.Write(data)
		if err != nil {
			r.mu.Lock()
			if r.failed == nil {
				r.failed = err
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return err
		}

		// The number of segments written and the length that goes with it move
		// together under one lock. A sidecar that caught one without the other
		// would resume at the wrong offset and drop a segment from the middle.
		r.mu.Lock()
		r.want++
		r.bytes += int64(n)
		r.mu.Unlock()
	}
}

// worker claims and fetches segments until there are none left to claim. It
// reports whether it left because the connection limit dropped, in which case
// it has already taken itself off the count: that has to happen under the same
// lock as the check, or two workers looking at the same excess would both go.
func (r *hlsRun) worker(ctx context.Context) (retired bool) {
	for {
		r.mu.Lock()
		for r.failed == nil && ctx.Err() == nil {
			if r.next >= len(r.media.Segments) {
				r.mu.Unlock()
				return false
			}
			// Between segments is the only place a connection can be given up
			// without throwing a half-fetched one away. The read-ahead window
			// below follows the limit down too, but on its own it only caps
			// the connections at twice what was asked for.
			if int(r.workers.Load()) > r.limit() {
				r.workers.Add(-1)
				r.mu.Unlock()
				return true
			}
			if r.next < r.want+r.limit()*2 && !(r.held >= maxBufferedBytes && r.next > r.want) {
				break
			}
			r.cond.Wait()
		}
		if r.failed != nil || ctx.Err() != nil {
			r.mu.Unlock()
			return false
		}
		i := r.next
		r.next++
		r.mu.Unlock()

		r.fetching.Add(1)
		data, err := r.fetchWithMap(ctx, i)
		r.fetching.Add(-1)

		r.mu.Lock()
		if err != nil {
			if r.failed == nil && ctx.Err() == nil {
				r.failed = fmt.Errorf("segment %d/%d: %w", i+1, len(r.media.Segments), err)
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return false
		}
		r.ready[i] = data
		r.held += int64(len(data))
		r.cond.Broadcast()
		r.mu.Unlock()
	}
}

// fetchWithMap fetches segment i, with the init segment that takes effect at it
// in front. Carrying the two as one piece keeps the write loop, the byte count
// and the sidecar as they were: a resume cuts the file at a segment boundary,
// which is also the boundary of the map that belongs to the next one.
func (r *hlsRun) fetchWithMap(ctx context.Context, i int) ([]byte, error) {
	data, err := r.fetchSegment(ctx, &r.media.Segments[i])
	if err != nil {
		return nil, err
	}
	if init := r.media.initBefore(i); init != nil {
		head, err := r.fetchSegment(ctx, init)
		if err != nil {
			return nil, fmt.Errorf("initialisation segment: %w", err)
		}
		data = append(head, data...)
	}
	return data, nil
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
	b, _, err := r.getWithRetryFrom(ctx, u, offset, length)
	return b, err
}

// getWithRetryFrom also says where the bytes came from once redirects were
// followed, which a playlist needs: its relative entries mean that address.
func (r *hlsRun) getWithRetryFrom(ctx context.Context, u string, offset, length int64) ([]byte, *url.URL, error) {
	var last error
	for attempt := 1; attempt <= r.opts.MaxRetries+1; attempt++ {
		b, final, err := r.getOnceFrom(ctx, u, offset, length)
		if err == nil {
			return b, final, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, nil, err
		}
		select {
		case <-time.After(backoff(attempt, err)):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, last
}

func (r *hlsRun) getOnce(ctx context.Context, u string, offset, length int64) ([]byte, error) {
	b, _, err := r.getOnceFrom(ctx, u, offset, length)
	return b, err
}

func (r *hlsRun) getOnceFrom(ctx context.Context, u string, offset, length int64) ([]byte, *url.URL, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	// u is whatever the playlist named: a variant, a segment or a key, on any
	// host it likes.
	applyHeaders(req, r.opts.Headers, r.opts.URL)
	if length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, nil, &statusError{
			code:       resp.StatusCode,
			status:     resp.Status,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	limit := maxSegmentBytes
	if length > 0 && length < limit {
		limit = length
	}
	// A segment server that goes quiet mid-response would otherwise hold this
	// connection, and with it every segment queued behind it, forever.
	body := newStallGuard(resp.Body, r.opts.StallTimeout, cancel)
	defer body.stop()
	// Read one byte past the cap: io.LimitReader stops silently at the limit,
	// and a segment quietly cut short produces a file of the right shape that
	// plays wrong. The speed limit is paid under the run's context, not the
	// request's, which the guard has already cancelled when it reports a
	// stall along with the last bytes.
	data, err := readPaced(ctx, io.LimitReader(body, limit+1), r.opts.Limiters)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("response is too large (over %d bytes)", limit)
	}
	if length > 0 && int64(len(data)) != length {
		return nil, nil, fmt.Errorf("byte range asked for %d bytes, got %d", length, len(data))
	}
	return data, resp.Request.URL, nil
}

// fetchPlaylist returns the playlist and the address it was finally served
// from. After a redirect that is not the address that was asked for, and the
// playlist's relative entries are relative to it.
func fetchPlaylist(ctx context.Context, c *http.Client, o HLSOptions, u string) ([]byte, *url.URL, error) {
	r := &hlsRun{opts: o, client: c}
	if r.opts.MaxRetries <= 0 {
		r.opts.MaxRetries = defaultRetries
	}
	return r.getWithRetryFrom(ctx, u, 0, 0)
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

// persist saves progress, in the order that keeps the sidecar honest: the
// count is taken first, the file flushed after, and only then is the sidecar
// written. The flush covers every byte the count includes (and any written
// meanwhile, which the sidecar does not claim), so a power cut cannot leave it
// saying segments are in when their bytes were still in the cache, which a
// resume would trust and leave as zeros inside the stream. The flush is done
// when one is due (dataSyncEvery, shared with the byte-range engine), or at
// once with force, as the init segment and a stopping download do. In between
// nothing is written, and the sidecar keeps the point of the last flush.
func (r *hlsRun) persist(f *os.File, force bool) {
	if !force && !r.syncedAt.IsZero() && time.Since(r.syncedAt) < dataSyncEvery {
		return
	}
	r.mu.Lock()
	done, written := r.want, r.bytes
	r.mu.Unlock()
	if err := syncData(f); err != nil {
		// Nothing is known to be on disk beyond the last flush, and that is
		// what the sidecar already says.
		return
	}
	r.syncedAt = time.Now()
	saveHLSState(r.target, &hlsState{
		Version:  stateVersion,
		Kind:     "hls",
		Playlist: r.opts.URL,
		Variant:  r.variant,
		Total:    len(r.media.Segments),
		Next:     done,
		Written:  written,
		InitDone: r.initDone,
		Duration: r.media.TotalDuration,
	})
}

func (r *hlsRun) report() {
	if r.opts.OnProgress == nil {
		return
	}
	r.mu.Lock()
	done, written := r.want, r.bytes
	r.mu.Unlock()
	total := len(r.media.Segments)
	est := written
	if done > 0 && done < total {
		est = written / int64(done) * int64(total)
	}
	r.opts.OnProgress(HLSProgress{
		Segments:      done,
		TotalSegments: total,
		Bytes:         written,
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
	Title    string        `json:"title,omitempty"`
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
	if _, err := url.Parse(o.URL); err != nil {
		return nil, fmt.Errorf("bad playlist URL: %w", err)
	}
	body, base, err := fetchPlaylist(ctx, client, o, o.URL)
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
		if master.hasSeparateAudio(chosen) {
			return nil, &SeparateAudioError{Group: chosen.AudioGroup}
		}
		if body, vb, err := fetchPlaylist(ctx, client, o, chosen.URI); err == nil {
			if _, m2, err := parsePlaylist(body, vb); err == nil && m2 != nil {
				media = m2
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
