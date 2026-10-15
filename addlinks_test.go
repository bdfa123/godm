package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// postJob sends body to one of the two addresses that add downloads, the way
// the manager page and the native host do, and returns the id of the download
// it made.
func postJob(t *testing.T, m *Manager, path, body string) string {
	t.Helper()
	s := &server{mgr: m}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if path == "/api/batch" {
		s.handleBatch(rec, req)
	} else {
		s.handleDownload(rec, req)
	}
	var out struct {
		ID  string   `json:"id"`
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s answered %d %q", path, rec.Code, rec.Body.String())
	}
	if out.ID == "" && len(out.IDs) == 1 {
		out.ID = out.IDs[0]
	}
	if out.ID == "" {
		t.Fatalf("%s made no download: %d %q", path, rec.Code, rec.Body.String())
	}
	return out.ID
}

// The manager's Add links dialog sends an address and a connection count and
// nothing else. For a master playlist that has to mean the best quality, as it
// does when the same link comes through the browser extension, and not the
// first one the playlist happens to list, which is usually the worst.
func TestALinkAddedWithoutAQualityGetsTheBest(t *testing.T) {
	cases := []struct {
		name, path, body, want string
	}{
		// Word for word what the page posts.
		{"add links", "/api/batch", `{"items":[{"url":"URL","connections":8}]}`, "1080p · 5.2 Mbps"},
		{"one download", "/api/download", `{"url":"URL","connections":8}`, "1080p · 5.2 Mbps"},
		{"the best by name", "/api/download", `{"url":"URL","variant":-1}`, "1080p · 5.2 Mbps"},
		// A quality that was chosen is still the one fetched, the first too.
		{"the first, chosen", "/api/download", `{"url":"URL","variant":0}`, "360p · 0.8 Mbps"},
		{"the first, chosen, in a batch", "/api/batch", `{"items":[{"url":"URL","variant":0}]}`, "360p · 0.8 Mbps"},
		{"the second, chosen", "/api/download", `{"url":"URL","variant":1}`, "720p · 2.4 Mbps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHLSServer(t, makeSegments(4))
			m := NewManager(t.TempDir(), 2)
			m.announce = nil
			id := postJob(t, m, c.path, strings.ReplaceAll(c.body, "URL", s.URL+"/master.m3u8"))
			waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
			if v, _ := findTask(m, id); v.Quality != c.want {
				t.Errorf("quality = %q, want %q", v.Quality, c.want)
			}
		})
	}
}

// What the native host sends is a jobRequest written out by this same program,
// and the quality picked in the extension's popup is in it. The first one is
// number 0, which must survive the trip: left out, it would now read as "the
// best".
func TestAChosenQualityIsAlwaysWrittenOut(t *testing.T) {
	for _, variant := range []int{0, 1, -1} {
		b, err := json.Marshal(jobRequest{URL: "https://h.example/master.m3u8", Variant: variant})
		if err != nil {
			t.Fatal(err)
		}
		var back addRequest
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if got := back.job().Variant; got != variant {
			t.Errorf("variant %d was sent as %s and read as %d", variant, b, got)
		}
	}
}

// A list saved by an earlier godm wrote the first quality as nothing at all.
// A stream in it that was begun in that quality has to stay in it: read as
// "the best", its next run would find segments of another quality saved and
// start over.
func TestASavedStreamKeepsTheQualityItWasBegunIn(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	old := `{"version":1,"limit":3,"tasks":[{"view":{"id":"s1","url":"https://h.example/master.m3u8",` +
		`"filename":"show.ts","path":"","state":"paused","size":-1,"received":0,"speed":0,"conns":8,` +
		`"active":0,"resumable":true,"kind":"hls","added_at":"2026-01-02T03:04:05Z"},` +
		`"req":{"url":"https://h.example/master.m3u8","filename":"","headers":null,"connections":8,"out_dir":""},` +
		`"out_dir":"` + filepath.ToSlash(dir) + `"}]}`
	if err := os.WriteFile(store, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir, 1)
	m.store = store
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	mt := m.get("s1")
	if mt == nil {
		t.Fatal("the saved stream was not restored")
	}
	if mt.req.Variant != 0 {
		t.Errorf("restored with variant %d, want 0, the first one listed, which is what it was saved as", mt.req.Variant)
	}
}

// The extension's link picker lists the streams a page played with its links
// and sends them in one batch, each stream marked as one. The daemon has always
// read a kind for every item of a batch, as it does for a single download, and
// this is what the picker relies on: a playlist that is not known by an address
// ending in .m3u8 (a player is as often given /play?id=7) goes to the stream
// engine only because the item says so, and takes the best quality as a stream
// added with no choice does. The items are judged one by one, so a stream the
// daemon will not take is refused with the same words as a single download
// would be, and the rest of the batch still goes through.
func TestABatchItemCanBeAStream(t *testing.T) {
	hls := newHLSServer(t, makeSegments(4))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/play" {
			r.URL.Path = "/master.m3u8"
		}
		hls.serve(w, r)
	}))
	t.Cleanup(front.Close)
	play := front.URL + "/play?id=7"

	postBatch := func(m *Manager, body string) (ids, errs []string) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&server{mgr: m}).handleBatch(rec, httptest.NewRequest(http.MethodPost, "/api/batch", strings.NewReader(body)))
		var out struct {
			IDs    []string `json:"ids"`
			Errors []string `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("/api/batch answered %d %q", rec.Code, rec.Body.String())
		}
		return out.IDs, out.Errors
	}

	t.Run("marked as a stream", func(t *testing.T) {
		m := NewManager(t.TempDir(), 2)
		m.announce = nil
		ids, errs := postBatch(m, `{"items":[`+
			`{"url":"ftp://files.test/film.m3u8","kind":"hls"},`+
			`{"url":"`+play+`","kind":"hls","filename":"The Film","connections":4}]}`)
		if len(ids) != 1 || len(errs) != 1 {
			t.Fatalf("ids = %v, errors = %v; want the stream taken and the ftp address refused", ids, errs)
		}
		if !strings.Contains(errs[0], "only http and https URLs are supported") {
			t.Errorf("the refusal reads %q, not as a single download's does", errs[0])
		}
		waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, ids[0], StateDone))
		v, _ := findTask(m, ids[0])
		if v.Kind != "hls" || v.Quality != "1080p · 5.2 Mbps" {
			t.Errorf("kind = %q, quality = %q; want the stream engine and the best quality", v.Kind, v.Quality)
		}
		if got, want := filepath.Base(v.Path), "The Film.ts"; got != want {
			t.Errorf("saved as %q, want %q", got, want)
		}
	})

	t.Run("not marked", func(t *testing.T) {
		// What the same address is without the mark: a plain file, the playlist
		// itself. It is why the mark has to survive the way here.
		m := NewManager(t.TempDir(), 2)
		m.announce = nil
		ids, _ := postBatch(m, `{"items":[{"url":"`+play+`"}]}`)
		if len(ids) != 1 {
			t.Fatalf("ids = %v", ids)
		}
		waitFor(t, 30*time.Second, "the download to finish", stateIs(m, ids[0], StateDone))
		if v, _ := findTask(m, ids[0]); v.Kind == "hls" {
			t.Errorf("an address that does not say it is a stream was run as one")
		}
	})
}
