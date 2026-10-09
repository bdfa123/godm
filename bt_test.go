package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"go.etcd.io/bbolt"
	"golang.org/x/time/rate"
)

// Every torrent test stays on this machine: the clients listen on 127.0.0.1
// only and find each other because the test names the peer, so no DHT, no
// trackers and no port mapping are ever involved.
func btLoopback(cfg *torrent.ClientConfig) {
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.DisableUTP = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
}

func newBTManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	m.bt.dir = t.TempDir()
	m.bt.configure = btLoopback
	t.Cleanup(m.shutdownBT)
	return m
}

type tfile struct {
	path string // inside the torrent's folder; empty for a single-file torrent
	data []byte
}

// makeTorrent writes the files under dir/name and builds a torrent of them.
func makeTorrent(t *testing.T, dir, name string, pieceLen int64, files ...tfile) *metainfo.MetaInfo {
	t.Helper()
	root := filepath.Join(dir, name)
	for _, f := range files {
		p := root
		if f.path != "" {
			p = filepath.Join(root, filepath.FromSlash(f.path))
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: pieceLen}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{}
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	return mi
}

func magnetOf(mi *metainfo.MetaInfo, name string, peers ...string) string {
	link := "magnet:?xt=urn:btih:" + mi.HashInfoBytes().HexString() + "&dn=" + url.QueryEscape(name)
	for _, p := range peers {
		link += "&x.pe=" + p
	}
	return link
}

// peer is another BitTorrent client on loopback.
type peer struct {
	cl *torrent.Client
	t  *torrent.Torrent
}

func (p *peer) addr() string { return p.cl.ListenAddrs()[0].String() }

func (p *peer) uploaded() int64 {
	s := p.t.Stats()
	return s.BytesWrittenData.Int64()
}

// newPeer starts a client holding mi with its data in dir. upload caps what it
// sends, in bytes per second; zero is no cap.
func newPeer(t *testing.T, dir string, mi *metainfo.MetaInfo, upload int) *peer {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := torrent.NewDefaultClientConfig()
	btLoopback(cfg)
	cfg.Seed = true
	cfg.DataDir = dir
	cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
		Logger:          quiet,
	})
	cfg.Slogger = quiet
	if upload > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(upload), 16<<10)
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	// A torrent wants nothing, and so connects to nobody, until asked for its
	// pieces.
	tor.DownloadAll()
	return &peer{cl: cl, t: tor}
}

// newSeeder is a peer that has checked it holds the whole torrent.
func newSeeder(t *testing.T, dir string, mi *metainfo.MetaInfo, upload int) *peer {
	t.Helper()
	p := newPeer(t, dir, mi, upload)
	waitFor(t, 20*time.Second, "the seeder to verify its data", p.t.Complete().Bool)
	return p
}

func taskOf(m *Manager, id string) *managedTask {
	return m.get(id)
}

// waitLoaded waits for a task's torrent to be in godm's client.
func waitLoaded(t *testing.T, m *Manager, id string) *torrent.Torrent {
	t.Helper()
	var tor *torrent.Torrent
	waitFor(t, 10*time.Second, "the torrent to be added to the client", func() bool {
		if mt := taskOf(m, id); mt != nil {
			tor = m.bt.loadedFor(mt)
		}
		return tor != nil
	})
	return tor
}

func btAPI(t *testing.T, m *Manager) (*server, *httptest.Server) {
	s := &server{mgr: m, token: "tok", port: 16801}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/torrent", s.guard(post(s.handleTorrentFile)))
	mux.HandleFunc("/api/seeding", s.guard(s.handleSeeding))
	mux.HandleFunc("/stream/", s.guard(s.handleStream))
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	return s, api
}

func bearer(method, target string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer tok")
	return req
}

