package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func sortOn(t *testing.T, m *Manager) {
	t.Helper()
	on := true
	if err := m.UpdateSettings(settingsUpdate{SortByType: &on}); err != nil {
		t.Fatal(err)
	}
}

// quickFile serves a small file at once.
func quickFile(t *testing.T) string {
	t.Helper()
	return newTestServer(t, &slowServer{payload: makePayload(24 << 10), chunk: 24 << 10, delay: time.Millisecond}).URL
}

// ---------- what a request is ----------

func TestTheFileTypeDecidesTheFolder(t *testing.T) {
	cases := []struct {
		name string
		req  jobRequest
		want string
	}{
		{"extension from the filename", jobRequest{Filename: "clip.mp4", URL: "https://h/x"}, "video"},
		{"capitals do not matter", jobRequest{Filename: "CLIP.MKV", URL: "https://h/x"}, "video"},
		{"extension from the address", jobRequest{URL: "https://h/files/song.mp3"}, "audio"},
		{"the query is not the name", jobRequest{URL: "https://h/dl/pack.zip?t=a.mp4"}, "archive"},
		{"an escaped name", jobRequest{URL: "https://h/dl/My%20Report.pdf"}, "doc"},
		{"a double extension counts the last", jobRequest{Filename: "backup.tar.gz"}, "archive"},
		{"programs", jobRequest{Filename: "setup.exe"}, "app"},
		{"images", jobRequest{Filename: "photo.JPEG"}, "image"},
		{"a path in the name", jobRequest{Filename: `C:\Users\x\clip.mp4`}, "video"},
		{"a type that is not known", jobRequest{Filename: "data.xyz", URL: "https://h/x"}, ""},
		{"no extension anywhere", jobRequest{URL: "https://h/download"}, ""},
		{"the filename has the say when it has an extension", jobRequest{Filename: "data.bin", URL: "https://h/x.mp4"}, ""},
		{"an address stands in for a name with no extension", jobRequest{Filename: "movie", URL: "https://h/x.mp4"}, "video"},
		{"a playlist address", jobRequest{URL: "https://h/live/index.m3u8"}, "video"},
		{"a stream asked for by kind", jobRequest{URL: "https://h/stream?id=4", Kind: "hls"}, "video"},
		{"a stream is video whatever its name says", jobRequest{URL: "https://h/s.m3u8", Kind: "hls", Filename: "talk.mp3"}, "video"},
		{"yt-dlp", jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp"}, "video"},
		{"a bad address", jobRequest{URL: "https://h/%zz"}, ""},
	}
	for _, c := range cases {
		if got := folderKindFor(c.req); got != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, got, c.want)
		}
	}
}

// The page colours each card by kindOf; the folders must mean the same by
// "video" or "doc" as the page does.
func TestFolderKindsMatchThePage(t *testing.T) {
	re := regexp.MustCompile(`if \(/\^\(([a-z0-9|]+)\)\$/\.test\(ext\)\) return "(\w+)";`)
	onPage := map[string][]string{}
	for _, m := range re.FindAllStringSubmatch(uiHTML, -1) {
		onPage[m[2]] = strings.Split(m[1], "|")
	}
	if len(onPage) != len(folderKinds) {
		t.Fatalf("the page sorts into %d kinds, the daemon into %d: %v", len(onPage), len(folderKinds), onPage)
	}
	for _, k := range folderKinds {
		page, ok := onPage[k.key]
		if !ok {
			t.Errorf("the page has no kind %q", k.key)
			continue
		}
		have, want := append([]string(nil), k.exts...), append([]string(nil), page...)
		sort.Strings(have)
		sort.Strings(want)
		if !reflect.DeepEqual(have, want) {
			t.Errorf("%s: daemon has %v, page has %v", k.key, have, want)
		}
	}
}

