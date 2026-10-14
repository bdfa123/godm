package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- a stand-in for a real HLS origin ----------

type hlsServer struct {
	*httptest.Server
	segs [][]byte
	init []byte
	key  []byte // nil leaves the segments in the clear
	live bool   // omit EXT-X-ENDLIST

	initFail  int           // status to answer the init segment with, zero for the bytes
	initDelay time.Duration // how long to hold the init segment back

	mu       sync.Mutex
	hits     map[string]int
	failAt   map[int]int           // segment index -> status code to answer with
	delay    map[int]time.Duration // segment index -> how long to stall
	inFlight int
	peak     int // most segment requests ever open at once
}

func newHLSServer(t *testing.T, segs [][]byte) *hlsServer {
	t.Helper()
	s := &hlsServer{
		segs:   segs,
		hits:   map[string]int{},
		failAt: map[int]int{},
		delay:  map[int]time.Duration{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *hlsServer) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// resetPeak starts the peak over from what is open right now.
func (s *hlsServer) resetPeak() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peak = s.inFlight
}

func (s *hlsServer) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *hlsServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	fail := s.failAt
	delay := s.delay
	s.mu.Unlock()

	switch {
	case r.URL.Path == "/master.m3u8":
		fmt.Fprint(w, s.master())
	case strings.HasSuffix(r.URL.Path, ".m3u8"):
		fmt.Fprint(w, s.media())
	case r.URL.Path == "/key.bin":
		w.Write(s.key)
	case r.URL.Path == "/init.mp4":
		s.mu.Lock()
		code, d := s.initFail, s.initDelay
		s.mu.Unlock()
		if d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		w.Write(s.init)
	case strings.HasPrefix(r.URL.Path, "/seg"):
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/seg"), ".ts"))
		if err != nil || i < 0 || i >= len(s.segs) {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		code, bad := fail[i]
		d := delay[i]
		s.inFlight++
		if s.inFlight > s.peak {
			s.peak = s.inFlight
		}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inFlight--
			s.mu.Unlock()
		}()
		if d > 0 {
			time.Sleep(d)
		}
		if bad {
			http.Error(w, http.StatusText(code), code)
			return
		}
		w.Write(s.segmentBody(i))
	default:
		http.NotFound(w, r)
	}
}

func (s *hlsServer) segmentBody(i int) []byte {
	if s.key == nil {
		return s.segs[i]
	}
	return encryptHLSSegment(s.segs[i], s.key, int64(i))
}

func (s *hlsServer) master() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i, bw := range []int{800000, 2400000, 5200000} {
		res := []string{"640x360", "1280x720", "1920x1080"}[i]
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s,CODECS=\"avc1.4d401f,mp4a.40.2\"\n", bw, res)
		fmt.Fprintf(&b, "media%d.m3u8\n", i)
	}
	return b.String()
}