func TestTellingTorrentsFromFiles(t *testing.T) {
	const ih = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		req  jobRequest
		want bool
	}{
		{jobRequest{URL: "magnet:?xt=urn:btih:" + ih}, true},
		{jobRequest{URL: "MAGNET:?xt=urn:btih:" + ih}, true},
		{jobRequest{URL: "https://example.com/files/ubuntu.iso.torrent"}, true},
		{jobRequest{URL: "https://example.com/files/ubuntu.iso.torrent?sig=1"}, true},
		{jobRequest{URL: "https://example.com/files/ubuntu.iso"}, false},
		{jobRequest{URL: "https://example.com/get?file=x.torrent"}, false}, // the name is in the query, not the path
		{jobRequest{URL: "https://example.com/stream.m3u8", Kind: "bt"}, true},
		{jobRequest{URL: "https://example.com/a.torrent", Kind: "hls"}, false},
	}
	for _, c := range cases {
		if got := isBTJob(c.req); got != c.want {
			t.Errorf("isBTJob(%q, kind %q) = %v, want %v", c.req.URL, c.req.Kind, got, c.want)
		}
	}

	m := NewManager(t.TempDir(), 1)
	m.mu.Lock()
	m.running = m.limit // nothing may start: this test has no client
	m.mu.Unlock()
	if _, err := m.Add(jobRequest{URL: "magnet:?dn=no-hash"}); err == nil {
		t.Error("a magnet link without an info hash was accepted")
	}
	if _, err := m.Add(jobRequest{URL: "magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32)}); err == nil {
		t.Error("a v2-only magnet link was accepted; the client cannot add it")
	}
	id, err := m.Add(jobRequest{URL: "magnet:?xt=urn:btih:" + ih + "&dn=Some%20Film"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := findTask(m, id)
	if v.Kind != "bt" || v.Filename != "Some Film" {
		t.Errorf("queued magnet shows kind %q name %q, want bt and the name it carries", v.Kind, v.Filename)
	}
	// The same torrent named twice, even with a different name, is one task.
	again, err := m.Add(jobRequest{URL: "magnet:?xt=urn:btih:" + strings.ToUpper(ih) + "&dn=other"})
	if err != nil || again != id {
		t.Errorf("second add of the same torrent = %q, %v; want the existing %q", again, err, id)
	}
	m.Pause(id)
	waitFor(t, 5*time.Second, "the pause", stateIs(m, id, StatePaused))
	// A torrent is found by its info hash: there is no link to expire or swap,
	// however much of it is on disk.
	mt := taskOf(m, id)
	mt.mu.Lock()
	mt.view.Path, mt.view.Size, mt.view.Received = filepath.Join(m.OutDir(), "Some Film"), 1000, 500
	mt.mu.Unlock()
	if _, err := m.RequestRefresh(id); err == nil {
		t.Error("a torrent was parked waiting for a refreshed link")
	}
	if err := m.ChangeAddress(id, "https://example.com/other.iso"); err == nil {
		t.Error("a torrent's address was changed to an HTTP link")
	}

	// The extension's right-click item sends a magnet down the same path as
	// any other link: a native message, then /api/download.
	const other = "89abcdef0123456789abcdef0123456789abcdef"
	nr := nativeRequest{Type: "download", URL: "magnet:?xt=urn:btih:" + other, Kind: "bt", Referrer: "https://x.example/"}
	b, _ := json.Marshal(nr.job(nr.URL, "", ""))
	rec := httptest.NewRecorder()
	(&server{mgr: m}).handleDownload(rec, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(b)))
	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if v, ok := findTask(m, out.ID); !out.OK || !ok || v.Kind != "bt" {
		t.Fatalf("magnet from the extension: %d %s, task %+v", rec.Code, rec.Body.String(), v)
	}
	m.Pause(out.ID)
}

func TestTorrentPathsStayInsideTheirFolder(t *testing.T) {
	fi := metainfo.FileInfo{Path: []string{"..", "evil:name?.mkv"}}
	got := torrentRelPath(&fi)
	if len(got) != 2 || got[0] == ".." || strings.ContainsAny(got[1], `:?/\`) {
		t.Errorf("torrentRelPath = %q", got)
	}

	info := &metainfo.Info{Name: "Film", PieceLength: 1 << 10, Files: []metainfo.FileInfo{
		{Path: []string{"Sample", "sample.mkv"}, Length: 50},
		{Path: []string{".pad", "1000"}, Length: 5000, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
		{Path: []string{"Film.mkv"}, Length: 900},
		{Path: []string{"soundtrack.flac"}, Length: 4000},
		{Path: []string{"film.nfo"}, Length: 10},
	}}
	if i, ok := torrentMediaIndex(info); !ok || i != 2 {
		t.Errorf("media index = %d, %v; want the film, not the sample, the soundtrack or the padding", i, ok)
	}
	info.Files = []metainfo.FileInfo{{Path: []string{"a.mp3"}, Length: 5}, {Path: []string{"b.flac"}, Length: 9}}
	if i, ok := torrentMediaIndex(info); !ok || i != 1 {
		t.Errorf("media index = %d, %v; want the larger audio file when there is no video", i, ok)
	}
}

func TestTorrentFilesThatShareANameOnNTFSGetTheirOwn(t *testing.T) {
	for _, c := range []struct {
		in   [][]string
		want []string
	}{
		{[][]string{{"a.mkv"}, {"A.mkv"}}, []string{"a.mkv", "A (1).mkv"}},
		{[][]string{{"a?.mkv"}, {"a*.mkv"}}, []string{"a_.mkv", "a_ (1).mkv"}},
		// One folder in two cases is one folder; only a file in both is renamed.
		{[][]string{{"Sub", "x.txt"}, {"sub", "x.txt"}, {"sub", "y.txt"}}, []string{"Sub/x.txt", "Sub/x (1).txt", "Sub/y.txt"}},
		{[][]string{{"x"}, {"X", "y"}}, []string{"x", "X (1)/y"}},
		{[][]string{{"d", "f"}, {"D"}}, []string{"d/f", "D (1)"}},
		{[][]string{{"a (1).mkv"}, {"a.mkv"}, {"A.mkv"}}, []string{"a (1).mkv", "a.mkv", "A (2).mkv"}},
	} {
		info := &metainfo.Info{Name: "T", PieceLength: 1 << 10}
		for _, p := range c.in {
			info.Files = append(info.Files, metainfo.FileInfo{Path: p, Length: 1})
		}
		var got []string
		for _, rel := range torrentFilePaths(info) {
			got = append(got, strings.Join(rel, "/"))
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q placed as %q, want %q", c.in, got, c.want)
		}
	}
	single := &metainfo.Info{Name: "film.mkv", Length: 5, PieceLength: 1 << 10}
	if got := torrentFilePaths(single); len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("a single-file torrent placed as %q, want the root itself", got)
	}

	// Two files the torrent tells apart only by case: each must keep its own
	// bytes, and playing and deleting must find the renamed one.
	src := t.TempDir()
	small, large := makePayload(100<<10), makePayload(150<<10)
	for i := range large {
		large[i] ^= 0xff
	}
	os.WriteFile(filepath.Join(src, "0.bin"), small, 0o644)
	os.WriteFile(filepath.Join(src, "1.bin"), large, 0o644)
	info := metainfo.Info{Name: "Pair", PieceLength: 32 << 10, Files: []metainfo.FileInfo{
		{Path: []string{"a.mkv"}, Length: int64(len(small))},
		{Path: []string{"A.mkv"}, Length: int64(len(large))},
	}}
	// The seeder keeps them apart on its own disk under other names.
	onDisk := func(fi *metainfo.FileInfo) string {
		if fi.Path[0] == "a.mkv" {
			return "0.bin"
		}
		return "1.bin"
	}
	if err := info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
		return os.Open(filepath.Join(src, onDisk(&fi)))
	}); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{}
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := torrent.NewDefaultClientConfig()
	btLoopback(cfg)
	cfg.Seed = true
	cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   src,
		FilePathMaker:   func(o storage.FilePathMakerOpts) string { return onDisk(o.File) },
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
		Logger:          quiet,
	})
	cfg.Slogger = quiet
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	seeding, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	seeding.DownloadAll()
	waitFor(t, 20*time.Second, "the seeder to verify its data", seeding.Complete().Bool)

	m := newBTManager(t)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "Pair", cl.ListenAddrs()[0].String())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	for name, want := range map[string][]byte{"a.mkv": small, "A (1).mkv": large} {
		got, err := os.ReadFile(filepath.Join(v.Path, name))
		if err != nil || sum(got) != sum(want) {
			t.Errorf("%s differs from the seeded file (%v)", name, err)
		}
	}
	if v.Media != "Pair/A (1).mkv" {
		t.Errorf("media %q, want the larger file under its new name", v.Media)
	}
	if !m.Remove(id, true) {
		t.Fatal("remove refused")
	}
	waitFor(t, 10*time.Second, "the torrent's folder to go", func() bool { return !pathExists(v.Path) })
}

func TestTorrentFilesAreWrittenWithPlainFileIO(t *testing.T) {
	// The setting is godm's, not something yt-dlp, ffmpeg or a player
	// should inherit.
	if v, ok := os.LookupEnv("TORRENT_STORAGE_DEFAULT_FILE_IO"); ok {
		t.Errorf("TORRENT_STORAGE_DEFAULT_FILE_IO=%s is still set; every program godm starts inherits it", v)
	}

	// Mapped files would show in two ways: the storage sizes a file in full
	// the moment it first writes to it, and on Windows the file cannot be
	// deleted afterwards, because the mapping outlives the torrent.
	const chunk = 16 << 10
	info := &metainfo.Info{Name: "film.bin", Length: 2 << 20, PieceLength: 1 << 20, Pieces: make([]byte, 2*20)}
	mt := &managedTask{outDir: t.TempDir()}
	pc, err := openBTCompletion(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	st := &btStorage{mt: mt, pc: pc, slog: slog.New(slog.NewTextHandler(io.Discard, nil)), failed: make(chan error, 1)}
	impl, err := st.OpenTorrent(t.Context(), info, metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := impl.Piece(info.Piece(0)).WriteAt(make([]byte, chunk), 0); err != nil {
		t.Fatal(err)
	}
	p := mt.snapshot().Path
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != chunk {
		t.Errorf("one chunk written made the file %d bytes, want %d: the storage is mapping files, not using plain file IO", fi.Size(), chunk)
	}
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Errorf("a file the torrent has let go of cannot be deleted: %v", err)
	}
}

func TestTheLogKeepsHashFailuresButNotEveryGoodPiece(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(quietHashes{slog.NewTextHandler(&buf, nil)}).With("torrent", "x")
	l.Warn("finished hashing piece", "piece", 1, "correct", true, "err", io.ErrShortWrite)
	if buf.Len() != 0 {
		t.Fatalf("a correct piece was logged: %s", buf.String())
	}
	l.Warn("finished hashing piece", "piece", 2, "correct", false, "err", io.ErrUnexpectedEOF)
	l.Warn("something else", "correct", true)
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Fatalf("logged %d lines, want the failed piece and the other warning:\n%s", got, buf.String())
	}
}

func TestTorrentFileFromTheUIDownloadsIntoTheTaskFolder(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(3<<20 + 12345)
	mi := makeTorrent(t, src, "film.bin", 64<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	_, api := btAPI(t, m)
	out := t.TempDir()
	var body bytes.Buffer
	if err := mi.Write(&body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(bearer(http.MethodPost, api.URL+"/api/torrent?out_dir="+url.QueryEscape(out), &body))
	if err != nil {
		t.Fatal(err)
	}
	var added struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&added)
	resp.Body.Close()
	if !added.OK || added.ID == "" {
		t.Fatalf("upload: status %d, %+v", resp.StatusCode, added)
	}
	id := added.ID

	waitLoaded(t, m, id).AddClientPeer(seed.cl)
	waitFor(t, 30*time.Second, "the torrent to finish", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if v.Kind != "bt" || v.Path != filepath.Join(out, "film.bin") || v.Filename != "film.bin" {
		t.Errorf("finished task: kind %q path %q name %q", v.Kind, v.Path, v.Filename)
	}
	if v.Size != int64(len(payload)) || v.Received != v.Size {
		t.Errorf("finished task shows %d of %d bytes, want %d", v.Received, v.Size, len(payload))
	}
	got, _ := os.ReadFile(v.Path)
	if sum(got) != sum(payload) {
		t.Fatal("downloaded file differs from the seeded one")
	}

	for _, bad := range []struct {
		req  *http.Request
		want int
	}{
		{bearer(http.MethodPost, api.URL+"/api/torrent", strings.NewReader("<html>login</html>")), http.StatusBadRequest},
		{bearer(http.MethodGet, api.URL+"/api/torrent", nil), http.StatusMethodNotAllowed},
		{func() *http.Request {
			r, _ := http.NewRequest(http.MethodPost, api.URL+"/api/torrent", strings.NewReader("x"))
			return r
		}(), http.StatusUnauthorized},
	} {
		resp, err := http.DefaultClient.Do(bad.req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != bad.want {
			t.Errorf("%s %s: status %d, want %d", bad.req.Method, bad.req.URL.Path, resp.StatusCode, bad.want)
		}
	}
}

func TestMagnetFetchesMetadataThenDownloads(t *testing.T) {
	src := t.TempDir()
	track, cover := makePayload(1<<20+7), makePayload(300<<10)
	mi := makeTorrent(t, src, "Album", 32<<10,
		tfile{"disc 1/track.flac", track}, tfile{"cover.jpg", cover})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "Album")})
	if err != nil {
		t.Fatal(err)
	}
	// No peer is known yet, so the task can only wait for the metainfo.
	waitFor(t, 10*time.Second, "the metadata stage", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Stage == "metadata"
	})
	if v, _ := findTask(m, id); v.Kind != "bt" || v.Filename != "Album" || v.Path != "" {
		t.Errorf("waiting for metadata: kind %q name %q path %q", v.Kind, v.Filename, v.Path)
	}

	waitLoaded(t, m, id).AddClientPeer(seed.cl)
	waitFor(t, 30*time.Second, "the torrent to finish", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if want := filepath.Join(m.OutDir(), "Album"); v.Path != want {
		t.Errorf("path %q, want %q", v.Path, want)
	}
	if v.Media != "Album/disc 1/track.flac" {
		t.Errorf("media %q, want the track", v.Media)
	}
	for rel, want := range map[string][]byte{"disc 1/track.flac": track, "cover.jpg": cover} {
		got, err := os.ReadFile(filepath.Join(v.Path, filepath.FromSlash(rel)))
		if err != nil || sum(got) != sum(want) {
			t.Errorf("%s differs from the seeded file (%v)", rel, err)
		}
	}
	if _, err := os.Stat(m.bt.metainfoPath(mi.HashInfoBytes())); err != nil {
		t.Errorf("the metainfo that arrived was not kept: %v", err)
	}
}

func TestAMagnetNobodyAnswersGivesUpItsPlaceInTheQueue(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.bt.dir = t.TempDir()
	m.bt.configure = btLoopback
	m.bt.metadataWait = 2 * time.Second
	t.Cleanup(m.shutdownBT)

	// No peer is named and nothing else is reachable, so no metadata comes.
	const ih = "0123456789abcdef0123456789abcdef01234567"
	magnet, err := m.Add(jobRequest{URL: "magnet:?xt=urn:btih:" + ih + "&dn=nobody"})
	if err != nil {
		t.Fatal(err)
	}
	inMetadata := func() bool {
		v, _ := findTask(m, magnet)
		return v.State == StateRunning && v.Stage == "metadata"
	}
	waitFor(t, 10*time.Second, "the metadata stage", inMetadata)
	data := makePayload(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.bin", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	file, err := m.Add(jobRequest{URL: srv.URL + "/a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	// One download at a time, so the file can only start once the magnet
	// lets go of the slot.
	waitFor(t, 10*time.Second, "the file queued behind the magnet", stateIs(m, file, StateDone))
	v, _ := findTask(m, magnet)
	if v.State != StateError || !strings.Contains(v.Error, "no peers answered") || v.Stage != "" {
		t.Fatalf("the magnet gave up as state %s, stage %q, error %q", v.State, v.Stage, v.Error)
	}
	if m.bt.loadedFor(taskOf(m, magnet)) != nil {
		t.Error("the magnet that gave up is still in the client")
	}

	// It can be tried again, and paused or removed while it waits.
	if !m.Resume(magnet) {
		t.Fatal("resume refused")
	}
	waitFor(t, 10*time.Second, "the metadata stage again", inMetadata)
	m.Pause(magnet)
	waitFor(t, 5*time.Second, "the pause", stateIs(m, magnet, StatePaused))
	if v, _ := findTask(m, magnet); v.Stage != "" || v.Error != "" || m.bt.loadedFor(taskOf(m, magnet)) != nil {
		t.Errorf("paused while waiting: stage %q, error %q, still loaded %v", v.Stage, v.Error, m.bt.loadedFor(taskOf(m, magnet)) != nil)
	}
	m.Resume(magnet)
	waitFor(t, 10*time.Second, "the metadata stage once more", inMetadata)
	mt := taskOf(m, magnet)
	m.Remove(magnet, true)
	waitFor(t, 5*time.Second, "the removed magnet to leave the client", func() bool { return m.bt.loadedFor(mt) == nil })
}

func TestTorrentResumesAfterARestartWithoutRefetching(t *testing.T) {
	const pieceLen = 32 << 10
	src := t.TempDir()
	payload := makePayload(2 << 20)
	mi := makeTorrent(t, src, "film.mkv", pieceLen, tfile{data: payload})
	first := newSeeder(t, src, mi, 384<<10)

	cfgDir, out := t.TempDir(), t.TempDir()
	daemon := func() *Manager {
		m := NewManager(out, 2)
		m.announce = nil
		m.store = filepath.Join(cfgDir, "tasks.json")
		m.bt.dir = filepath.Join(cfgDir, "bt")
		m.bt.configure = btLoopback
		t.Cleanup(m.shutdownBT)
		return m
	}

	m1 := daemon()
	id, err := m1.Add(jobRequest{URL: magnetOf(mi, "film.mkv", first.addr())})
	if err != nil {
		t.Fatal(err)
	}
	running := waitLoaded(t, m1, id)
	waitFor(t, 20*time.Second, "part of the film", func() bool {
		s := running.Stats()
		return s.PiecesComplete >= 16
	})
	// Pieces only partly fetched at the pause are fetched again; whole ones
	// must not be. More may finish before the pause lands, never fewer.
	s := running.Stats()
	verified := int64(s.PiecesComplete) * pieceLen
	m1.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m1, id, StatePaused))
	before, _ := findTask(m1, id)
	if before.Received >= int64(len(payload)) {
		t.Fatal("the film finished before the restart; slow the seeder down")
	}
	if err := m1.save(); err != nil {
		t.Fatal(err)
	}
	m1.shutdownBT()
	// From here on nobody has the metainfo but godm's own copy of it.
	first.cl.Close()

	m2 := daemon()
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if v, ok := findTask(m2, id); !ok || v.State != StatePaused || v.Path != before.Path {
		t.Fatalf("restored task: %+v", v)
	}
	if !m2.Resume(id) {
		t.Fatal("resume refused")
	}
	tor := waitLoaded(t, m2, id)
	if tor.Info() == nil {
		t.Fatal("the torrent came back without its metainfo; it would wait for peers to send it again")
	}
	waitFor(t, 10*time.Second, "the pieces already on disk to be counted", func() bool {
		return tor.BytesCompleted() >= verified
	})
	onDisk := tor.BytesCompleted()

	second := newSeeder(t, src, mi, 0)
	tor.AddClientPeer(second.cl)
	waitFor(t, 30*time.Second, "the rest of the film", stateIs(m2, id, StateDone))
	v, _ := findTask(m2, id)
	got, _ := os.ReadFile(v.Path)
	if sum(got) != sum(payload) {
		t.Fatal("resumed film differs from the seeded one")
	}
	if sent, limit := second.uploaded(), int64(len(payload))-onDisk+4*pieceLen; sent > limit {
		t.Errorf("after the restart %d bytes were fetched again; only %d were missing", sent, int64(len(payload))-onDisk)
	}
}

func TestSeedingPolicyAfterTheDownloadCompletes(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(1 << 20)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})

	for _, c := range []struct {
		policy      SeedPolicy
		serves      bool // a newcomer can fetch the torrent from godm
		stillSeeded bool // and godm is still offering it afterwards
	}{
		{SeedStop, false, false},
		{SeedRatio, true, false},
		{SeedKeep, true, true},
	} {
		t.Run(string(c.policy), func(t *testing.T) {
			seed := newSeeder(t, src, mi, 0)
			m := newBTManager(t)
			if c.policy != SeedStop {
				m.SetSeedPolicy(c.policy)
			} else if got := m.SeedPolicy(); got != SeedStop {
				t.Fatalf("default policy %q, want stop", got)
			}
			id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
			// From here on godm holds the only complete copy.
			seed.cl.Close()
			sentBefore := m.bt.client().ConnStats()

			newcomer := newPeer(t, t.TempDir(), mi, 0)
			newcomer.t.AddClientPeer(m.bt.client())
			mt := taskOf(m, id)
			if !c.serves {
				time.Sleep(3 * time.Second)
				if n := newcomer.t.BytesCompleted(); n != 0 {
					t.Fatalf("godm uploaded %d bytes after the download completed", n)
				}
				sentAfter := m.bt.client().ConnStats()
				if a, b := sentAfter.BytesWrittenData.Int64(), sentBefore.BytesWrittenData.Int64(); a != b {
					t.Fatalf("godm sent %d bytes of data after the download completed", a-b)
				}
				if m.bt.loadedFor(mt) != nil {
					t.Fatal("the finished torrent is still in the client")
				}
				return
			}
			waitFor(t, 30*time.Second, "the newcomer to fetch the torrent from godm", newcomer.t.Complete().Bool)
			if c.stillSeeded {
				time.Sleep(1500 * time.Millisecond)
				v, _ := findTask(m, id)
				if m.bt.loadedFor(mt) == nil || v.Stage != "seeding" || v.Uploaded < int64(len(payload)) {
					t.Fatalf("keep seeding: loaded %v, stage %q, uploaded %d", m.bt.loadedFor(mt) != nil, v.Stage, v.Uploaded)
				}
				// Changing the policy reaches a torrent that is already seeding.
				m.SetSeedPolicy(SeedStop)
				waitFor(t, 3*time.Second, "seeding to stop with the policy", func() bool {
					v, _ := findTask(m, id)
					return m.bt.loadedFor(mt) == nil && v.Stage == ""
				})
				return
			}
			waitFor(t, 10*time.Second, "seeding to stop at ratio 1.0", func() bool {
				v, _ := findTask(m, id)
				return m.bt.loadedFor(mt) == nil && v.Stage == ""
			})
			if v, _ := findTask(m, id); v.Uploaded < int64(len(payload)) {
				t.Errorf("stopped at %d uploaded bytes, before ratio 1.0 (%d)", v.Uploaded, len(payload))
			}
		})
	}
}

func TestASeedingTorrentShowsWhoIsConnectedNow(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(2 << 20)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	m.bt.configure = func(cfg *torrent.ClientConfig) {
		btLoopback(cfg)
		// Two complete peers usually part at once. Here the seed stays,
		// as one does whose connection is still busy when the last piece
		// lands, so it is still counted when the download finishes.
		cfg.DropMutuallyCompletePeers = false
		// Slow enough that the newcomer below stays connected a while.
		cfg.UploadRateLimiter = rate.NewLimiter(256<<10, 32<<10)
	}
	m.SetSeedPolicy(SeedKeep)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Stage != "seeding" {
		t.Fatalf("stage %q after the download, want seeding", v.Stage)
	}
	waitFor(t, 5*time.Second, "the seed to be counted", func() bool {
		v, _ := findTask(m, id)
		return v.Active == 1 && v.Conns == 1
	})
	seed.cl.Close()
	waitFor(t, 5*time.Second, "the seed that left to be counted out", func() bool {
		v, _ := findTask(m, id)
		return v.Active == 0 && v.Conns == 0
	})
	newcomer := newPeer(t, t.TempDir(), mi, 0)
	newcomer.t.AddClientPeer(m.bt.client())
	waitFor(t, 5*time.Second, "the newcomer to be counted", func() bool {
		v, _ := findTask(m, id)
		return v.Active == 1 && v.Conns == 0
	})
	m.SetSeedPolicy(SeedStop)
	waitFor(t, 5*time.Second, "seeding to stop", func() bool {
		v, _ := findTask(m, id)
		return v.Stage == "" && v.Active == 0 && v.Conns == 0
	})
}

// serveTorrentFile puts a .torrent on a local web server, as a site would.
func serveTorrentFile(t *testing.T, mi *metainfo.MetaInfo) string {
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/files/film.torrent"
}

func TestTorrentLinkIsFetchedAndKept(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(700 << 10)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)
	link := serveTorrentFile(t, mi)

	m := newBTManager(t)
	id, err := m.Add(jobRequest{URL: link})
	if err != nil {
		t.Fatal(err)
	}
	waitLoaded(t, m, id).AddClientPeer(seed.cl)
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if got, _ := os.ReadFile(v.Path); sum(got) != sum(payload) {
		t.Fatal("downloaded file differs from the seeded one")
	}
	mt := taskOf(m, id)
	mt.mu.Lock()
	jobURL := mt.req.URL
	mt.mu.Unlock()
	// The list keeps the link the user gave; the job now names the torrent
	// itself, so a restart does not depend on that link still working.
	if v.URL != link || !strings.HasPrefix(jobURL, "magnet:?xt=urn:btih:"+mi.HashInfoBytes().HexString()) {
		t.Errorf("shown URL %q, job URL %q", v.URL, jobURL)
	}
	if _, err := os.Stat(m.bt.metainfoPath(mi.HashInfoBytes())); err != nil {
		t.Errorf("the fetched .torrent was not kept: %v", err)
	}
}

func TestATorrentRemovedAsItsFileArrivesKeepsNoCopy(t *testing.T) {
	src := t.TempDir()
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: makePayload(100 << 10)})
	link := serveTorrentFile(t, mi)
	m := newBTManager(t)
	m.mu.Lock()
	m.running = m.limit // the run is driven by hand below
	m.mu.Unlock()
	id, err := m.Add(jobRequest{URL: link})
	if err != nil {
		t.Fatal(err)
	}
	mt := taskOf(m, id)
	// The removal cleans up after the web link, which names no torrent yet.
	m.Remove(id, true)
	time.Sleep(200 * time.Millisecond)
	// Then the .torrent finishes arriving, as it would for a removal that came
	// after the last byte was read.
	if _, err := m.torrentSpec(t.Context(), mt); err == nil {
		t.Error("a removed task carried on as if it were still in the list")
	}
	if _, err := os.Stat(m.bt.metainfoPath(mi.HashInfoBytes())); !os.IsNotExist(err) {
		t.Errorf("the .torrent fetched for a removed task was kept: %v", err)
	}
}

func TestADuplicateTorrentLeavesTheOriginalAlone(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(512 << 10)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	first, _ := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	waitFor(t, 30*time.Second, "the download", stateIs(m, first, StateDone))
	// A link to the same torrent as a .torrent file only shows it is the same
	// once fetched.
	second, err := m.Add(jobRequest{URL: serveTorrentFile(t, mi)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the duplicate to be refused", stateIs(m, second, StateError))
	if v, _ := findTask(m, second); !strings.Contains(v.Error, "already in the list") {
		t.Errorf("duplicate failed with %q", v.Error)
	}
	m.Remove(second, true)
	time.Sleep(500 * time.Millisecond)
	v, _ := findTask(m, first)
	if _, err := os.Stat(m.bt.metainfoPath(mi.HashInfoBytes())); err != nil {
		t.Errorf("removing the duplicate took the original's metainfo: %v", err)
	}
	if got, _ := os.ReadFile(v.Path); sum(got) != sum(payload) {
		t.Error("removing the duplicate touched the original's file")
	}
}

func TestTorrentsKeepToTheOverallSpeedLimit(t *testing.T) {
	const limit = 256 << 10
	src := t.TempDir()
	payload := makePayload(3 << 20) // twelve seconds at the limit
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	m.SetSpeedLimit(limit) // before the client starts
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	tor := waitLoaded(t, m, id)
	waitFor(t, 10*time.Second, "the file list", func() bool { return tor.Info() != nil })
	start, before := time.Now(), tor.BytesCompleted()
	time.Sleep(3 * time.Second)
	got := tor.BytesCompleted() - before
	want := int64(time.Since(start).Seconds() * limit)
	if got < want*6/10 || got > want*14/10 {
		t.Errorf("under a limit of %d bytes/s, %d bytes arrived in %s; want about %d", limit, got, time.Since(start).Round(time.Millisecond), want)
	}

	// Lifting the limit reaches the torrent that is already running.
	m.SetSpeedLimit(0)
	lifted := time.Now()
	waitFor(t, 10*time.Second, "the rest without a limit", stateIs(m, id, StateDone))
	if d := time.Since(lifted); d > 5*time.Second {
		t.Errorf("the last %d bytes took %s with the limit lifted", int64(len(payload))-got-before, d.Round(time.Millisecond))
	}
	v, _ := findTask(m, id)
	if b, _ := os.ReadFile(v.Path); sum(b) != sum(payload) {
		t.Fatal("downloaded file differs from the seeded one")
	}
}

func TestSeedingPolicyEndpointAndPersistence(t *testing.T) {
	m := newBTManager(t)
	_, api := btAPI(t, m)
	call := func(method, query string, body io.Reader) (int, map[string]any) {
		resp, err := http.DefaultClient.Do(bearer(method, api.URL+"/api/seeding"+query, body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, out := call(http.MethodGet, "", nil); code != 200 || out["policy"] != "stop" {
		t.Fatalf("GET: %d %v, want the stop default", code, out)
	}
	if code, out := call(http.MethodPost, "?policy=ratio", nil); code != 200 || out["policy"] != "ratio" {
		t.Fatalf("POST ratio: %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "", strings.NewReader(`{"policy":"keep"}`)); code != 200 || out["policy"] != "keep" {
		t.Fatalf("POST keep as JSON: %d %v", code, out)
	}
	if code, _ := call(http.MethodPost, "?policy=forever", nil); code != http.StatusBadRequest {
		t.Fatalf("POST nonsense: %d, want 400", code)
	}
	if got := m.SeedPolicy(); got != SeedKeep {
		t.Fatalf("policy %q after a rejected change, want keep", got)
	}

	store := filepath.Join(t.TempDir(), "tasks.json")
	m.store = store
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(t.TempDir(), 1)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if got := m2.SeedPolicy(); got != SeedKeep {
		t.Fatalf("restored policy %q, want keep", got)
	}
}

func TestTorrentNeverOverwritesAnExistingFile(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(512 << 10)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	theirs := filepath.Join(m.OutDir(), "film.bin")
	if err := os.WriteFile(theirs, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if want := filepath.Join(m.OutDir(), "film (1).bin"); v.Path != want {
		t.Errorf("path %q, want %q", v.Path, want)
	}
	if got, _ := os.ReadFile(theirs); string(got) != "someone else's file" {
		t.Fatal("the torrent wrote into a file it did not create")
	}
	if got, _ := os.ReadFile(v.Path); sum(got) != sum(payload) {
		t.Fatal("downloaded file differs from the seeded one")
	}
}

func TestRemovingATorrentDeletesOnlyWhatItWrote(t *testing.T) {
	src := t.TempDir()
	mi := makeTorrent(t, src, "Show", 32<<10,
		tfile{"S01/e01.mkv", makePayload(400 << 10)}, tfile{"S01/e02.mkv", makePayload(300 << 10)})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	id, _ := m.Add(jobRequest{URL: magnetOf(mi, "Show", seed.addr())})
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	mine := filepath.Join(v.Path, "notes.txt")
	if err := os.WriteFile(mine, []byte("the user's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := taskOf(m, id)
	if !m.Remove(id, true) {
		t.Fatal("remove refused")
	}
	waitFor(t, 10*time.Second, "the torrent's files to go", func() bool {
		_, err := os.Stat(filepath.Join(v.Path, "S01"))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a file the torrent did not write was deleted: %v", err)
	}
	if _, err := os.Stat(m.bt.metainfoPath(mi.HashInfoBytes())); !os.IsNotExist(err) {
		t.Errorf("the kept metainfo outlived the task: %v", err)
	}
	if m.bt.loadedFor(mt) != nil {
		t.Error("the removed torrent is still in the client")
	}
}

// A finished torrent is played straight from disk, and Windows will not delete
// a file a player still has open, so removing it with its files must end the
// stream first.
func TestRemovingAFinishedTorrentEndsItsPlayerAndDeletesIt(t *testing.T) {
	src := t.TempDir()
	mi := makeTorrent(t, src, "Film", 256<<10, tfile{"Film.mkv", makePayload(32 << 20)})
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	_, api := btAPI(t, m)
	id, _ := m.Add(jobRequest{URL: magnetOf(mi, "Film", seed.addr())})
	waitFor(t, 60*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	film := filepath.Join(v.Path, "Film.mkv")

	// A player that read a little and then stopped: the rest of the response
	// sits in a write nobody reads, with the file open behind it.
	resp, err := http.DefaultClient.Do(bearer(http.MethodGet, api.URL+"/stream/"+id+"/Film.mkv", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}

	if !m.Remove(id, true) {
		t.Fatal("remove refused")
	}
	waitFor(t, 10*time.Second, "the film to be deleted", func() bool {
		_, err := os.Stat(film)
		return os.IsNotExist(err)
	})
}

func TestRemovingATorrentForgetsItsPieceRecords(t *testing.T) {
	// What the library's own bolt record wrote, as godm used it before, still
	// reads the same.
	old := t.TempDir()
	lib, err := storage.NewBoltPieceCompletion(old)
	if err != nil {
		t.Fatal(err)
	}
	key := metainfo.PieceKey{InfoHash: metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567"), Index: 7}
	if err := lib.Set(key, true); err != nil {
		t.Fatal(err)
	}
	other := key
	other.Index = 8
	lib.Set(other, false)
	lib.Close()
	pc, err := openBTCompletion(old)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := pc.Get(key); !c.Ok || !c.Complete {
		t.Errorf("a finished piece the library recorded reads as %+v", c)
	}
	if c, _ := pc.Get(other); !c.Ok || c.Complete {
		t.Errorf("an unfinished piece the library recorded reads as %+v", c)
	}
	pc.Close()

	src := t.TempDir()
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: makePayload(300 << 10)})
	seed := newSeeder(t, src, mi, 0)
	m := newBTManager(t)
	id, _ := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	ih := mi.HashInfoBytes()
	records := func() (n int) {
		m.bt.pc.db.View(func(tx *bbolt.Tx) error {
			if b := tx.Bucket(btCompletionBucket); b != nil {
				if b = b.Bucket(ih[:]); b != nil {
					n = b.Stats().KeyN
				}
			}
			return nil
		})
		return n
	}
	if n := records(); n == 0 {
		t.Fatal("the finished torrent has no piece records")
	}
	m.Remove(id, false)
	waitFor(t, 10*time.Second, "the removed torrent's piece records to go", func() bool { return records() == 0 })
	if c, _ := m.bt.pc.Get(metainfo.PieceKey{InfoHash: ih}); c.Ok {
		t.Errorf("a removed torrent's first piece still reads as %+v", c)
	}
}

func TestRemovingATorrentMidDownloadLeavesNoFiles(t *testing.T) {
	src := t.TempDir()
	var files []tfile
	for i := 1; i <= 6; i++ {
		files = append(files, tfile{fmt.Sprintf("S01/e%02d.mkv", i), makePayload(700 << 10)})
	}
	mi := makeTorrent(t, src, "Show", 64<<10, files...)
	seed := newSeeder(t, src, mi, 0)

	// The client writes chunks with its lock let go, so a removal in the
	// middle of a download nearly always lands while some are in flight.
	for i := 0; i < 5; i++ {
		m := newBTManager(t)
		id, err := m.Add(jobRequest{URL: magnetOf(mi, "Show", seed.addr())})
		if err != nil {
			t.Fatal(err)
		}
		tor := waitLoaded(t, m, id)
		waitFor(t, 20*time.Second, "some of the show", func() bool { return tor.BytesCompleted() > 1<<20 })
		v, _ := findTask(m, id)
		if !m.Remove(id, true) {
			t.Fatal("remove refused")
		}
		waitFor(t, 10*time.Second, fmt.Sprintf("run %d: the show's folder to go", i), func() bool { return !pathExists(v.Path) })
		// A write still in flight at the delete would bring a file back.
		time.Sleep(300 * time.Millisecond)
		if pathExists(v.Path) {
			t.Fatalf("run %d: %s came back after it was deleted", i, v.Path)
		}
		m.shutdownBT()
	}
}

func TestTorrentStreamServesARangeBeforeTheDownloadFinishes(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	old := launchPlayer
	launchPlayer = func(target, title string) error {
		mu.Lock()
		opened = append(opened, target)
		mu.Unlock()
		return nil
	}
	defer func() { launchPlayer = old }()

	src := t.TempDir()
	film := makePayload(4 << 20)
	mi := makeTorrent(t, src, "Film", 32<<10,
		tfile{"Film.mkv", film}, tfile{"Sample/sample.mkv", makePayload(1 << 20)}, tfile{"film.nfo", []byte("notes")})
	seed := newSeeder(t, src, mi, 512<<10) // about ten seconds for all of it

	m := newBTManager(t)
	s, api := btAPI(t, m)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "Film", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the file list", func() bool {
		v, _ := findTask(m, id)
		return v.Media != ""
	})
	if v, _ := findTask(m, id); v.Media != "Film/Film.mkv" {
		t.Fatalf("media %q, want the film rather than its sample", v.Media)
	}
	play := func() map[string]any {
		rec := httptest.NewRecorder()
		s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/api/play?id="+id, nil))
		var out map[string]any
		json.NewDecoder(rec.Body).Decode(&out)
		return out
	}
	if r := play(); r["ok"] != true {
		t.Fatalf("play a torrent in progress: %v", r)
	}

	const from, to = 3 << 20, 3<<20 + 256<<10 - 1
	req := bearer(http.MethodGet, api.URL+"/stream/"+id+"/Film.mkv", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", from, to, len(film)); got != want {
		t.Errorf("Content-Range %q, want %q", got, want)
	}
	if sum(body) != sum(film[from:to+1]) {
		t.Fatal("streamed bytes differ from the film")
	}
	if v, _ := findTask(m, id); v.State == StateDone {
		t.Fatal("the range only arrived once the whole torrent had: nothing was streamed")
	}

	waitFor(t, 40*time.Second, "completion", stateIs(m, id, StateDone))
	resp, err = http.DefaultClient.Do(bearer(http.MethodGet, api.URL+"/stream/"+id+"/Film.mkv", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || sum(body) != sum(film) {
		t.Fatalf("finished film: status %d, %d bytes", resp.StatusCode, len(body))
	}
	if r := play(); r["ok"] != true {
		t.Fatalf("play a finished torrent: %v", r)
	}

	v, _ := findTask(m, id)
	mu.Lock()
	defer mu.Unlock()
	if want := "http://127.0.0.1:16801/stream/" + id + "/Film.mkv?token=tok"; len(opened) != 2 || opened[0] != want {
		t.Fatalf("opened %q, want the stream link first", opened)
	}
	if want := filepath.Join(v.Path, "Film.mkv"); opened[1] != want {
		t.Errorf("finished torrent opened %q, want %q", opened[1], want)
	}
}

func TestTorrentStreamOnlyServesCheckedPieces(t *testing.T) {
	src := t.TempDir()
	film := makePayload(1 << 20)
	mi := makeTorrent(t, src, "film.mkv", 256<<10, tfile{data: film})
	seed := newSeeder(t, src, mi, 256<<10)
	// The seeder checked the real film, and its file storage reads the disk
	// afresh each time, so from here on it sends bytes that fail the piece
	// hash while believing it has the torrent: a bad peer, in short.
	bad := make([]byte, len(film))
	for i := range film {
		bad[i] = film[i] ^ 0x5a
	}
	if err := os.WriteFile(filepath.Join(src, "film.mkv"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	m := newBTManager(t)
	_, api := btAPI(t, m)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.mkv", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the file list", func() bool {
		v, _ := findTask(m, id)
		return v.Media != ""
	})
	const n = 64 << 10
	req := bearer(http.MethodGet, api.URL+"/stream/"+id+"/film.mkv", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	// The first piece never passes its check, so the right answer is no
	// answer: the player waits, as it would for a slow peer.
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) > 0 && bytes.Equal(body, bad[:len(body)]) {
			t.Fatalf("the stream served %d bytes of a piece that failed its hash check", len(body))
		}
	}
	if tor := m.bt.loadedFor(taskOf(m, id)); tor == nil || tor.Stats().PiecesComplete != 0 {
		t.Fatal("the bad first piece was not what the stream waited on")
	}
	m.Pause(id)
}
