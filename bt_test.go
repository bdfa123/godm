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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
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
	// This peer is the same library as godm's client, and can lose the same
	// wake-up: a chunk it has ready then waits for its writer's next look.
	cfg.KeepAliveTimeout = btPeerWake
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

// waitTorrentDone waits for a torrent task to finish. How long that takes says
// more about the machine than about the torrent: on GitHub's Windows runners,
// with the tests' files on the system disk, about one run in eight loses
// seconds to the disk, and a download of eight seconds has taken twenty-four.
// What a torrent must not do is stand still, so it fails when no byte has come
// for the time stalled, or when the whole wait has gone past three minutes,
// and says what the torrent and its seeder were doing: whether the connection
// went (no peers), or stayed and moved nothing.
func waitTorrentDone(t *testing.T, m *Manager, id string, seed *peer, stalled time.Duration) {
	t.Helper()
	const most = 3 * time.Minute
	begun := time.Now()
	var trace strings.Builder
	next, last, moved := begun, int64(-1), begun
	for !stateIs(m, id, StateDone)() {
		now := time.Now()
		var have int64
		if mt := taskOf(m, id); mt != nil {
			if tor := m.bt.loadedFor(mt); tor != nil {
				have = tor.BytesCompleted()
			}
		}
		if have != last {
			last, moved = have, now
		}
		if !now.Before(next) {
			v, _ := findTask(m, id)
			line := fmt.Sprintf("\n  %s: task %s %q", now.Format("15:04:05.0"), v.State, v.Error)
			if mt := taskOf(m, id); mt != nil {
				if tor := m.bt.loadedFor(mt); tor != nil {
					s := tor.Stats()
					line += fmt.Sprintf(", %d bytes done, %d active peers, %d pending, %d half-open, %d chunks read",
						tor.BytesCompleted(), s.ActivePeers, s.PendingPeers, s.HalfOpenPeers, s.ChunksReadUseful.Int64())
				}
			}
			if seed != nil {
				line += fmt.Sprintf("; seeder sent %d, %d peers", seed.uploaded(), seed.t.Stats().ActivePeers)
			}
			trace.WriteString(line)
			next = now.Add(2 * time.Second)
		}
		if now.Sub(moved) > stalled {
			t.Fatalf("no byte arrived for %s (%s since the wait began):%s", now.Sub(moved).Round(time.Millisecond), now.Sub(begun).Round(time.Millisecond), trace.String())
		}
		if now.Sub(begun) > most {
			t.Fatalf("still not done %s after the wait began:%s", now.Sub(begun).Round(time.Second), trace.String())
		}
		time.Sleep(15 * time.Millisecond)
	}
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
	cfg.KeepAliveTimeout = btPeerWake // as in newPeer
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

// hybridOf turns a single-file torrent into the v1+v2 hybrid libtorrent 2
// makes by default: the v1 fields as they were, plus the v2 file tree, whose
// one entry is the file under its own name, and the piece layer a .torrent
// carries for a file of more than one piece.
func hybridOf(t *testing.T, mi *metainfo.MetaInfo, data []byte) *metainfo.MetaInfo {
	t.Helper()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	var hashes [][32]byte
	var layer []byte
	for off := 0; off < len(data); off += int(info.PieceLength) {
		h := merkle.NewHash()
		h.Write(data[off:min(off+int(info.PieceLength), len(data))])
		var s [32]byte
		if len(data) > int(info.PieceLength) {
			h.SumMinLength(s[:0], int(info.PieceLength))
		} else {
			h.Sum(s[:0]) // a file of one piece is hashed as it is, unpadded
		}
		hashes, layer = append(hashes, s), append(layer, s[:]...)
	}
	root := merkle.RootWithPadHash(hashes, metainfo.HashForPiecePad(info.PieceLength))
	info.MetaVersion = 2
	info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{
		info.Name: {File: metainfo.FileTreeFile{Length: info.Length, PiecesRoot: string(root[:])}},
	}}
	out := &metainfo.MetaInfo{}
	// By its address: the tree encodes itself, through a pointer.
	if out.InfoBytes, err = bencode.Marshal(&info); err != nil {
		t.Fatal(err)
	}
	if len(hashes) > 1 {
		out.PieceLayers = map[string]string{string(root[:]): string(layer)}
	}
	return out
}