func TestFolderNamesAreCheckedBeforeTheyAreUsed(t *testing.T) {
	good := map[string]string{
		"Video": "Video", "  Movies  ": "Movies", "": "", "My Films": "My Films", "Müzik": "Müzik", "a.b": "a.b",
	}
	for in, want := range good {
		got, err := cleanFolderName(in)
		if err != nil || got != want {
			t.Errorf("cleanFolderName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"..", ".", `..\..\Windows`, "a/b", `a\b`, `C:\x`, "C:", "what?", "star*", `q"q`, "<x>", "a|b",
		"CON", "nul", "com1", "lpt3.txt", "ends.", "x\x00y", "tab\tin", strings.Repeat("a", 101), "\xff\xfe",
	} {
		if got, err := cleanFolderName(bad); err == nil {
			t.Errorf("cleanFolderName(%q) = %q, want it refused", bad, got)
		}
	}
}

// ---------- where a download goes ----------

func TestSortingIsOffUntilItIsAskedFor(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	if got := m.downloadDir(jobRequest{Filename: "clip.mp4"}); got != m.outDir {
		t.Fatalf("with sorting off a video goes to %q, want %q", got, m.outDir)
	}
	if m.settingsView()["sort_by_type"] != false {
		t.Error("sorting is on by default")
	}
}

func TestSortedDownloadsLandInTheirFolders(t *testing.T) {
	base := t.TempDir()
	m := NewManager(base, 3)
	sortOn(t, m)
	srv := quickFile(t)

	jobs := []struct{ name, folder string }{
		{"clip.mp4", "Video"}, {"song.mp3", "Music"}, {"pack.zip", "Archives"},
		{"paper.pdf", "Documents"}, {"setup.exe", "Programs"}, {"pic.png", "Images"},
		{"mystery.xyz", ""},
	}
	ids := map[string]string{}
	for _, j := range jobs {
		id, err := m.Add(jobRequest{URL: srv + "/" + j.name, Filename: j.name, Connections: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids[j.name] = id
	}
	for _, j := range jobs {
		waitFor(t, 30*time.Second, j.name, stateIs(m, ids[j.name], StateDone))
		v, _ := findTask(m, ids[j.name])
		if want := filepath.Join(base, j.folder, j.name); v.Path != want {
			t.Errorf("%s saved to %s, want %s", j.name, v.Path, want)
		}
		if _, err := os.Stat(v.Path); err != nil {
			t.Errorf("%s: %v", j.name, err)
		}
	}
}

func TestAFolderTheCallerNamesBeatsSorting(t *testing.T) {
	base, chosen := t.TempDir(), t.TempDir()
	m := NewManager(base, 2)
	sortOn(t, m)
	id, _ := m.Add(jobRequest{URL: quickFile(t) + "/clip.mp4", Filename: "clip.mp4", OutDir: chosen, Connections: 1})
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Path != filepath.Join(chosen, "clip.mp4") {
		t.Errorf("saved to %s, want it in the folder that was asked for", v.Path)
	}
	if _, err := os.Stat(filepath.Join(base, "Video")); err == nil {
		t.Error("a Video folder was made although the download went elsewhere")
	}
}

func TestStreamsAndYTDLPGoWithTheVideo(t *testing.T) {
	t.Run("hls", func(t *testing.T) {
		base := t.TempDir()
		m := NewManager(base, 2)
		sortOn(t, m)
		s := newHLSServer(t, makeSegments(4))
		id, err := m.Add(jobRequest{URL: s.URL + "/media.m3u8", Connections: 2, Variant: -1})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 30*time.Second, "the stream", stateIs(m, id, StateDone))
		v, _ := findTask(m, id)
		if dir := filepath.Dir(v.Path); dir != filepath.Join(base, "Video") {
			t.Errorf("stream saved in %s, want the Video folder", dir)
		}
		if _, err := os.Stat(v.Path); err != nil {
			t.Error(err)
		}
	})
	t.Run("yt-dlp", func(t *testing.T) {
		stubYTDLP(t, "ok")
		t.Setenv("GODM_YTDLP", os.Args[0])
		base := t.TempDir()
		m := NewManager(base, 2)
		sortOn(t, m)
		id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 30*time.Second, "yt-dlp", stateIs(m, id, StateDone))
		v, _ := findTask(m, id)
		if dir := filepath.Dir(v.Path); dir != filepath.Join(base, "Video") {
			t.Errorf("yt-dlp output in %s, want the Video folder", dir)
		}
	})
}

// A stream is written without the engine making its folder first, which a
// download that is sorted into a new subfolder runs straight into.
func TestAStreamCreatesTheFolderItWritesTo(t *testing.T) {
	segs := makeSegments(3)
	s := newHLSServer(t, segs)
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	target, err := DownloadHLS(t.Context(), HLSOptions{URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(joined(segs)) {
		t.Error("the file is not the stream")
	}
}

// Sorting applies to what is added from now on, and to nothing else.
func TestDownloadsAlreadyInTheListStayWhereTheyAre(t *testing.T) {
	base := t.TempDir()
	m := NewManager(base, 2)
	srv := newTestServer(t, &slowServer{payload: makePayload(512 << 10), chunk: 8 << 10, delay: 20 * time.Millisecond})

	old, _ := m.Add(jobRequest{URL: srv.URL + "/old.mp4", Filename: "old.mp4", Connections: 1})
	waitFor(t, 10*time.Second, "some progress", func() bool {
		v, _ := findTask(m, old)
		return v.Received > 32<<10
	})
	m.Pause(old)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, old, StatePaused))

	sortOn(t, m)
	if !m.Resume(old) {
		t.Fatal("resume refused")
	}
	fresh, _ := m.Add(jobRequest{URL: srv.URL + "/fresh.mp4", Filename: "fresh.mp4", Connections: 1})
	waitFor(t, 30*time.Second, "both downloads", func() bool {
		return stateIs(m, old, StateDone)() && stateIs(m, fresh, StateDone)()
	})

	if v, _ := findTask(m, old); v.Path != filepath.Join(base, "old.mp4") {
		t.Errorf("the download that was under way moved to %s", v.Path)
	}
	if v, _ := findTask(m, fresh); v.Path != filepath.Join(base, "Video", "fresh.mp4") {
		t.Errorf("the new download is at %s, want it sorted", v.Path)
	}
}

func TestFolderNamesCanBeChangedOrLeftEmpty(t *testing.T) {
	base := t.TempDir()
	m := NewManager(base, 2)
	sortOn(t, m)
	if err := m.UpdateSettings(settingsUpdate{Folders: map[string]string{"video": "Movies", "audio": ""}}); err != nil {
		t.Fatal(err)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.mp4"}); got != filepath.Join(base, "Movies") {
		t.Errorf("video goes to %s after renaming the folder", got)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.mp3"}); got != base {
		t.Errorf("music goes to %s with its folder name empty, want the main folder", got)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.zip"}); got != filepath.Join(base, "Archives") {
		t.Errorf("a folder nobody renamed lost its name: %s", got)
	}
}

func TestABadFolderNameChangesNothing(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	off := false
	for _, bad := range []map[string]string{
		{"video": `..\..`}, {"video": "a/b"}, {"photos": "Photos"}, {"video": "Movies", "audio": "x:y"},
	} {
		if err := m.UpdateSettings(settingsUpdate{KeepAwake: &off, Folders: bad}); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if !m.KeepAwake() {
		t.Error("a refused request still changed another setting")
	}
	if got := m.settingsView()["folders"].(map[string]string)["video"]; got != "Video" {
		t.Errorf("video folder = %q after refused requests, want Video", got)
	}
}

// ---------- remembered across restarts ----------

func TestSortingChoicesSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	m := NewManager(dir, 2)
	m.store = store
	sortOn(t, m)
	if err := m.UpdateSettings(settingsUpdate{Folders: map[string]string{"video": "Movies", "doc": ""}}); err != nil {
		t.Fatal(err)
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 2)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	v := m2.settingsView()
	if v["sort_by_type"] != true {
		t.Error("sorting was not restored")
	}
	want := defaultFolders()
	want["video"], want["doc"] = "Movies", ""
	if got := v["folders"]; !reflect.DeepEqual(got, want) {
		t.Errorf("folders = %v, want %v", got, want)
	}
}

func TestAnOldTaskListGetsSortingOffAndTheDefaultNames(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	// Written by a version that had settings, but none of these.
	os.WriteFile(store, []byte(`{"version":1,"limit":3,"tasks":[],"settings":{"keep_awake":false}}`), 0o600)
	m := NewManager(dir, 2)
	m.store = store
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	v := m.settingsView()
	if v["sort_by_type"] != false || v["keep_awake"] != false || !reflect.DeepEqual(v["folders"], defaultFolders()) {
		t.Errorf("settings = %v, want sorting off, keep-awake as saved and the default names", v)
	}
}

// tasks.json is in the user's profile, but a name read from it still must not
// lead anywhere outside the downloads folder.
func TestAHandEditedFolderNameCannotLeaveTheDownloadsFolder(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	os.WriteFile(store, []byte(`{"version":1,"tasks":[],"settings":{"keep_awake":true,"sort_by_type":true,`+
		`"folders":{"video":"..\\..\\Windows","audio":"Songs","photos":"x","app":"a/b"}}}`), 0o600)
	base := t.TempDir()
	m := NewManager(base, 2)
	m.store = store
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.mp4"}); got != filepath.Join(base, "Video") {
		t.Errorf("video goes to %s, want the default folder after the bad name was dropped", got)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.exe"}); got != filepath.Join(base, "Programs") {
		t.Errorf("programs go to %s, want the default folder", got)
	}
	if got := m.downloadDir(jobRequest{Filename: "a.mp3"}); got != filepath.Join(base, "Songs") {
		t.Errorf("music goes to %s, want the folder name that was fine", got)
	}
	if _, ok := m.settingsView()["folders"].(map[string]string)["photos"]; ok {
		t.Error("an unknown kind was kept")
	}
}