func (s *hlsServer) media() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
	if s.init != nil {
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	if s.key != nil {
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n")
	}
	for i := range s.segs {
		fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
	}
	if !s.live {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// encryptHLSSegment is deliberately written out in full rather than calling
// defaultIV: if that helper ever changed, reusing it here would change both
// sides of the test at once and prove nothing.
func encryptHLSSegment(plain, key []byte, seq int64) []byte {
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(seq))
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(buf, buf)
	return buf
}

// makeSegments gives every segment its own length and its own filler byte, so
// a mix-up in order or a duplicated segment shows up as a byte difference.
func makeSegments(n int) [][]byte {
	segs := make([][]byte, n)
	for i := range segs {
		segs[i] = bytes.Repeat([]byte{byte(i + 1)}, 1000+i*37)
	}
	return segs
}

func joined(segs [][]byte) []byte { return bytes.Join(segs, nil) }

// ---------- parsing ----------

func TestParseAttrsKeepsCommasInsideQuotes(t *testing.T) {
	a := parseAttrs(`BANDWIDTH=5200000,RESOLUTION=1920x1080,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aud0"`)
	if a["CODECS"] != "avc1.4d401f,mp4a.40.2" {
		t.Errorf("CODECS = %q, the comma inside the quotes was treated as a separator", a["CODECS"])
	}
	if a["BANDWIDTH"] != "5200000" || a["RESOLUTION"] != "1920x1080" || a["AUDIO"] != "aud0" {
		t.Errorf("attributes = %v", a)
	}
}

func TestParseMasterPicksTheBestStream(t *testing.T) {
	s := newHLSServer(t, makeSegments(3))
	base, _ := url.Parse(s.URL + "/master.m3u8")
	master, media, err := parsePlaylist([]byte(s.master()), base)
	if err != nil {
		t.Fatal(err)
	}
	if media != nil || master == nil {
		t.Fatal("a playlist with EXT-X-STREAM-INF is a master playlist")
	}
	if len(master.Variants) != 3 {
		t.Fatalf("got %d variants, want 3", len(master.Variants))
	}
	if got := master.best(); got != 2 {
		t.Errorf("best variant = %d, want the 5.2 Mbps one (2)", got)
	}
	if got := master.Variants[2].Label(); got != "1080p · 5.2 Mbps" {
		t.Errorf("label = %q", got)
	}
	if got := master.Variants[0].URI; got != s.URL+"/media0.m3u8" {
		t.Errorf("relative variant URI resolved to %q", got)
	}
}

func TestParseMediaByteRangesFollowOnFromEachOther(t *testing.T) {
	body := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXTINF:4.0,
#EXT-X-BYTERANGE:1000@0
whole.ts
#EXTINF:4.0,
#EXT-X-BYTERANGE:500
whole.ts
#EXTINF:4.0,
#EXT-X-BYTERANGE:250
whole.ts
#EXT-X-ENDLIST
`
	base, _ := url.Parse("https://example.test/v/index.m3u8")
	_, m, err := parsePlaylist([]byte(body), base)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ off, length int64 }{{0, 1000}, {1000, 500}, {1500, 250}}
	for i, w := range want {
		if m.Segments[i].Offset != w.off || m.Segments[i].Length != w.length {
			t.Errorf("segment %d = %d@%d, want %d@%d",
				i, m.Segments[i].Length, m.Segments[i].Offset, w.length, w.off)
		}
	}
	if m.TotalDuration != 12 {
		t.Errorf("duration = %v, want 12", m.TotalDuration)
	}
}

func TestDRMStreamsAreRefused(t *testing.T) {
	base, _ := url.Parse("https://example.test/v/index.m3u8")
	cases := map[string]string{
		"FairPlay":  `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://abc",KEYFORMAT="com.apple.streamingkeydelivery"`,
		"Widevine":  `#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,URI="data:;base64,AAA",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"`,
		"PlayReady": `#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,URI="data:;base64,AAA",KEYFORMAT="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95"`,
	}
	for name, tag := range cases {
		body := "#EXTM3U\n" + tag + "\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
		_, _, err := parsePlaylist([]byte(body), base)
		var drm *DRMError
		if err == nil || !asDRM(err, &drm) {
			t.Errorf("%s: err = %v, want a DRMError", name, err)
		}
	}

	// Plain AES-128 with the key served in the open is not DRM and must be
	// accepted, or the refusal above would be useless in practice.
	ok := "#EXTM3U\n" + `#EXT-X-KEY:METHOD=AES-128,URI="key.bin"` + "\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	if _, _, err := parsePlaylist([]byte(ok), base); err != nil {
		t.Errorf("plain AES-128 was refused: %v", err)
	}
}

func asDRM(err error, target **DRMError) bool {
	d, ok := err.(*DRMError)
	if ok {
		*target = d
	}
	return ok
}

func TestLiveStreamIsRefusedRatherThanTruncated(t *testing.T) {
	s := newHLSServer(t, makeSegments(3))
	s.live = true
	_, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Connections: 2,
	})
	if _, ok := err.(*LiveStreamError); !ok {
		t.Fatalf("err = %v, want a LiveStreamError; silently saving a prefix of a live stream looks like a corrupt file", err)
	}
}

// ---------- downloading ----------

func TestDownloadHLSWritesTheSegmentsInOrder(t *testing.T) {
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	// The first segment is the slowest, so with eight connections almost
	// everything finishes before it does. Appending in completion order would
	// scramble the file; this is what the ordered writer is for.
	s.delay[0] = 250 * time.Millisecond

	dir := t.TempDir()
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := joined(segs); !bytes.Equal(got, want) {
		at := -1
		for i := 0; i < len(got) && i < len(want); i++ {
			if got[i] != want[i] {
				at = i
				break
			}
		}
		t.Fatalf("the file is not the segments in playlist order: %d bytes (want %d), "+
			"first difference at byte %d", len(got), len(want), at)
	}
	if _, err := os.Stat(statePath(target)); !os.IsNotExist(err) {
		t.Error("the sidecar should be gone once the download finished")
	}
}

func TestDownloadHLSDecryptsAES128(t *testing.T) {
	segs := makeSegments(8)
	s := newHLSServer(t, segs)
	s.key = []byte("0123456789abcdef")

	dir := t.TempDir()
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("the decrypted file does not match the plaintext segments")
	}
	// One key for the whole playlist means one request for it, not one per
	// segment.
	if n := s.hitCount("/key.bin"); n != 1 {
		t.Errorf("the key was fetched %d times, want 1", n)
	}
}