// A v2 file tree has the same shape for one file as for a folder holding one
// file, and the library reads both as a folder. What tells them apart is the
// v1 half of the torrent, which lists a folder's files and gives one file
// only its length.
func TestAHybridTorrentOfOneFileIsWrittenAsThatFile(t *testing.T) {
	const length, pieceLen, chunk = 100 << 10, 32 << 10, 16 << 10
	ih := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	leaf := metainfo.FileTree{File: metainfo.FileTreeFile{Length: length}}
	in := func(name string, sub metainfo.FileTree) metainfo.FileTree {
		return metainfo.FileTree{Dir: map[string]metainfo.FileTree{name: sub}}
	}
	listed := func(p ...string) []metainfo.FileInfo {
		return []metainfo.FileInfo{{Path: p, Length: length}}
	}
	hybridFile := metainfo.Info{Name: "film.mkv", Length: length, MetaVersion: 2, FileTree: in("film.mkv", leaf)}
	pc, err := openBTCompletion(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	// place opens the torrent for a task, writes the start of its file, and
	// checks that writing, playing and deleting all find it at want.
	place := func(what string, info metainfo.Info, mt *managedTask, want string) {
		t.Helper()
		info.PieceLength, info.Pieces = pieceLen, make([]byte, 4*20)
		st := &btStorage{mt: mt, pc: pc, slog: slog.New(slog.NewTextHandler(io.Discard, nil)), failed: make(chan error, 1)}
		impl, err := st.OpenTorrent(t.Context(), &info, ih)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if _, err := impl.Piece(info.Piece(0)).WriteAt(make([]byte, chunk), 0); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		file := filepath.Join(mt.outDir, filepath.FromSlash(want))
		if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() || fi.Size() != chunk {
			t.Errorf("%s: no file of %d bytes at %s (%v); the download folder holds %q", what, chunk, want, err, treeOf(mt.outDir))
		}
		mt.torrentDetails(&info, length)
		v := mt.snapshot()
		if wantRoot := filepath.Join(mt.outDir, strings.Split(want, "/")[0]); v.Path != wantRoot || v.Filename != filepath.Base(wantRoot) {
			t.Errorf("%s: the task shows path %q, name %q; want %q", what, v.Path, v.Filename, wantRoot)
		}
		if p, err := torrentMediaPath(v); v.Media != want || err != nil || p != file {
			t.Errorf("%s: media %q plays %q (%v), want %q", what, v.Media, p, err, want)
		}
		if err := impl.Close(); err != nil {
			t.Fatal(err)
		}
		deleteTorrentFiles(v.Path, &info)
		if left := treeOf(mt.outDir); len(left) != 0 {
			t.Errorf("%s: deleting it left %q", what, left)
		}
	}
	for _, c := range []struct {
		what string
		info metainfo.Info
		want string // where the file goes in the download folder
	}{
		{"a hybrid of one file", hybridFile, "film.mkv"},
		{"a hybrid of a folder holding one file",
			metainfo.Info{Name: "Film", Files: listed("film.mkv"), MetaVersion: 2, FileTree: in("film.mkv", leaf)}, "Film/film.mkv"},
		{"a hybrid of a folder holding one file of its own name",
			metainfo.Info{Name: "film.mkv", Files: listed("film.mkv"), MetaVersion: 2, FileTree: in("film.mkv", leaf)}, "film.mkv/film.mkv"},
		{"a hybrid whose one file is in a folder of its own",
			metainfo.Info{Name: "Film", Files: listed("Disc", "film.mkv"), MetaVersion: 2, FileTree: in("Disc", in("film.mkv", leaf))}, "Film/Disc/film.mkv"},
		{"a v1 torrent of one file", metainfo.Info{Name: "film.mkv", Length: length}, "film.mkv"},
		{"a v1 torrent of a folder holding one file", metainfo.Info{Name: "Film", Files: listed("film.mkv")}, "Film/film.mkv"},
	} {
		place(c.what, c.info, &managedTask{outDir: t.TempDir()}, c.want)
	}

	// godm 0.1.1 and earlier made a folder for such a torrent and put the
	// file in it. One begun that way goes on that way: its pieces are on
	// record where they are, and the folder is what the task has claimed.
	old := &managedTask{outDir: t.TempDir()}
	old.view.Path = filepath.Join(old.outDir, "film.mkv")
	if err := os.Mkdir(old.view.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	place("a hybrid of one file begun by an earlier godm", hybridFile, old, "film.mkv/film.mkv")

	// And from a swarm: the name is taken, so it gets a number as a file
	// does, before the extension, and the player and the delete follow it.
	var mu sync.Mutex
	var opened []string
	oldPlayer := launchPlayer
	launchPlayer = func(p playerLaunch) error {
		mu.Lock()
		opened = append(opened, p.Target)
		mu.Unlock()
		return nil
	}
	defer func() { launchPlayer = oldPlayer }()

	src := t.TempDir()
	film := makePayload(600<<10 + 123)
	// The library keeps such a torrent's file in a folder of the same name,
	// so that is where the seeder has it.
	mi := hybridOf(t, makeTorrent(t, filepath.Join(src, "film.mkv"), "film.mkv", pieceLen, tfile{data: film}), film)
	if info, err := mi.UnmarshalInfo(); err != nil || !info.HasV1() || !info.HasV2() || !info.IsDir() {
		t.Fatalf("the torrent made here is not the hybrid the library reads as a folder: %+v, %v", info, err)
	}
	seed := newSeeder(t, src, mi, 0)

	m := newBTManager(t)
	s, api := btAPI(t, m)
	theirs := filepath.Join(m.OutDir(), "film.mkv")
	if err := os.WriteFile(theirs, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if err := mi.Write(&body); err != nil {
		t.Fatal(err)
	}
	id, err := m.AddTorrentFile(body.Bytes(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitLoaded(t, m, id).AddClientPeer(seed.cl)
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if want := filepath.Join(m.OutDir(), "film (1).mkv"); v.Path != want || v.Filename != "film (1).mkv" || v.Media != "film (1).mkv" {
		t.Errorf("path %q, name %q, media %q; want the file %q", v.Path, v.Filename, v.Media, want)
	}
	if got, err := os.ReadFile(v.Path); err != nil || sum(got) != sum(film) {
		t.Fatalf("%s is not the seeded film (%v); the download folder holds %q", v.Path, err, treeOf(m.OutDir()))
	}
	if got, _ := os.ReadFile(theirs); string(got) != "someone else's file" {
		t.Fatal("the torrent wrote into a file it did not create")
	}
	resp, err := http.DefaultClient.Do(bearer(http.MethodGet, api.URL+"/stream/"+id+"/"+url.PathEscape(v.Filename), nil))
	if err != nil {
		t.Fatal(err)
	}
	streamed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || sum(streamed) != sum(film) {
		t.Errorf("the stream of the finished film: status %d, %d bytes", resp.StatusCode, len(streamed))
	}
	rec := httptest.NewRecorder()
	s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/api/play?id="+id, nil))
	mu.Lock()
	if len(opened) != 1 || opened[0] != v.Path {
		t.Errorf("play opened %q (%s), want %q", opened, rec.Body.String(), v.Path)
	}
	mu.Unlock()
	if !m.Remove(id, true) {
		t.Fatal("remove refused")
	}
	waitFor(t, 10*time.Second, "the film to be deleted", func() bool { return !pathExists(v.Path) })
	if left := treeOf(m.OutDir()); len(left) != 1 || left[0] != "film.mkv" {
		t.Errorf("after the removal the download folder holds %q, want only the file that was there before", left)
	}
}

// treeOf lists everything under dir, folders with a trailing slash.
func treeOf(dir string) (out []string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel = filepath.ToSlash(rel); d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	return out
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

// The library's writer for a peer connection looks at it only when told there
// is something to send, or when a keep-alive is due, and it can miss being
// told: the connection then asks for nothing more until the keep-alive. That
// cannot be staged here, but what bounds it can be seen: a connection with
// nothing to say is written to, which is the writer looking at it, every
// btPeerWake and not once a minute.
func TestAnIdlePeerConnectionIsLookedAtAgainWithinSeconds(t *testing.T) {
	src := t.TempDir()
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: makePayload(1 << 20)})
	// One chunk and then next to nothing, so godm is left waiting on what it
	// has asked for, with no more to ask.
	seed := newSeeder(t, src, mi, 1)

	m := newBTManager(t)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	tor := waitLoaded(t, m, id)
	waitFor(t, 10*time.Second, "the first chunk", func() bool {
		s := tor.Stats()
		return s.ChunksReadUseful.Int64() >= 1
	})
	written := func() int64 {
		s := tor.Stats()
		return s.BytesWritten.Int64()
	}
	sent := written()
	waitFor(t, 10*time.Second, "godm to have no more to send", func() bool {
		time.Sleep(500 * time.Millisecond)
		before := sent
		sent = written()
		return sent == before
	})
	idle := time.Now()
	waitFor(t, 2*btPeerWake+2*time.Second, "the idle connection to be looked at again", func() bool { return written() > sent })
	if s := tor.Stats(); s.ActivePeers != 1 || tor.Complete().Bool() {
		t.Fatalf("after %s: %d peers, complete %v; this no longer watches an idle connection", time.Since(idle).Round(time.Millisecond), s.ActivePeers, tor.Complete().Bool())
	}
	m.Pause(id)
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

// trackerLink serves a .torrent the way a tracker does: at download.php, to
// the logged-in cookie uid=1 only. It returns the link, and what the tracker
// was last sent.
func trackerLink(t *testing.T, mi *metainfo.MetaInfo) (string, func() (cookie, referer string)) {
	var body bytes.Buffer
	if err := mi.Write(&body); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var cookie, referer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookie, referer = r.Header.Get("Cookie"), r.Header.Get("Referer")
		mu.Unlock()
		if r.URL.Path != "/download.php" || r.Header.Get("Cookie") != "uid=1" {
			http.Error(w, "log in first", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write(body.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/download.php?id=42", func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return cookie, referer
	}
}

// jobOf is what a task will run, and save, as it stands.
func jobOf(m *Manager, id string) jobRequest {
	mt := taskOf(m, id)
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.req
}

// The extension hands over a .torrent the browser started to download as the
// link that was clicked, which on a tracker is seldom a path ending in .torrent
// and only answers to the person's login. The kind is what makes it a torrent,
// and the browser's cookie and referrer have to reach the fetch. The browser's
// own copy is gone by then, so the file is fetched before the daemon answers:
// a link godm cannot fetch is refused, and the extension gives it back to the
// browser, rather than becoming a failed task and no .torrent anywhere.
func TestATrackerLinkFromTheBrowserIsFetchedBeforeItIsAccepted(t *testing.T) {
	mi := makeTorrent(t, t.TempDir(), "film.bin", 32<<10, tfile{data: makePayload(64 << 10)})
	link, seen := trackerLink(t, mi)
	m := newBTManager(t)
	add := func(cookie string) *httptest.ResponseRecorder {
		nr := nativeRequest{
			Type: "download", URL: link, Kind: "bt",
			Filename: "Some Film.torrent", Cookie: cookie, Referrer: "https://tracker.example/t/42",
		}
		b, _ := json.Marshal(nr.job(nr.URL, nr.Filename, nr.Cookie))
		rec := httptest.NewRecorder()
		(&server{mgr: m}).handleDownload(rec, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(b)))
		return rec
	}

	// The native host passes a 400 on as the daemon's refusal, in its words.
	if rec := add(""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "403") {
		t.Fatalf("a .torrent the tracker would not give was answered %d %q", rec.Code, rec.Body.String())
	}
	if tasks := m.List(); len(tasks) != 0 {
		t.Fatalf("a .torrent that could not be fetched left a task: %+v", tasks)
	}

	rec := add("uid=1")
	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if !out.OK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if cookie, referer := seen(); cookie != "uid=1" || referer != "https://tracker.example/t/42" {
		t.Errorf("the tracker saw cookie %q and referer %q", cookie, referer)
	}
	job := jobOf(m, out.ID)
	if !strings.HasPrefix(job.URL, "magnet:?xt=urn:btih:"+mi.HashInfoBytes().HexString()) {
		t.Errorf("job URL %q, want the magnet link of the torrent that was fetched", job.URL)
	}
	if _, ok := job.Headers["Cookie"]; ok {
		t.Errorf("the tracker login is kept with the task, which never needs it again: %v", job.Headers)
	}
	if v, _ := findTask(m, out.ID); v.Kind != "bt" {
		t.Errorf("task kind %q, want bt", v.Kind)
	}
	// The file was kept, so the torrent loads without waiting for a peer.
	waitLoaded(t, m, out.ID)
}

// A link to a .torrent queued with others is fetched when its task runs. Once
// the job names the torrent itself, the login that fetched the file is dropped
// from it, as the job is what goes into the saved list.
func TestATorrentLinkFetchedByItsTaskKeepsNoLogin(t *testing.T) {
	mi := makeTorrent(t, t.TempDir(), "film.bin", 32<<10, tfile{data: makePayload(64 << 10)})
	link, _ := trackerLink(t, mi)
	m := newBTManager(t)
	nr := nativeRequest{Type: "download", URL: link, Kind: "bt", Cookie: "uid=1", UserAgent: "test-agent"}
	id, err := m.Add(nr.job(nr.URL, "", nr.Cookie))
	if err != nil {
		t.Fatal(err)
	}
	waitLoaded(t, m, id)
	job := jobOf(m, id)
	if !strings.HasPrefix(job.URL, "magnet:") {
		t.Fatalf("job URL %q, want the magnet link of the torrent that was fetched", job.URL)
	}
	if _, ok := job.Headers["Cookie"]; ok || job.Headers["User-Agent"] != "test-agent" {
		t.Errorf("headers kept with the task: %v; want the login gone and the rest left", job.Headers)
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

// holdsLimit watches what a torrent completes for the time d, and for up to
// five times that if it has to, and judges its pace against a limit in bytes
// per second. All of the stretch together must come to no more than the limit
// allows, with slack bytes besides for what a change of limit lets through
// early; and some second of it must have come at no less than six tenths of
// the limit, counting from half a second after the change.
//
// It does not ask for the average, because a runner whose system disk now
// and then slows the transfer for a second or two takes that much out of any
// stretch, and a limit does not say how much a torrent gets when it does not
// get it all. A limit that is not in force shows in the whole stretch, as too
// much; a torrent starved by the change, or held far under the limit, shows in
// there being no second at the pace.
func holdsLimit(t *testing.T, tor *torrent.Torrent, limit int64, d time.Duration, slack int64) {
	t.Helper()
	type sample struct {
		at   time.Time
		have int64
	}
	start := time.Now()
	samples := []sample{{start, tor.BytesCompleted()}}
	best := 0.0
	for {
		time.Sleep(50 * time.Millisecond)
		now := sample{time.Now(), tor.BytesCompleted()}
		samples = append(samples, now)
		// The latest second that began after the change had settled.
		for i := len(samples) - 2; i >= 0 && samples[i].at.Sub(start) >= 500*time.Millisecond; i-- {
			if age := now.at.Sub(samples[i].at); age >= time.Second {
				best = max(best, float64(now.have-samples[i].have)/age.Seconds())
				break
			}
		}
		elapsed := now.at.Sub(start)
		// A system disk that is slow for a second or two takes that much out
		// of the stretch, and one has held a torrent under half of a limit of
		// 256 KB/s for nine seconds on end. The films are long enough to wait
		// out five times as much as the stretch asks for, and a torrent that is
		// held for longer than that is not one a pace can be judged on.
		if elapsed >= d && (best >= 0.6*float64(limit) || elapsed >= 5*d) {
			break
		}
	}
	last := samples[len(samples)-1]
	var bySecond strings.Builder
	for sec, from := 1, 0; ; sec++ {
		to := slices.IndexFunc(samples[from:], func(s sample) bool { return s.at.Sub(start) >= time.Duration(sec)*time.Second })
		if to < 0 {
			break
		}
		fmt.Fprintf(&bySecond, " %d", samples[from+to].have-samples[from].have)
		from += to
	}
	elapsed := last.at.Sub(start)
	t.Logf("under a limit of %d bytes/s: %d bytes in %s, the best second %.0f; by the second:%s", limit, last.have-samples[0].have, elapsed.Round(time.Millisecond), best, bySecond.String())
	if got, most := last.have-samples[0].have, int64(1.4*float64(limit)*elapsed.Seconds())+slack; got > most {
		t.Errorf("under a limit of %d bytes/s, %d bytes arrived in %s; no more than %d should have", limit, got, elapsed.Round(time.Millisecond), most)
	}
	if best < 0.6*float64(limit) {
		t.Errorf("under a limit of %d bytes/s, no second of %s came at more than %.0f bytes/s, as if the limit were lower", limit, elapsed.Round(time.Millisecond), best)
	}
}

// waitForPace waits until a stretch of the length over, of what the torrent
// completes, has come at atLeast bytes per second, and fails if none has
// within the time given. The fastest stretch is the one to ask for: how long
// the rest takes, or how much comes in a given time, is as much the disk's and
// the machine's as the limit's. The stretch has to be long enough that the
// reads which were out when the limit changed, and are paid for late, cannot
// make up a pace the limit would not have allowed.
func waitForPace(t *testing.T, tor *torrent.Torrent, atLeast float64, over, within time.Duration, what string) {
	t.Helper()
	type sample struct {
		at   time.Time
		have int64
	}
	start := time.Now()
	samples := []sample{{start, tor.BytesCompleted()}}
	best := 0.0
	for {
		time.Sleep(25 * time.Millisecond)
		now := sample{time.Now(), tor.BytesCompleted()}
		samples = append(samples, now)
		for i := len(samples) - 2; i >= 0; i-- {
			if age := now.at.Sub(samples[i].at); age >= over {
				best = max(best, float64(now.have-samples[i].have)/age.Seconds())
				break
			}
		}
		if best >= atLeast {
			t.Logf("%s: %.0f bytes/s after %s", what, best, now.at.Sub(start).Round(time.Millisecond))
			return
		}
		if now.at.Sub(start) > within {
			t.Fatalf("%s: no %s of the %s came at %.0f bytes/s or more; the fastest came at %.0f", what, over, within, atLeast, best)
		}
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
	holdsLimit(t, tor, limit, 3*time.Second, 0)

	// Lifting the limit reaches the torrent that is already running. How long
	// the rest takes says little, as the connection can stand still for a few
	// seconds at a time (see btPeerWake); how fast it comes while it comes
	// says it all. Each half second is sampled, as under the limit a single
	// read can bring a tenth of a second's worth at once.
	m.SetSpeedLimit(0)
	lifted := time.Now()
	size := tor.Length()
	last, lastAt := tor.BytesCompleted(), lifted
	var fastest float64
	var trace strings.Builder
	done := stateIs(m, id, StateDone)
	for {
		now, finished := time.Now(), done()
		if d := now.Sub(lastAt); d >= 500*time.Millisecond || finished {
			have := tor.BytesCompleted()
			if finished {
				have = size // the torrent has left the client by now
			}
			fastest = max(fastest, float64(have-last)/max(d, 500*time.Millisecond).Seconds())
			v, _ := findTask(m, id)
			s := tor.Stats()
			fmt.Fprintf(&trace, "\n  %5.1fs: %d of %d bytes, %d peers, %d chunks read, task %s", now.Sub(lifted).Seconds(), have, size, s.ActivePeers, s.ChunksReadUseful.Int64(), v.State)
			last, lastAt = have, now
		}
		if finished {
			break
		}
		if now.Sub(lifted) > 30*time.Second {
			t.Fatalf("the rest had not arrived %s after the limit was lifted:%s", now.Sub(lifted).Round(time.Millisecond), trace.String())
		}
		time.Sleep(15 * time.Millisecond)
	}
	if fastest < 3*limit {
		t.Errorf("with the limit lifted the rest came at %.0f bytes/s at most, as if still under %d:%s", fastest, limit, trace.String())
	}
	v, _ := findTask(m, id)
	if b, _ := os.ReadFile(v.Path); sum(b) != sum(payload) {
		t.Fatal("downloaded file differs from the seeded one")
	}
}

// The library cuts each read from a peer to the limiter's burst as it is
// before the read, pays for the bytes after it, and panics if the limiter
// refuses, which it does to anything larger than its burst as it is by then.
// A read lasts as long as the peer keeps it waiting, so the limit has to be
// changeable in every direction while reads are out.
func TestTheSpeedLimitChangesUnderARunningTorrent(t *testing.T) {
	const (
		high   = 1 << 20
		low    = 64 << 10
		middle = 128 << 10
	)
	src := t.TempDir()
	payload := makePayload(6 << 20)
	mi := makeTorrent(t, src, "film.bin", 32<<10, tfile{data: payload})
	// Slower than the high limit, so that under it godm's reads are waiting on
	// the peer, cut to that limit's burst, when the limit changes.
	const peerRate = 384 << 10
	seed := newSeeder(t, src, mi, peerRate)

	m := newBTManager(t)
	m.SetSpeedLimit(high)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	tor := waitLoaded(t, m, id)
	waitFor(t, 20*time.Second, "the first of the film", func() bool { return tor.BytesCompleted() >= 256<<10 })

	// A read begun under an earlier limit is paid for whole when it returns,
	// so around a change one read's worth may arrive early or late: at most
	// the burst of the highest limit so far.
	_, oneRead := btDownloadLimit(high)
	holds := func(limit int64, d time.Duration) {
		t.Helper()
		holdsLimit(t, tor, limit, d, int64(oneRead))
	}

	m.SetSpeedLimit(low)
	holds(low, 4*time.Second)

	m.SetSpeedLimit(middle)
	holds(middle, 3*time.Second)

	// With no limit the peer sets the pace, three times the last limit. How
	// soon it gets there is no matter, only that it does: a limit still in
	// force would keep every second under two and a half times itself, even
	// with the read it owes from before.
	m.SetSpeedLimit(0)
	waitForPace(t, tor, 2.5*middle, time.Second, 20*time.Second, "the peer's own pace once the limit is lifted")

	// A limit that comes back finds reads that began before it went away:
	// sized under the high limit, still out through the time without one,
	// and paid for under the low one.
	m.SetSpeedLimit(high)
	time.Sleep(time.Second)
	m.SetSpeedLimit(0)
	m.SetSpeedLimit(low)
	holds(low, 4*time.Second)

	if tor.Complete().Bool() {
		t.Fatal("the film finished before the last change of limit; make it longer")
	}
}

// The same rule without a swarm: whatever the limit becomes, the limiter must
// still accept a read cut to any burst it has had under a limit, and a limit
// must not come into force with more saved up than it would have let build.
func TestTheTorrentLimiterKeepsRoomForReadsAlreadyBegun(t *testing.T) {
	e := &btEngine{}
	e.setDownloadRate(1 << 20)
	lim := e.config(0).DownloadRateLimiter
	begun := 0 // the largest read that may still be waiting on a peer
	for _, n := range []int64{64 << 10, 32 << 20, 0, 16 << 10, 0, 0, 256 << 10, 1 << 10, 4 << 20, 64 << 10} {
		// Without a limit the library reads past the limiter altogether.
		if lim.Limit() != rate.Inf {
			begun = max(begun, lim.Burst())
		}
		e.setDownloadRate(n)
		if lim.Burst() < begun {
			t.Fatalf("with the limit now %d bytes/s the burst is %d: a read of %d bytes begun earlier is refused, and the library panics", n, lim.Burst(), begun)
		}
		if n == 0 {
			if lim.Limit() != rate.Inf {
				t.Fatalf("lifting the limit left one of %v bytes/s", lim.Limit())
			}
			continue
		}
		if lim.Limit() != rate.Limit(n) {
			t.Fatalf("limit is %v bytes/s, want %d", lim.Limit(), n)
		}
		// A tenth of a second's worth, and what the clock adds while this runs.
		_, most := btDownloadLimit(n)
		if saved := lim.Tokens(); saved > float64(most)+float64(n)/20 {
			t.Errorf("a limit of %d bytes/s came into force with %.0f bytes saved up; the most it lets build is %d", n, saved, most)
		}
	}

	// Nor may the room a higher limit left fill up later, while less arrives
	// than the limit allows. Running torrents see to that as they tick.
	const limit = 2 << 20
	e.setDownloadRate(32 << 20)
	e.setDownloadRate(limit)
	_, most := btDownloadLimit(limit)
	time.Sleep(300 * time.Millisecond)
	if saved := lim.Tokens(); saved < float64(most)+limit/10 {
		t.Fatalf("only %.0f bytes saved up after a quiet 300ms at %d bytes/s; this no longer tests the trim", saved, limit)
	}
	e.trimDownload()
	if saved := lim.Tokens(); saved > float64(most)+limit/20 {
		t.Errorf("%.0f bytes saved up under a limit of %d bytes/s after the trim; the most it lets build is %d", saved, limit, most)
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
	launchPlayer = func(p playerLaunch) error {
		mu.Lock()
		opened = append(opened, p.Target)
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
