package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

	mu     sync.Mutex
	hits   map[string]int
	failAt map[int]int           // segment index -> status code to answer with
	delay  map[int]time.Duration // segment index -> how long to stall
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
		s.mu.Unlock()
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