func TestDownloadHLSFollowsTheMasterPlaylist(t *testing.T) {
	segs := makeSegments(5)
	s := newHLSServer(t, segs)
	dir := t.TempDir()

	var start HLSStart
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/master.m3u8", OutDir: dir, Filename: "clip.ts",
		Connections: 3, Variant: -1,
		OnStart: func(si HLSStart) { start = si },
	})
	if err != nil {
		t.Fatal(err)
	}
	if start.Chosen != 2 || len(start.Variants) != 3 {
		t.Errorf("chose variant %d of %d, want the best of 3", start.Chosen, len(start.Variants))
	}
	if start.Segments != 5 || start.Duration != 20 {
		t.Errorf("start info = %d segments / %v s", start.Segments, start.Duration)
	}
	if n := s.hitCount("/media2.m3u8"); n != 1 {
		t.Errorf("the 1080p media playlist was fetched %d times, want 1", n)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("content mismatch")
	}
}

func TestDownloadHLSResumesWithoutRefetching(t *testing.T) {
	segs := makeSegments(10)
	s := newHLSServer(t, segs)
	s.failAt[6] = http.StatusForbidden // not retryable, so it gives up quickly

	dir := t.TempDir()
	opts := HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts",
		Connections: 2, MaxRetries: 1,
	}
	target, err := DownloadHLS(context.Background(), opts)
	if err == nil {
		t.Fatal("the download should have failed on segment 7")
	}
	st, ok := loadHLSState(target)
	if !ok {
		t.Fatal("no sidecar was written, so there is nothing to resume from")
	}
	if st.Next == 0 || st.Next > 6 {
		t.Fatalf("sidecar stopped at segment %d, want somewhere in 1..6", st.Next)
	}
	partial, _ := os.ReadFile(target)
	if !bytes.Equal(partial, joined(segs[:st.Next])) {
		t.Fatal("the partial file is not a clean prefix of the stream")
	}

	s.mu.Lock()
	delete(s.failAt, 6)
	s.mu.Unlock()

	target2, err := DownloadHLS(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if target2 != target {
		t.Fatalf("resumed into %s instead of continuing %s", target2, target)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("the resumed file does not match the full stream")
	}
	if n := s.hitCount("/seg0.ts"); n != 1 {
		t.Errorf("segment 0 was fetched %d times; resuming should not start over", n)
	}
}

func TestDownloadHLSWritesTheInitSegmentExactlyOnce(t *testing.T) {
	segs := makeSegments(6)
	s := newHLSServer(t, segs)
	s.init = []byte("FTYPISO6MOOVBOXHERE")
	s.failAt[3] = http.StatusForbidden

	dir := t.TempDir()
	opts := HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir,
		Connections: 1, MaxRetries: 1,
	}
	target, err := DownloadHLS(context.Background(), opts)
	if err == nil {
		t.Fatal("expected the download to fail")
	}
	if filepath.Ext(target) != ".mp4" {
		t.Errorf("target = %s, an EXT-X-MAP stream is fragmented MP4, not TS", target)
	}

	s.mu.Lock()
	delete(s.failAt, 3)
	s.mu.Unlock()

	if _, err := DownloadHLS(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	want := append(append([]byte{}, s.init...), joined(segs)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("file is %d bytes, want %d: the init segment was probably written twice",
			len(got), len(want))
	}
}

func TestDownloadHLSNeverOverwritesSomeoneElsesFile(t *testing.T) {
	segs := makeSegments(4)
	s := newHLSServer(t, segs)
	dir := t.TempDir()

	existing := filepath.Join(dir, "clip.ts")
	if err := os.WriteFile(existing, []byte("a file that was already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if target == existing {
		t.Fatal("the existing file was overwritten")
	}
	if got, _ := os.ReadFile(existing); string(got) != "a file that was already here" {
		t.Error("the existing file was modified")
	}
	if filepath.Base(target) != "clip (1).ts" {
		t.Errorf("target = %s, want clip (1).ts", filepath.Base(target))
	}
}

func TestDownloadHLSReportsProgressBySegment(t *testing.T) {
	segs := makeSegments(10)
	s := newHLSServer(t, segs)
	for i := range segs {
		s.delay[i] = 40 * time.Millisecond
	}
	var mu sync.Mutex
	var last HLSProgress
	seen := 0
	_, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Connections: 2,
		OnProgress: func(p HLSProgress) {
			mu.Lock()
			defer mu.Unlock()
			if p.Segments < last.Segments {
				t.Errorf("progress went backwards: %d after %d", p.Segments, last.Segments)
			}
			last = p
			seen++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == 0 {
		t.Fatal("no progress was reported")
	}
	if last.Segments != 10 || last.TotalSegments != 10 {
		t.Errorf("final progress = %d/%d, want 10/10", last.Segments, last.TotalSegments)
	}
	if last.Bytes != int64(len(joined(segs))) {
		t.Errorf("final byte count = %d, want %d", last.Bytes, len(joined(segs)))
	}
}

func TestHLSRejectsExternalAudioBeforeDownloadingVideo(t *testing.T) {
	var mediaHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n"+
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",DEFAULT=YES,URI=\"audio.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f\",AUDIO=\"audio\"\n"+
				"video.m3u8\n")
			return
		}
		mediaHits.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()

	checks := []struct {
		name string
		run  func() error
	}{
		{"inspect", func() error {
			_, err := InspectHLS(context.Background(), HLSOptions{URL: srv.URL + "/master.m3u8"})
			return err
		}},
		{"download", func() error {
			_, err := DownloadHLS(context.Background(), HLSOptions{
				URL: srv.URL + "/master.m3u8", OutDir: t.TempDir(), Variant: -1,
			})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			var separate *SeparateAudioError
			if err := check.run(); !errors.As(err, &separate) {
				t.Fatalf("error = %v, want SeparateAudioError", err)
			}
		})
	}
	if got := mediaHits.Load(); got != 0 {
		t.Fatalf("fetched %d media playlists before rejecting separate audio", got)
	}
}

