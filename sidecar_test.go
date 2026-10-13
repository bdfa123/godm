package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// wouldBeSidecar reports whether Windows would open a file of this name when
// asked for some other file's sidecar: it ignores case, and it drops dots and
// spaces from the end of a name.
func wouldBeSidecar(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(name, " .")), ".godm")
}

// sidecarNames are ways a server can ask for a file to be saved as the
// sidecar of "<victim>".
func sidecarNames(victim string) []string {
	return []string{
		victim + ".godm",
		victim + ".GODM",
		victim + ".GoDm",
		victim + ".godm.",
		victim + ".godm ",
		victim + ".godm. . .",
		`C:\Users\someone\Downloads\` + victim + ".godm",
		"../" + victim + ".godm",
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestNoDownloadIsNamedLikeASidecar(t *testing.T) {
	names := append(sidecarNames("installer.exe"),
		".godm",
		"a.godm",
		"CON.godm",
		"report?.godm",
		"spaced   out .godm",
		// Cut to 180 bytes, these end exactly on the extension, or on the dot
		// after it that Windows would then drop.
		strings.Repeat("a", 175)+".godm"+strings.Repeat("x", 30),
		strings.Repeat("a", 174)+".godm."+strings.Repeat("x", 30),
		strings.Repeat("a", 174)+".godm "+strings.Repeat("x", 30),
		strings.Repeat("a", 200)+".godm",
		strings.Repeat("中", 80)+".godm",
	)
	for _, in := range names {
		got := sanitize(in)
		if got == "" {
			t.Errorf("sanitize(%q) gave up on the name", in)
			continue
		}
		if wouldBeSidecar(got) {
			t.Errorf("sanitize(%q) = %q, which godm would read back as a sidecar", in, got)
		}
		if got != strings.TrimRight(got, " .") {
			t.Errorf("sanitize(%q) = %q, and Windows drops what it ends with", in, got)
		}
		if again := sanitize(got); again != got {
			t.Errorf("sanitize(%q) = %q, but sanitizing that gives %q", in, got, again)
		}
	}

	// Names that only mention it are left as they were.
	for _, in := range []string{"godm", "godm.exe", "notes.godm.txt", "installer.exe", "my.godmother.jpg", "x.godm_"} {
		if got := sanitize(in); got != in {
			t.Errorf("sanitize(%q) = %q, want it untouched", in, got)
		}
	}
}

// plantServer answers /plant with a file that asks to be saved as name and
// whose contents are body, and /installer.exe with a file that is not the one
// the user already has.
func plantServer(t *testing.T, name string, body func(srvURL string) []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plant":
			w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `\`, `\\`)+`"`)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body(srv.URL)))
		case "/installer.exe":
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(strings.Repeat("EVIL", 1000)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A sidecar is how godm knows a file is its own unfinished work and may be
// written into. A server that gets a file saved under a sidecar's name writes
// that permission itself, for any file already in the folder.
func TestADownloadCannotPlantASidecarForAnotherFile(t *testing.T) {
	const genuine = "the genuine installer the user downloaded last week"
	for _, name := range sidecarNames("installer.exe") {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(dir, "installer.exe")
			if err := os.WriteFile(victim, []byte(genuine), 0o644); err != nil {
				t.Fatal(err)
			}
			srv := plantServer(t, name, func(srvURL string) []byte {
				// "Our own attempt at this very link, which can no longer be
				// resumed": the branch that starts over in place.
				return []byte(fmt.Sprintf(`{"version":1,"url":%q,"size":1,"segments":[]}`, srvURL+"/installer.exe"))
			})

			first, err := Download(context.Background(), Options{URL: srv.URL + "/plant", OutDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if wouldBeSidecar(filepath.Base(first.Path)) {
				t.Errorf("the first download was saved as %q, a sidecar's name", filepath.Base(first.Path))
			}
			second, err := Download(context.Background(), Options{URL: srv.URL + "/installer.exe", OutDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(victim); string(got) != genuine {
				t.Errorf("the file that was already there now starts %q (%d bytes)", got[:min(len(got), 16)], len(got))
			}
			if filepath.Base(second.Path) != "installer (1).exe" {
				t.Errorf("the second download went to %q, want installer (1).exe; the folder holds %q",
					filepath.Base(second.Path), dirNames(t, dir))
			}
		})
	}
}

// The name a download is given by whoever starts it, which for the browser
// extension is still the name the server suggested, and the last part of the
// address are no more to be trusted than Content-Disposition.
func TestNoSourceOfANameYieldsASidecar(t *testing.T) {
	payload := []byte(`{"version":1}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	}))
	defer srv.Close()

	for what, o := range map[string]Options{
		"the address":    {URL: srv.URL + "/files/installer.exe.GODM"},
		"the given name": {URL: srv.URL + "/files/x.bin", Filename: "installer.exe.godm."},
	} {
		o.OutDir = t.TempDir()
		res, err := Download(context.Background(), o)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if wouldBeSidecar(filepath.Base(res.Path)) {
			t.Errorf("%s: saved as %q, a sidecar's name", what, filepath.Base(res.Path))
		}
	}
}

// The stream planner reads the same sidecar path and trusts it the same way:
// one that names its playlist lets it truncate the file and write the stream
// into it.
func TestADownloadCannotPlantASidecarForAStream(t *testing.T) {
	const genuine = "a recording that was already here"
	segs := makeSegments(4)
	s := newHLSServer(t, segs)
	playlist := s.URL + "/media.m3u8"

	for _, name := range sidecarNames("clip.ts") {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(dir, "clip.ts")
			if err := os.WriteFile(victim, []byte(genuine), 0o644); err != nil {
				t.Fatal(err)
			}
			srv := plantServer(t, name, func(string) []byte {
				b, _ := json.Marshal(hlsState{Version: stateVersion, Kind: "hls", Playlist: playlist, Variant: -1, Total: len(segs)})
				return b
			})
			if _, err := Download(context.Background(), Options{URL: srv.URL + "/plant", OutDir: dir}); err != nil {
				t.Fatal(err)
			}
			target, err := DownloadHLS(context.Background(), HLSOptions{
				URL: playlist, OutDir: dir, Filename: "clip.ts", Connections: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(victim); string(got) != genuine {
				t.Errorf("the file that was already there is now %d bytes of the stream", len(got))
			}
			if filepath.Base(target) != "clip (1).ts" {
				t.Errorf("the stream went to %q, want clip (1).ts; the folder holds %q",
					filepath.Base(target), dirNames(t, dir))
			}
		})
	}
}

// A stream takes its name from whoever starts it or from the playlist's
// address, and both reach the planner by a different road from a plain
// download's.
func TestNoStreamIsNamedLikeASidecar(t *testing.T) {
	segs := makeSegments(2)
	s := newHLSServer(t, segs)
	for what, o := range map[string]HLSOptions{
		"the given name": {URL: s.URL + "/media.m3u8", Filename: "clip.ts.GODM"},
		"a name to trim": {URL: s.URL + "/media.m3u8", Filename: "clip.ts.godm. "},
		"the address":    {URL: s.URL + "/clip.ts.godm.m3u8"},
	} {
		o.OutDir, o.Connections = t.TempDir(), 2
		target, err := DownloadHLS(context.Background(), o)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if wouldBeSidecar(filepath.Base(target)) {
			t.Errorf("%s: the stream was saved as %q, a sidecar's name", what, filepath.Base(target))
		}
		if got, _ := os.ReadFile(target); !bytes.Equal(got, joined(segs)) {
			t.Errorf("%s: the saved file does not match the stream", what)
		}
	}
}

// A name that sanitizes to nothing must not become the download folder itself
// as the target, which would put the stream beside the folder.
func TestAStreamWithNoUsableNameStaysInItsFolder(t *testing.T) {
	segs := makeSegments(2)
	s := newHLSServer(t, segs)
	for _, name := range []string{"...", " . ", "bad\xff\xfename"} {
		parent := t.TempDir()
		out := filepath.Join(parent, "Downloads")
		if err := os.Mkdir(out, 0o755); err != nil {
			t.Fatal(err)
		}
		target, err := DownloadHLS(context.Background(), HLSOptions{
			URL: s.URL + "/media.m3u8", Filename: name, OutDir: out, Connections: 2,
		})
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if filepath.Dir(target) != out {
			t.Errorf("%q: the stream was saved as %q, outside %q", name, target, out)
		}
		if got := dirNames(t, parent); len(got) != 1 {
			t.Errorf("%q: the folder holds %v beside the download folder", name, got)
		}
	}
}

// downloadHLSWithin fails the test instead of waiting for ever on a download
// that has stopped making progress without ending.
func downloadHLSWithin(t *testing.T, limit time.Duration, o HLSOptions) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		target string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		target, err := DownloadHLS(ctx, o)
		done <- result{target, err}
	}()
	select {
	case r := <-done:
		return r.target, r.err
	case <-time.After(limit):
		t.Fatalf("the download had not ended after %v", limit)
		return "", nil
	}
}

// A sidecar's numbers decide where the file is cut and which segment is
// fetched next. One that was damaged, edited by hand or written by something
// else must not get that far: a segment index below zero took the daemon down
// with every download in it, and one past the end left the task running for
// ever with nothing to wait for.
func TestAMalformedStreamSidecarIsNotResumedFrom(t *testing.T) {
	segs := makeSegments(4)
	whole := joined(segs)
	initSeg := []byte("FTYPISO6MOOVBOXHERE")
	cases := []struct {
		name string
		file []byte
		st   hlsState
		init bool // the stream has an EXT-X-MAP
	}{
		{name: "a segment index below zero", st: hlsState{Next: -1}},
		{name: "a segment index past the end", file: whole, st: hlsState{Next: 5, Written: int64(len(whole))}},
		{name: "a length below zero", st: hlsState{Written: -1}},
		{name: "segments counted but no bytes", file: whole[:len(segs[0])], st: hlsState{Next: 2}},
		{name: "bytes counted but no segments", file: []byte("not part of the stream"), st: hlsState{Written: 22}},
		{name: "segments without the init segment before them", file: segs[0], st: hlsState{Next: 1, Written: int64(len(segs[0]))}, init: true},
		{name: "an init segment the stream does not have", file: initSeg, st: hlsState{Written: int64(len(initSeg)), InitDone: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHLSServer(t, segs)
			want := whole
			if c.init {
				s.init = initSeg
				want = append(append([]byte{}, initSeg...), whole...)
			}
			dir := t.TempDir()
			o := HLSOptions{URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 2, MaxRetries: 1}
			target := filepath.Join(dir, "clip.ts")
			if err := os.WriteFile(target, c.file, 0o644); err != nil {
				t.Fatal(err)
			}
			st := c.st
			st.Version, st.Kind, st.Playlist, st.Variant, st.Total = stateVersion, "hls", o.URL, -1, len(segs)
			if err := saveHLSState(target, &st); err != nil {
				t.Fatal(err)
			}

			got, err := downloadHLSWithin(t, 10*time.Second, o)
			if err != nil {
				t.Fatalf("the download failed: %v", err)
			}
			// The sidecar names this playlist, so the file is this download's
			// own and is started over where it is.
			if got != target {
				t.Errorf("the stream went to %s, want it started over in %s", filepath.Base(got), filepath.Base(target))
			}
			if b, _ := os.ReadFile(got); !bytes.Equal(b, want) {
				t.Errorf("the file is %d bytes and is not the stream (%d bytes)", len(b), len(want))
			}
			if pathExists(statePath(target)) {
				t.Error("the sidecar is still there after the download finished")
			}
		})
	}
}

// The sidecar is written on a timer, and cleared only after the finished file
// has been flushed to disk. A process that dies in between leaves one that
// counts every segment as written. That is a sound sidecar, and the only work
// left is to say so.
func TestAStreamWithEverySegmentWrittenFinishesWhenResumed(t *testing.T) {
	segs := makeSegments(4)
	whole := joined(segs)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	o := HLSOptions{URL: s.URL + "/media.m3u8", OutDir: dir, Filename: "clip.ts", Connections: 2}
	target := filepath.Join(dir, "clip.ts")
	// Something of a write that never completed is past the counted length.
	if err := os.WriteFile(target, append(append([]byte{}, whole...), "torn"...), 0o644); err != nil {
		t.Fatal(err)
	}
	err := saveHLSState(target, &hlsState{
		Version: stateVersion, Kind: "hls", Playlist: o.URL, Variant: -1,
		Total: len(segs), Next: len(segs), Written: int64(len(whole)),
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var last HLSProgress
	o.OnProgress = func(p HLSProgress) {
		mu.Lock()
		last = p
		mu.Unlock()
	}
	got, err := downloadHLSWithin(t, 10*time.Second, o)
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != target {
		t.Errorf("the stream went to %s instead of finishing %s", filepath.Base(got), filepath.Base(target))
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, whole) {
		t.Errorf("the file is %d bytes, want the %d of the stream", len(b), len(whole))
	}
	if pathExists(statePath(target)) {
		t.Error("the sidecar is still there after the download finished")
	}
	for i := range segs {
		if n := s.hitCount(fmt.Sprintf("/seg%d.ts", i)); n != 0 {
			t.Errorf("segment %d was fetched again; it was already in the file", i)
		}
	}
	if last.Segments != len(segs) || last.Bytes != int64(len(whole)) {
		t.Errorf("the last progress report was %d segments and %d bytes, want it to show the stream complete",
			last.Segments, last.Bytes)
	}
}

// A plain download's sidecar is checked range by range before it is resumed
// from. A range with no end is how a download whose server gave no way to
// resume is recorded; once the server does offer ranges, there is no request
// that asks for "the rest" of it.
func TestASidecarRangeWithoutAnEndIsNotResumedFrom(t *testing.T) {
	payload := makePayload(64 << 10)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()
	dir := t.TempDir()
	o := Options{URL: srv.URL + "/f.bin", OutDir: dir, Connections: 2, MinSplit: 4 << 10, MaxRetries: 1}
	target := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(target, make([]byte, len(payload)), 0o644); err != nil {
		t.Fatal(err)
	}
	err := saveState(target, &State{
		Version: stateVersion, URL: o.URL, Size: int64(len(payload)), ETag: `"fixed-etag"`,
		Segments: []SegSnap{{Start: 0, End: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Download(context.Background(), o)
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	if res.Resumed || res.Path != target {
		t.Errorf("resumed = %v into %s, want the download started over in f.bin", res.Resumed, filepath.Base(res.Path))
	}
	if got, _ := os.ReadFile(res.Path); sum(got) != sum(payload) {
		t.Error("content mismatch")
	}
}
