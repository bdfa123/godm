package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// put writes files, named by their path from dir, with the text they hold.
func put(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tree lists the files below dir, slash-separated and sorted.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	out := filesUnder(t, dir)
	for i := range out {
		out[i] = filepath.ToSlash(out[i])
	}
	slices.Sort(out)
	return out
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAYTDLPDownloadHasAFolderInsideTheDownloadFolder(t *testing.T) {
	out := t.TempDir()
	a, b := ytdlpPlaceFor(out, "t1792060197602-7"), ytdlpPlaceFor(out, "t1792060197602-8")
	if a.root == b.root {
		t.Fatal("two downloads share a folder")
	}
	for _, p := range []ytdlpPlace{a, b} {
		if filepath.Dir(p.root) != out || !strings.HasPrefix(filepath.Base(p.root), ".godm-") {
			t.Errorf("the folder is %s, want a .godm-… folder directly in %s", p.root, out)
		}
		if filepath.Dir(p.files) != p.root || filepath.Dir(p.work) != p.root || p.files == p.work {
			t.Errorf("the finished files (%s) and the rest (%s) are to be two folders of %s", p.files, p.work, p.root)
		}
	}
	// The ids come back from a file anyone can edit, and a folder is named from them.
	for _, id := range []string{`..\..\..\Windows`, "../../etc", "a/b", `a\b`, "", "..", "x:y"} {
		p := ytdlpPlaceFor(out, id)
		if rel, err := filepath.Rel(out, p.root); err != nil || !filepath.IsLocal(rel) || strings.ContainsAny(rel, `/\`) {
			t.Errorf("id %q put the folder at %s, which is not directly inside %s", id, p.root, out)
		}
	}
}

func TestSettlingMovesTheResultOutAndClearsTheFolder(t *testing.T) {
	out := t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Film [x].mp4": "the film"})
	// What an earlier try at another quality left: not a result, and not the
	// download folder's.
	put(t, place.work, map[string]string{
		"Film [x].f137.mp4.part": "part", "Film [x].f137.mp4.ytdl": "notes", "Film [x].f140.m4a": "stream",
	})
	got, err := place.settle(out, filepath.Join(place.files, "Film [x].mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "Film [x].mp4"); got != want {
		t.Errorf("the file is at %s, want %s", got, want)
	}
	if left := tree(t, out); !slices.Equal(left, []string{"Film [x].mp4"}) {
		t.Errorf("the download folder holds %q, want the film alone", left)
	}
	if readText(t, got) != "the film" {
		t.Error("the film was changed")
	}
	if exists(place.root) {
		t.Error("the download's own folder is still there")
	}
}

// A name that is taken is not written over, whoever took it.
func TestSettlingNeverWritesOverAFileThatIsThere(t *testing.T) {
	out := t.TempDir()
	put(t, out, map[string]string{"Film.mp4": "somebody's film"})
	for n, want := range []string{"Film (1).mp4", "Film (2).mp4", "Film (3).mp4"} {
		place := ytdlpPlaceFor(out, "t"+want)
		put(t, place.files, map[string]string{"Film.mp4": "download " + want})
		got, err := place.settle(out, filepath.Join(place.files, "Film.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != want || readText(t, got) != "download "+want {
			t.Errorf("download %d went to %s (%q), want %s", n, got, readText(t, got), want)
		}
	}
	if readText(t, filepath.Join(out, "Film.mp4")) != "somebody's film" {
		t.Error("the file that was there was written over")
	}
	if left := tree(t, out); len(left) != 4 {
		t.Errorf("the download folder holds %q, want four films", left)
	}
}

// The configuration of the user's own may ask for the subtitles, the
// thumbnail, the description: they come out beside the film, and stay named
// after it when the film is not called what it was going to be.
func TestSettlingKeepsWhatWentWithTheFileBesideIt(t *testing.T) {
	files := map[string]string{
		"Film.mp4": "film", "Film.en.vtt": "subtitles", "Film.info.json": "{}", "Film.jpg": "picture",
		"Other/Elsewhere.txt": "elsewhere", "Filmography.txt": "not named after the film",
	}
	t.Run("nothing in the way", func(t *testing.T) {
		out := t.TempDir()
		place := ytdlpPlaceFor(out, "t1")
		put(t, place.files, files)
		if _, err := place.settle(out, filepath.Join(place.files, "Film.mp4")); err != nil {
			t.Fatal(err)
		}
		want := []string{"Film.en.vtt", "Film.info.json", "Film.jpg", "Film.mp4", "Filmography.txt", "Other/Elsewhere.txt"}
		if got := tree(t, out); !slices.Equal(got, want) {
			t.Errorf("the download folder holds %q, want %q", got, want)
		}
	})
	t.Run("the film's name is taken", func(t *testing.T) {
		out := t.TempDir()
		put(t, out, map[string]string{"Film.mp4": "mine"})
		place := ytdlpPlaceFor(out, "t1")
		put(t, place.files, files)
		got, err := place.settle(out, filepath.Join(place.files, "Film.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != "Film (1).mp4" {
			t.Errorf("the film is %s, want Film (1).mp4", got)
		}
		// What is not named after the film keeps its name, which was free.
		want := []string{
			"Film (1).en.vtt", "Film (1).info.json", "Film (1).jpg", "Film (1).mp4", "Film.mp4",
			"Filmography.txt", "Other/Elsewhere.txt",
		}
		if tr := tree(t, out); !slices.Equal(tr, want) {
			t.Errorf("the download folder holds %q, want %q", tr, want)
		}
	})
	t.Run("only a subtitle's name is taken", func(t *testing.T) {
		out := t.TempDir()
		put(t, out, map[string]string{"Film.en.vtt": "somebody's subtitles"})
		place := ytdlpPlaceFor(out, "t1")
		put(t, place.files, map[string]string{"Film.mp4": "film", "Film.en.vtt": "subtitles"})
		got, err := place.settle(out, filepath.Join(place.files, "Film.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		// The film and its subtitles keep each other's name; the film does not
		// move in with the subtitles that were there before.
		if filepath.Base(got) != "Film (1).mp4" || readText(t, filepath.Join(out, "Film (1).en.vtt")) != "subtitles" {
			t.Errorf("the film is %s, and the folder holds %q", got, tree(t, out))
		}
		if readText(t, filepath.Join(out, "Film.en.vtt")) != "somebody's subtitles" {
			t.Error("the subtitles that were there were written over")
		}
	})
}

// Another download of godm's may have chosen the name and not made the file
// yet.
func TestSettlingKeepsAwayFromANameAnotherDownloadHasChosen(t *testing.T) {
	out := t.TempDir()
	release, ok := reserveTarget(filepath.Join(out, "Film.mp4"))
	if !ok {
		t.Fatal("could not reserve the name")
	}
	defer release()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Film.mp4": "film"})
	got, err := place.settle(out, filepath.Join(place.files, "Film.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "Film (1).mp4" {
		t.Errorf("the film is %s, want Film (1).mp4", got)
	}
	// And the names it held are let go when it is done.
	if _, ok := reserveTarget(got); !ok {
		t.Error("the name of the moved file is still held")
	}
}

func TestSettlingLeavesAFileYTDLPWroteElsewhere(t *testing.T) {
	out, elsewhere := t.TempDir(), t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.work, map[string]string{"leftover.part": "part"})
	put(t, elsewhere, map[string]string{"Film.mp4": "film"})
	path := filepath.Join(elsewhere, "Film.mp4")
	got, err := place.settle(out, path)
	if err != nil || got != path {
		t.Fatalf("settle = %q, %v, want %s untouched", got, err, path)
	}
	if readText(t, path) != "film" {
		t.Error("the file was changed")
	}
	if exists(place.root) {
		t.Error("the download's own folder is still there")
	}
}

func TestSettlingWillNotClaimAFileThatIsNotThere(t *testing.T) {
	out := t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Other.mp4": "film"})
	put(t, place.work, map[string]string{"Film.f1.mp4.part": "part"})
	if got, err := place.settle(out, filepath.Join(place.files, "Film.mp4")); err == nil {
		t.Fatalf("settle returned %s for a file that is not there", got)
	}
	// Nothing was moved or deleted: the next try finds what is there.
	if got := tree(t, place.root); !slices.Equal(got, []string{"files/Other.mp4", "work/Film.f1.mp4.part"}) {
		t.Errorf("the download's folder holds %q", got)
	}
	if got := tree(t, out); len(got) != 2 {
		t.Errorf("the download folder holds %q", got)
	}
}

// If a place for one of the files cannot be made, nothing has been moved and no
// name is held.
func TestSettlingThatFailsMovesNothing(t *testing.T) {
	out := t.TempDir()
	// A file where a folder of the result's is to be.
	put(t, out, map[string]string{"Sub": "a file, not a folder"})
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Film.mp4": "film", "Sub/Film.en.vtt": "subtitles"})
	if got, err := place.settle(out, filepath.Join(place.files, "Film.mp4")); err == nil {
		t.Fatalf("settle returned %s though a folder could not be made", got)
	}
	if got := tree(t, out); !slices.Equal(got, []string{".godm-t1/files/Film.mp4", ".godm-t1/files/Sub/Film.en.vtt", "Sub"}) {
		t.Errorf("the download folder holds %q", got)
	}
	if _, ok := reserveTarget(filepath.Join(out, "Film.mp4")); !ok {
		t.Error("a name is still held after the failure")
	}
}

// ---------- through the manager ----------

func ytdlpManager(t *testing.T, dir string) *Manager {
	t.Helper()
	stubYTDLP(t, "ok")
	t.Setenv("GODM_YTDLP", os.Args[0])
	m := NewManager(dir, 2)
	m.announce = nil
	return m
}

func TestAFinishedYTDLPDownloadIsInTheDownloadFolderAlone(t *testing.T) {
	dir := t.TempDir()
	m := ytdlpManager(t, dir)
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if want := filepath.Join(dir, "A Video Title.mp4"); v.Path != want {
		t.Errorf("path = %s, want %s", v.Path, want)
	}
	if readText(t, v.Path) != standInVideo {
		t.Error("the file is not what yt-dlp made")
	}
	waitFor(t, 5*time.Second, "the download's own folder to go", func() bool {
		rest, _ := filepath.Glob(filepath.Join(dir, ".godm-*"))
		return len(rest) == 0
	})
	if got := tree(t, dir); !slices.Equal(got, []string{"A Video Title.mp4"}) {
		t.Errorf("the download folder holds %q, want the video alone", got)
	}
	if !m.Remove(id, true) {
		t.Fatal("could not remove the download")
	}
	waitFor(t, 10*time.Second, "the file to be deleted", func() bool { return !exists(v.Path) })
}

// Adding a video that is in the folder already is another download, as it is
// for any other file: it was reported done at once, with the path of the file
// that was there, and removing it with its file deleted the first download's.
func TestAYTDLPDownloadOfWhatIsAlreadyThereMakesAnotherCopy(t *testing.T) {
	dir := t.TempDir()
	m := ytdlpManager(t, dir)
	original := filepath.Join(dir, "A Video Title.mp4")
	put(t, dir, map[string]string{"A Video Title.mp4": "the first download, or the user's own"})

	add := func() TaskView {
		t.Helper()
		id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
		v, _ := findTask(m, id)
		return v
	}
	second, third := add(), add()
	if want := filepath.Join(dir, "A Video Title (1).mp4"); second.Path != want {
		t.Errorf("the second download is at %s, want %s", second.Path, want)
	}
	if want := filepath.Join(dir, "A Video Title (2).mp4"); third.Path != want {
		t.Errorf("the third download is at %s, want %s", third.Path, want)
	}
	if readText(t, second.Path) != standInVideo || readText(t, third.Path) != standInVideo {
		t.Error("the copies are not what yt-dlp made")
	}
	if readText(t, original) != "the first download, or the user's own" {
		t.Error("the file that was there was changed")
	}

	if !m.Remove(second.ID, true) {
		t.Fatal("could not remove the second download")
	}
	waitFor(t, 10*time.Second, "the copy to be deleted", func() bool { return !exists(second.Path) })
	if !exists(original) || !exists(third.Path) {
		t.Errorf("removing a download with its file deleted another's: the folder holds %q", tree(t, dir))
	}
}

// A removal that comes just as yt-dlp finishes: the file it made is not to be
// moved out of the folder that is about to go.
func TestRemovedWithItsFilesAsYTDLPFinishesLeavesNothing(t *testing.T) {
	out := t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Film.mp4": "film"})
	finished := filepath.Join(place.files, "Film.mp4")

	mt := &managedTask{
		view:   TaskView{ID: "t1", State: StateRunning, Kind: "yt-dlp"},
		req:    jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp"},
		outDir: out,
	}
	mt.deleteAfter = true
	if got, err := mt.settleYTDLP(place, out, finished); err != errRemovedWithFiles {
		t.Fatalf("settleYTDLP = %q, %v, want to be told the download was removed", got, err)
	}
	if got := tree(t, out); !slices.Equal(got, []string{".godm-t1/files/Film.mp4"}) {
		t.Errorf("the download folder holds %q: the file was moved out of the folder to be deleted", got)
	}
	mt.discardYTDLP()
	waitFor(t, 5*time.Second, "the folder to go", func() bool { return len(tree(t, out)) == 0 })
}

func TestSettlingTellsTheTaskWhereTheFileIs(t *testing.T) {
	out := t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	put(t, place.files, map[string]string{"Film.mp4": "film"})
	mt := &managedTask{
		view:   TaskView{ID: "t1", State: StateRunning, Kind: "yt-dlp"},
		req:    jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp"},
		outDir: out,
	}
	got, err := mt.settleYTDLP(place, out, filepath.Join(place.files, "Film.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	// Known before the run has ended, so that a removal that comes in between
	// deletes the file where it is now.
	if v := mt.snapshot(); v.Path != got || got != filepath.Join(out, "Film.mp4") {
		t.Errorf("path = %q, settled at %q", v.Path, got)
	}
}

// godm can be told to download into a folder named from where it is started,
// and yt-dlp, which is not started there, names everything in full. A folder of
// the download's that was not in full would not be recognised as the one
// the finished file is in.
func TestARelativeDownloadFolderIsSpelledOutInFull(t *testing.T) {
	here := t.TempDir()
	t.Chdir(here)
	place := ytdlpPlaceFor("downloads", "t1")
	if !filepath.IsAbs(place.root) || !filepath.IsAbs(place.files) || !filepath.IsAbs(place.work) {
		t.Fatalf("the folders are %s, %s and %s", place.root, place.files, place.work)
	}
	put(t, place.files, map[string]string{"Film.mp4": "film"})
	got, err := place.settle("downloads", filepath.Join(place.files, "Film.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !sameFolder(filepath.Dir(got), filepath.Join(here, "downloads")) || readText(t, got) != "film" {
		t.Errorf("the film is at %s, want it in %s", got, filepath.Join(here, "downloads"))
	}
	if exists(place.root) {
		t.Error("the download's own folder is still there")
	}
}

// Not finding the finished file where it was expected is not a reason to
// delete the folder it may be in.
func TestSettlingDoesNotDeleteAFolderAroundAFileItDoesNotUnderstand(t *testing.T) {
	out := t.TempDir()
	place := ytdlpPlaceFor(out, "t1")
	// In the download's folder, but where unfinished files go.
	put(t, place.work, map[string]string{"Film.mp4": "film"})
	if got, err := place.settle(out, filepath.Join(place.work, "Film.mp4")); err == nil {
		t.Fatalf("settle returned %s", got)
	}
	if !exists(filepath.Join(place.work, "Film.mp4")) {
		t.Error("the file was deleted")
	}
}