// ---------- routing through the task manager ----------

func TestManagerRoutesPlaylistsToTheHLSEngine(t *testing.T) {
	segs := makeSegments(6)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 2)

	id, err := m.Add(jobRequest{URL: s.URL + "/master.m3u8", Connections: 4, Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))

	v, ok := findTask(m, id)
	if !ok {
		t.Fatal("the task disappeared from the list")
	}
	if v.Kind != "hls" {
		t.Errorf("kind = %q, want hls: a .m3u8 URL must not go to the byte-range engine", v.Kind)
	}
	if v.SegDone != 6 || v.SegTotal != 6 {
		t.Errorf("segments = %d/%d, want 6/6", v.SegDone, v.SegTotal)
	}
	if v.Quality != "1080p · 5.2 Mbps" {
		t.Errorf("quality = %q, want the best variant from the master playlist", v.Quality)
	}
	if v.Duration != 24 {
		t.Errorf("duration = %v, want 24", v.Duration)
	}
	got, err := os.ReadFile(v.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("the saved file does not match the stream")
	}
}

// ---------- inspection ----------

func TestInspectListsQualitiesWithoutFetchingAnySegment(t *testing.T) {
	segs := makeSegments(7)
	s := newHLSServer(t, segs)

	info, err := InspectHLS(context.Background(), HLSOptions{URL: s.URL + "/master.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "hls" || info.Best != 2 || len(info.Variants) != 3 {
		t.Fatalf("info = %+v", info)
	}
	if info.Variants[0].Height != 360 || info.Variants[2].Label != "1080p · 5.2 Mbps" {
		t.Errorf("variants = %+v", info.Variants)
	}
	// Duration and segment count come from the quality that would actually be
	// downloaded, not from the master playlist, which carries neither.
	if info.Segments != 7 || info.Duration != 28 {
		t.Errorf("got %d segments / %v s, want 7 / 28", info.Segments, info.Duration)
	}
	for i := range segs {
		if n := s.hitCount(fmt.Sprintf("/seg%d.ts", i)); n != 0 {
			t.Fatalf("inspecting fetched segment %d; looking at a stream must not start downloading it", i)
		}
	}
}

func TestInspectReportsDRMAndLiveBeforeATaskIsCreated(t *testing.T) {
	drm := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://k",KEYFORMAT="com.apple.streamingkeydelivery"` +
		"\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	live := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n"

	for name, body := range map[string]string{"drm": drm, "live": live} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		_, err := InspectHLS(context.Background(), HLSOptions{URL: srv.URL + "/index.m3u8"})
		srv.Close()
		if err == nil {
			t.Errorf("%s: inspect accepted a stream it cannot download", name)
			continue
		}
		switch name {
		case "drm":
			if _, ok := err.(*DRMError); !ok {
				t.Errorf("drm: err = %v, want a DRMError", err)
			}
		case "live":
			if _, ok := err.(*LiveStreamError); !ok {
				t.Errorf("live: err = %v, want a LiveStreamError", err)
			}
		}
	}
}

// ---------- bugs found while reading it back ----------

func TestAnOversizedResponseFailsRatherThanTruncating(t *testing.T) {
	segs := makeSegments(2)
	s := newHLSServer(t, segs)
	restore := maxSegmentBytes
	maxSegmentBytes = 500 // the first segment is 1000 bytes
	defer func() { maxSegmentBytes = restore }()

	_, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Connections: 1, MaxRetries: 1,
	})
	if err == nil {
		t.Fatal("a response past the cap was accepted; silently keeping the first N bytes " +
			"produces a file that looks fine and plays wrong")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want it to say the response was too large", err)
	}
}

func TestAFailedStartLeavesNothingBehind(t *testing.T) {
	segs := makeSegments(3)
	s := newHLSServer(t, segs)
	s.failAt[0] = http.StatusForbidden

	dir := t.TempDir()
	opts := HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts",
		Connections: 1, MaxRetries: 1,
	}
	if _, err := DownloadHLS(context.Background(), opts); err == nil {
		t.Fatal("expected the download to fail")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("an empty file was left behind: %v", names)
	}

	s.mu.Lock()
	delete(s.failAt, 0)
	s.mu.Unlock()

	target, err := DownloadHLS(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(target) != "clip.ts" {
		t.Errorf("retried into %s; a failed attempt that wrote nothing must not burn the name",
			filepath.Base(target))
	}
}

// The same holds when it is the initialisation segment that cannot be had, and
// when the download is stopped while it is still being fetched.
func TestAFailedInitSegmentLeavesNothingBehind(t *testing.T) {
	segs := makeSegments(3)
	s := newHLSServer(t, segs)
	s.init = []byte("FTYPISO6MOOVBOXHERE")

	leftIn := func(dir string) []string {
		var names []string
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			names = append(names, e.Name())
		}
		return names
	}

	dir := t.TempDir()
	opts := HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip",
		Connections: 1, MaxRetries: 1,
	}

	s.initFail = http.StatusForbidden
	if _, err := DownloadHLS(context.Background(), opts); err == nil {
		t.Fatal("expected the download to fail")
	}
	if names := leftIn(dir); len(names) != 0 {
		t.Errorf("a refused init segment left %v behind", names)
	}

	s.initFail, s.initDelay = 0, 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := DownloadHLS(ctx, opts); err == nil {
		t.Fatal("expected the stopped download to fail")
	}
	if names := leftIn(dir); len(names) != 0 {
		t.Errorf("a download stopped during its init segment left %v behind", names)
	}

	s.initDelay = 0
	target, err := DownloadHLS(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(target) != "clip.mp4" {
		t.Errorf("finished as %s; failed attempts that wrote nothing must not burn the name",
			filepath.Base(target))
	}
	got, _ := os.ReadFile(target)
	if want := append(append([]byte{}, s.init...), joined(segs)...); !bytes.Equal(got, want) {
		t.Errorf("file is %d bytes, want %d", len(got), len(want))
	}
}

func TestRaisingTheConnectionCountAddsConnections(t *testing.T) {
	segs := makeSegments(48)
	s := newHLSServer(t, segs)
	for i := range segs {
		s.delay[i] = 60 * time.Millisecond // long enough to overlap
	}
	var limit atomic.Int32
	limit.Store(2)
	go func() {
		time.Sleep(400 * time.Millisecond)
		limit.Store(8)
	}()

	_, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts",
		Connections: 2, ConnLimit: func() int { return int(limit.Load()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak := s.peakConcurrency(); peak < 5 {
		t.Errorf("peak concurrency was %d; the connection count was raised to 8 part way "+
			"through and nothing picked it up", peak)
	}
}

// Lowering the count is as much a request as raising it: a server that is
// refusing connections is not helped by a client that keeps twice as many open
// as it was told to.
func TestLoweringTheConnectionCountBringsConnectionsDown(t *testing.T) {
	for _, c := range []struct{ from, to, segments int }{{8, 2, 170}, {8, 4, 260}, {4, 1, 90}} {
		t.Run(fmt.Sprintf("%d to %d", c.from, c.to), func(t *testing.T) {
			segs := makeSegments(c.segments)
			s := newHLSServer(t, segs)
			for i := range segs {
				s.delay[i] = 50 * time.Millisecond
			}
			var limit atomic.Int32
			limit.Store(int32(c.from))

			var mu sync.Mutex
			var late []int // Active as reported once the new limit has had time to take hold
			var settled atomic.Bool
			go func() {
				time.Sleep(300 * time.Millisecond)
				limit.Store(int32(c.to))
				time.Sleep(700 * time.Millisecond)
				s.resetPeak()
				settled.Store(true)
			}()

			_, err := DownloadHLS(context.Background(), HLSOptions{
				URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts",
				Connections: c.from, ConnLimit: func() int { return int(limit.Load()) },
				OnProgress: func(p HLSProgress) {
					if settled.Load() {
						mu.Lock()
						late = append(late, p.Active)
						mu.Unlock()
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !settled.Load() {
				t.Fatal("the download ended before the new limit could be measured")
			}
			if peak := s.peakConcurrency(); peak > c.to {
				t.Errorf("peak concurrency was %d after the limit went from %d to %d", peak, c.from, c.to)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, a := range late {
				if a > c.to {
					t.Errorf("progress reported %d active connections against a limit of %d", a, c.to)
					break
				}
			}
		})
	}
}

func TestProgressIsReportedWhileASegmentIsStillArriving(t *testing.T) {
	segs := makeSegments(3)
	s := newHLSServer(t, segs)
	s.delay[0] = 1500 * time.Millisecond

	var mu sync.Mutex
	first := time.Duration(-1)
	start := time.Now()
	_, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts", Connections: 2,
		OnProgress: func(p HLSProgress) {
			mu.Lock()
			if first < 0 {
				first = time.Since(start)
			}
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if first < 0 || first > time.Second {
		t.Errorf("the first progress report came after %v; while a slow segment is in flight "+
			"the display freezes and the speed shown goes stale", first)
	}
}

// ---------- more than one EXT-X-MAP ----------

const twoMapsPlaylist = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n" +
	"#EXT-X-MAP:URI=\"init-a.mp4\"\n#EXTINF:4,\na0.m4s\n#EXTINF:4,\na1.m4s\n" +
	"#EXT-X-DISCONTINUITY\n#EXT-X-MAP:URI=\"init-b.mp4\"\n#EXTINF:4,\nb0.m4s\n" +
	// Some packagers repeat the tag at every discontinuity; the same map again
	// is not a new one.
	"#EXT-X-DISCONTINUITY\n#EXT-X-MAP:URI=\"init-b.mp4\"\n#EXTINF:4,\nb1.m4s\n#EXT-X-ENDLIST\n"

func TestParseMediaKeepsTheMapEachSegmentIsPlayedWith(t *testing.T) {
	base, _ := url.Parse("https://x.test/a/index.m3u8")
	_, m, err := parsePlaylist([]byte(twoMapsPlaylist), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 4 {
		t.Fatalf("%d segments, want 4", len(m.Segments))
	}
	if m.Init == nil || m.Init.URI != "https://x.test/a/init-a.mp4" {
		t.Errorf("the file starts with %+v, want init-a", m.Init)
	}
	wantMap := []string{"init-a", "init-a", "init-b", "init-b"}
	for i, s := range m.Segments {
		if s.Init == nil || s.Init.URI != "https://x.test/a/"+wantMap[i]+".mp4" {
			t.Errorf("segment %d is played with %+v, want %s", i, s.Init, wantMap[i])
		}
	}
	for i, want := range []string{"", "", "https://x.test/a/init-b.mp4", ""} {
		got := ""
		if init := m.initBefore(i); init != nil {
			got = init.URI
		}
		if got != want {
			t.Errorf("init segment to write before segment %d = %q, want %q", i, got, want)
		}
	}
}

func TestDownloadHLSWritesEachInitSegmentWhereItApplies(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	failB0 := true
	body := map[string][]byte{
		"/init-a.mp4": []byte("INIT-A"), "/init-b.mp4": []byte("INIT-B"),
		"/a0.m4s": bytes.Repeat([]byte("a"), 100), "/a1.m4s": bytes.Repeat([]byte("A"), 120),
		"/b0.m4s": bytes.Repeat([]byte("b"), 140), "/b1.m4s": bytes.Repeat([]byte("B"), 160),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		fail := r.URL.Path == "/b0.m4s" && failB0
		mu.Unlock()
		switch {
		case r.URL.Path == "/index.m3u8":
			fmt.Fprint(w, twoMapsPlaylist)
		case fail:
			http.Error(w, "no", http.StatusForbidden)
		case body[r.URL.Path] != nil:
			w.Write(body[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	want := bytes.Join([][]byte{
		body["/init-a.mp4"], body["/a0.m4s"], body["/a1.m4s"],
		body["/init-b.mp4"], body["/b0.m4s"], body["/b1.m4s"],
	}, nil)
	opts := HLSOptions{URL: srv.URL + "/index.m3u8", OutDir: t.TempDir(), Filename: "clip", Connections: 1, MaxRetries: 1}

	// The first attempt dies at the segment that needs the second map, so the
	// second one resumes from a file that stops right before it.
	target, err := DownloadHLS(context.Background(), opts)
	if err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	mu.Lock()
	failB0 = false
	mu.Unlock()
	target2, err := DownloadHLS(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if target2 != target {
		t.Fatalf("resumed into %s, not %s", target2, target)
	}
	if filepath.Ext(target) != ".mp4" {
		t.Errorf("target = %s, want fragmented MP4", target)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, want) {
		t.Errorf("file is %d bytes, want %d: each init segment belongs in front of the segments it "+
			"applies to\n got %q...\nwant %q...", len(got), len(want), firstBytes(got), firstBytes(want))
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/init-a.mp4"] != 1 || hits["/init-b.mp4"] < 1 || hits["/init-b.mp4"] > 2 {
		t.Errorf("init segments fetched %d and %d times", hits["/init-a.mp4"], hits["/init-b.mp4"])
	}
}

func firstBytes(b []byte) string {
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

// ---------- naming ----------

// The extension sends the page title as the file name. A dot in a title is
// punctuation, not an extension, and a slash in one is part of the title, not
// the end of a folder.
func TestHLSNameKeepsTheWholeTitleAndAddsTheContainer(t *testing.T) {
	ts, mp4 := &hlsMedia{}, &hlsMedia{Init: &hlsSegment{}}
	for _, c := range []struct {
		title string
		media *hlsMedia
		want  string
	}{
		{"plain title", ts, "plain title.ts"},
		{"Mr. Robot S01E01", ts, "Mr. Robot S01E01.ts"},
		{"Node.js Crash Course - YouTube", ts, "Node.js Crash Course - YouTube.ts"},
		{"Episode 1.5", mp4, "Episode 1.5.mp4"},
		{"What happened next...", ts, "What happened next.ts"},
		{"AC/DC - Thunderstruck (Live)", ts, "AC_DC - Thunderstruck (Live).ts"},
		{"Fate/stay night 01", ts, "Fate_stay night 01.ts"},
		{`back\slash`, ts, "back_slash.ts"},
		{"../../etc/passwd", ts, "_.._etc_passwd.ts"},
		{"clip.ts", ts, "clip.ts"},
		{"clip.MP4", ts, "clip.MP4"},
		{"clip.mkv", mp4, "clip.mkv"},
		{"clip", mp4, "clip.mp4"},
		{"clip.mp4", mp4, "clip.mp4"},
		{"...", ts, "video.ts"},
		{"/", mp4, "_.mp4"},
		{"notes.godm", ts, "notes.godm.ts"},
		{"CON", ts, "_CON.ts"},
	} {
		o := HLSOptions{URL: "https://x.test/a/index.m3u8", Filename: c.title}
		got := hlsName(o, c.media)
		if got != c.want {
			t.Errorf("title %q -> %q, want %q", c.title, got, c.want)
		}
		if filepath.Base(filepath.Join("dir", got)) != got || isStateName(got) {
			t.Errorf("title %q -> %q escapes the folder or reads as a sidecar", c.title, got)
		}
		// A resumed task is handed its file's name back and must get it back.
		o.Filename = got
		if again := hlsName(o, c.media); again != got {
			t.Errorf("title %q: name %q came out as %q the second time", c.title, got, again)
		}
	}

	long := strings.Repeat("long title. ", 30)
	o := HLSOptions{URL: "https://x.test/a/index.m3u8", Filename: long}
	got := hlsName(o, ts)
	if !strings.HasSuffix(got, ".ts") || len(got) > 180 {
		t.Errorf("a long title came out as %d bytes, %q", len(got), got)
	}
	o.Filename = got
	if again := hlsName(o, ts); again != got {
		t.Errorf("a long name came out as %q the second time", again)
	}
}

// ---------- redirects ----------

// A playlist that was redirected is read from where it ended up: that is the
// address its relative entries mean, and it is the one every player resolves
// them against.
func TestRelativeURIsFollowTheRedirectedPlaylist(t *testing.T) {
	segs := makeSegments(3)
	s := newHLSServer(t, segs)

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch/123/index.m3u8":
			http.Redirect(w, r, s.URL+"/media.m3u8", http.StatusFound)
		case "/watch/master.m3u8":
			http.Redirect(w, r, s.URL+"/master.m3u8", http.StatusFound)
		case "/plain-master.m3u8":
			// A master that is served in place but names a variant that redirects.
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=640x360\nvariant/media.m3u8\n")
		case "/variant/media.m3u8":
			http.Redirect(w, r, s.URL+"/media.m3u8", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(front.Close)

	for name, path := range map[string]string{
		"a media playlist":                  "/watch/123/index.m3u8",
		"a master playlist":                 "/watch/master.m3u8",
		"a variant of a master that is not": "/plain-master.m3u8",
	} {
		t.Run(name, func(t *testing.T) {
			target, err := DownloadHLS(context.Background(), HLSOptions{
				URL: front.URL + path, OutDir: t.TempDir(), Filename: "clip.ts",
				Connections: 1, MaxRetries: 1, Variant: -1,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(target)
			if !bytes.Equal(got, joined(segs)) {
				t.Fatalf("file is %d bytes, want %d", len(got), len(joined(segs)))
			}
			if path == "/watch/123/index.m3u8" {
				return // nothing in a media playlist is fetched by an inspection
			}
			info, err := InspectHLS(context.Background(), HLSOptions{URL: front.URL + path, MaxRetries: 1})
			if err != nil {
				t.Fatal(err)
			}
			if info.Segments != len(segs) {
				t.Errorf("inspection saw %d segments, want %d: the variant was looked for beside the "+
					"address that was asked for", info.Segments, len(segs))
			}
		})
	}
}

// ---------- flushing the data before the sidecar that describes it ----------

// flushWatch stands in for syncData and keeps what the file held at the last
// flush that completed, which is all a power cut would leave of it.
type flushWatch struct {
	mu      sync.Mutex
	flushed []byte
	flushes int
}

func (w *flushWatch) install(t *testing.T, every time.Duration) {
	t.Helper()
	oldSync, oldEvery := syncData, dataSyncEvery
	t.Cleanup(func() { syncData, dataSyncEvery = oldSync, oldEvery })
	dataSyncEvery = every
	syncData = func(f *os.File) error {
		err := f.Sync()
		b, _ := os.ReadFile(f.Name())
		w.mu.Lock()
		w.flushed, w.flushes = b, w.flushes+1
		w.mu.Unlock()
		// A flush takes time, and the segments are written on through it. What
		// goes in meanwhile is not on disk yet and must not be claimed.
		time.Sleep(40 * time.Millisecond)
		return err
	}
}

func TestTheHLSSidecarNeverClaimsMoreThanWasFlushed(t *testing.T) {
	var w flushWatch
	w.install(t, 500*time.Millisecond)

	segs := makeSegments(40)
	s := newHLSServer(t, segs)
	s.init = []byte("FTYPISO6MOOVBOXHERE")
	for i := range segs {
		s.delay[i] = 150 * time.Millisecond
	}
	want := append(append([]byte{}, s.init...), joined(segs)...)

	dir := t.TempDir()
	target := filepath.Join(dir, "clip.mp4")
	var claimed int64
	var reported atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, err := DownloadHLS(ctx, HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.mp4",
		Connections: 2, MaxRetries: 1,
		// This runs just before the run saves again, so the sidecar it reads
		// was saved a tick ago, and the last flush is no older.
		OnProgress: func(p HLSProgress) {
			reported.Store(int64(p.Segments))
			if p.Segments >= 24 {
				cancel()
			}
			st, ok := loadHLSState(target)
			if !ok {
				return
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			claimed = st.Written
			if st.Written > int64(len(w.flushed)) || !bytes.Equal(w.flushed[:st.Written], want[:st.Written]) {
				t.Errorf("the sidecar says %d bytes are in; after the last flush the file holds %d bytes there that match",
					st.Written, matching(w.flushed, want, 0, st.Written))
				cancel()
			}
		},
	})
	if err == nil {
		t.Fatal("the download was not interrupted")
	}
	elapsed := time.Since(start)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushes < 2 || claimed == 0 {
		t.Fatalf("%d flushes and %d bytes claimed: the test looked at nothing", w.flushes, claimed)
	}
	// The flush is rationed: one per interval, not one per tick of the
	// display, so throughput does not wait on the disk. The init segment and
	// the stop each flush once more.
	if max := int(elapsed/dataSyncEvery) + 3; w.flushes > max {
		t.Errorf("%d flushes in %v with one due every %v: the file is being flushed on every tick", w.flushes, elapsed, dataSyncEvery)
	}

	// A stopped download keeps all that it got: the lag is for the sidecar of
	// a running one, and ends when it stops.
	st, ok := loadHLSState(target)
	if !ok {
		t.Fatal("no sidecar after the interruption")
	}
	if int64(st.Next) < reported.Load() {
		t.Errorf("the sidecar after stopping says %d segments, %d were reported done", st.Next, reported.Load())
	}
	onDisk, _ := os.ReadFile(target)
	if int64(len(w.flushed)) < st.Written || !bytes.Equal(w.flushed[:st.Written], want[:st.Written]) {
		t.Errorf("the sidecar after stopping says %d bytes are in, and the last flush holds only %d of them", st.Written, len(w.flushed))
	}
	if st.Written != int64(len(onDisk)) {
		t.Errorf("the sidecar after stopping says %d bytes, the file is %d", st.Written, len(onDisk))
	}

	// And it resumes from there to the whole stream.
	s.mu.Lock()
	for i := range segs {
		delete(s.delay, i)
	}
	s.mu.Unlock()
	if _, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.mp4", Connections: 2, MaxRetries: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, want) {
		t.Fatalf("the resumed file is %d bytes, want %d, or differs", len(got), len(want))
	}
}

func TestAFailedFlushDoesNotStopAnHLSDownload(t *testing.T) {
	oldSync, oldEvery := syncData, dataSyncEvery
	defer func() { syncData, dataSyncEvery = oldSync, oldEvery }()
	dataSyncEvery = time.Millisecond
	syncData = func(*os.File) error { return errors.New("the disk would not flush") }

	segs := makeSegments(8)
	s := newHLSServer(t, segs)
	for i := range segs {
		s.delay[i] = 100 * time.Millisecond
	}
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts", Connections: 2, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, joined(segs)) {
		t.Fatalf("the file is %d bytes, want %d", len(got), len(joined(segs)))
	}
	if _, ok := loadHLSState(target); ok {
		t.Error("the sidecar was left behind after a finished download")
	}
}
