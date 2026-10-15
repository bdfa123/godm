package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The edges of the yt-dlp update: what happens in the moments when the file is
// not there, when a program does not end, when quitting meets an update, and
// the words the page is given.

// ---------- while yt-dlp is being replaced ----------

// An update takes the old file away before it puts the new one in its place.
// A download that starts in between must wait for the update, as one that
// starts a moment earlier does, and not end in an error for a yt-dlp that is
// only away; an inspection is told so, and so is a second update.
func TestADownloadThatStartsWhileTheFileIsAwayWaitsForTheUpdate(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "ok")
	t.Setenv("PATH", t.TempDir()) // no other yt-dlp to be found
	body, err := os.ReadFile(rig.bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	endUpdate := sync.OnceFunc(ytdlpGate.endUpdate)
	defer endUpdate()
	if err := os.Remove(rig.bin); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := RunYTDLP(context.Background(), YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: t.TempDir()})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the download ended with %v when it should have waited for the update", err)
	case <-time.After(400 * time.Millisecond):
	}

	if _, err := InspectYTDLP(context.Background(), YTDLPOptions{URL: "https://example.test/watch?v=1"}); !errors.Is(err, errYTDLPUpdating) {
		t.Errorf("the inspection was told %v, want that yt-dlp is being updated", err)
	}
	var tool ytTool
	var ref *ytRefusal
	if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "updating" {
		t.Errorf("a second update was refused with %v, want that one is under way", err)
	}

	// The new file is in place and the update is over.
	if err := os.WriteFile(rig.bin, body, 0o755); err != nil {
		t.Fatal(err)
	}
	endUpdate()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the download that waited failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not go on once the update was over")
	}
}

// ---------- what an update leaves to be told ----------

// How the last update went is told for a while, and then it is old news: it
// must not hide a release that came out since.
func TestHowTheLastUpdateWentIsToldForAWhileOnly(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubUpdateTo, "2026.08.19")
	var tool ytTool
	updateAndWait(t, &tool)
	if tool.status().Result == nil {
		t.Fatal("the result is gone as soon as it is there")
	}
	tool.mu.Lock()
	tool.resultAt = time.Now().Add(-2 * ytResultKeep)
	tool.mu.Unlock()
	if tool.status().Result != nil {
		t.Error("an update of long ago is still told as the news")
	}
}

// An update after which yt-dlp does not run is not good news, and the page is
// not told it is.
func TestAnUpdateAfterWhichYTDLPWillNotSayItsVersionIsNoGoodNews(t *testing.T) {
	check := func(t *testing.T, res *ytUpdateResult) {
		t.Helper()
		if res.OK || res.ErrorCode != "after-version" || !strings.Contains(res.Error, "after the update") ||
			!strings.Contains(res.Error, "ModuleNotFoundError") {
			t.Errorf("result = %+v, want a failure that says yt-dlp does not run after the update", *res)
		}
		if res.To != "" || res.Changed {
			t.Errorf("result = %+v: a version that was never read is shown as the one updated to", *res)
		}
	}
	t.Run("yt-dlp -U", func(t *testing.T) {
		newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
		t.Setenv(stubUpdateTo, "crash") // the stand-in is broken from then on
		var tool ytTool
		check(t, updateAndWait(t, &tool).Result)
	})
	t.Run("winget", func(t *testing.T) {
		rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
		rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {To: "crash"}})
		var tool ytTool
		check(t, updateAndWait(t, &tool).Result)
	})
	// A copy that would not run before and does now was repaired: that it was
	// not read before does not make the update a failure, nor "unchanged".
	t.Run("one that did not run before", func(t *testing.T) {
		newUpdateRig(t, ytInstStandalone, "", "")
		t.Setenv(stubUpdateTo, "2026.08.19")
		var tool ytTool
		res := updateAndWait(t, &tool).Result
		if !res.OK || res.From != "" || res.To != "2026.08.19" || !res.Changed {
			t.Errorf("result = %+v", *res)
		}
	})
}

// ---------- looking again after a failure ----------

// A look that failed is remembered for a minute and then asked again, also when
// the release learnt before it is still fresh: else the page keeps saying that
// the newest release could not be found, for hours after GitHub is back.
func TestAFailedLookIsAskedAgainWhenTheTimeToRetryHasCome(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.install(t)
	var tool ytTool
	ctx := context.Background()
	if _, err := tool.newest(ctx, false); err != nil {
		t.Fatal(err)
	}
	gh.set("", errors.New("could not reach GitHub"))
	if _, err := tool.newest(ctx, true); err == nil {
		t.Fatal("the look that was asked for did not fail")
	}
	if tool.status().LatestError == "" {
		t.Fatal("the failure is not remembered")
	}

	// Within the minute nothing is asked again, and the answer from before stands.
	asked := gh.calls.Load()
	if tag, err := tool.newest(ctx, false); tag != "2026.08.19" || err != nil || gh.calls.Load() != asked {
		t.Errorf("newest = %q, %v after %d more questions, want the answer from before and no question", tag, err, gh.calls.Load()-asked)
	}

	// Then GitHub is asked, and what it says now is what the page shows.
	replace(t, &ytLatestRetry, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	gh.set("2026.09.01", nil)
	if tag, err := tool.newest(ctx, false); tag != "2026.09.01" || err != nil {
		t.Errorf("newest = %q, %v, want the new release", tag, err)
	}
	if st := tool.status(); st.LatestError != "" || st.Latest != "2026.09.01" {
		t.Errorf("status = latest %q, error %q: the error outlived the failure", st.Latest, st.LatestError)
	}
	// And a failure that goes on is asked about once in that time, not at every look.
	gh.set("", errors.New("could not reach GitHub"))
	tool.newest(ctx, true)
	asked = gh.calls.Load()
	for range 3 {
		tool.newest(ctx, false)
	}
	if gh.calls.Load() != asked {
		t.Errorf("GitHub was asked %d times within the retry time of a failure", gh.calls.Load()-asked)
	}
}

// ---------- the words the page is given ----------

// A reason godm words itself comes with a code and what it needs said, for the
// page to say it in its language; a reason that is another program's does not.
func TestReasonsGodmWordsCarryACodeForThePage(t *testing.T) {
	reset := time.Now().Add(7 * time.Minute).Unix()
	for name, c := range map[string]struct {
		serve     http.HandlerFunc
		code, arg string
	}{
		"a server error": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "oops", 502) }, "gh-status", "502 Bad Gateway"},
		"the rate limit": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			http.Error(w, "rate limit exceeded", 403)
		}, "gh-limit", "7"},
		"not json": {func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }, "gh-unreadable", ""},
		"no version": {func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"tag_name":"nightly"}`))
		}, "gh-tag", "nightly"},
	} {
		t.Run(name, func(t *testing.T) {
			githubReturning(t, c.serve)
			_, err := fetchLatestYTDLP(context.Background())
			if w := asWhy(err); w.Code != c.code || w.Arg != c.arg || w.Msg == "" {
				t.Errorf("reason = %+v, want code %q and argument %q", w, c.code, c.arg)
			}
		})
	}
	t.Run("GitHub does not answer in time", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		githubReturning(t, func(w http.ResponseWriter, r *http.Request) { <-release })
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := fetchLatestYTDLP(ctx)
		if w := asWhy(err); w.Code != "gh-timeout" {
			t.Errorf("reason = %+v", w)
		}
	})

	// They are in the status and in what the API answers.
	t.Run("in the status", func(t *testing.T) {
		newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
		fail := ytWhy{"gh-status", "502 Bad Gateway", "GitHub answered 502 Bad Gateway"}
		replace(t, &fetchLatestYTDLP, func(context.Context) (string, error) { return "", fail })
		s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
		api := httptest.NewServer(s.routes())
		defer api.Close()
		body := apiCall(t, api.URL, "tok", http.MethodPost, "/api/ytdlp/check?force=1", "")
		if body["latest_error_code"] != "gh-status" || body["latest_error_arg"] != "502 Bad Gateway" ||
			body["latest_error"] != "GitHub answered 502 Bad Gateway" {
			t.Errorf("answer = %v", body)
		}
		// Another program's wording has no code.
		s.mgr.yt = ytTool{}
		replace(t, &fetchLatestYTDLP, func(context.Context) (string, error) { return "", errors.New("something unforeseen") })
		body = apiCall(t, api.URL, "tok", http.MethodPost, "/api/ytdlp/check?force=1", "")
		if _, has := body["latest_error_code"]; has || body["latest_error"] != "something unforeseen" {
			t.Errorf("answer = %v", body)
		}
	})
}

// What winget prints in a language of its own is not cut through a character,
// and a version is not either.
func TestALongLineIsNeverCutThroughACharacter(t *testing.T) {
	// "a" and then characters of three bytes: the cut at 300 bytes falls inside one.
	got := lastSaying("a\n" + "a" + strings.Repeat("更", 200))
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > 310 {
		t.Errorf("a long line came back as %q (%d bytes)", got, len(got))
	}
	v, found := versionIn([]string{"a" + strings.Repeat("版", 40)}, true)
	if !found || !utf8.ValidString(v) || len(v) > 60 {
		t.Errorf("a long version came back as %q (%d bytes)", v, len(v))
	}
}

// ---------- quitting ----------

// Quitting godm while winget or yt-dlp -U is replacing yt-dlp ends them in the
// middle of it and can leave no yt-dlp at all: so it waits, for as long as it
// can be sure to be waited for, and says so.
func TestQuittingWaitsForAnUpdateToEnd(t *testing.T) {
	replace(t, &ytdlpGate, &ytGate{})
	m := NewManager(t.TempDir(), 2)
	var mu sync.Mutex
	var told []string
	m.announce = func(title, text, open string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, title+"|"+text)
	}
	toldCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(told)
	}

	// With no update under way there is nothing to wait for or to say.
	began := time.Now()
	m.finishYTDLPUpdate()
	if time.Since(began) > time.Second || toldCount() != 0 {
		t.Errorf("quitting with no update under way took %v and told %d times", time.Since(began), toldCount())
	}

	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		m.finishYTDLPUpdate()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("quitting did not wait for the update")
	case <-time.After(300 * time.Millisecond):
	}
	if toldCount() != 1 || !strings.Contains(told[0], "Finishing the yt-dlp update") {
		t.Errorf("told %q, want one notice that the update is being waited for", told)
	}
	ytdlpGate.endUpdate()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("quitting did not go on once the update was over")
	}

	// And it does not wait for ever.
	replace(t, &ytQuitWait, 150*time.Millisecond)
	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	defer ytdlpGate.endUpdate()
	began = time.Now()
	m.finishYTDLPUpdate()
	if took := time.Since(began); took < 100*time.Millisecond || took > 3*time.Second {
		t.Errorf("quitting waited %v for an update that did not end, want about %v", took, ytQuitWait)
	}
}

// ---------- Deno ----------

// yt-dlp can be the newest while the Deno it asks for is not, and then the
// update is for Deno alone.
func TestDenoIsUpdatedEvenWhenYTDLPIsTheNewest(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows can end a program with a code as large as winget's")
	}
	rig := newUpdateRig(t, ytInstWinget, "2026.09.15", "")
	rig.withWinget(map[string]wingetStep{
		"yt-dlp.yt-dlp": {Exit: 0x8A15002B, Out: "No available upgrade found.\n"},
		"DenoLand.Deno": {Out: "Successfully installed\n"},
	})
	var tool ytTool
	if tool.status().DenoViaWinget {
		t.Error("the status says Deno came from winget when none is on the computer")
	}
	rig.withDeno()
	if !tool.status().DenoViaWinget {
		t.Error("the status does not say that Deno came from winget")
	}
	res := updateAndWait(t, &tool).Result
	if !res.OK || res.Changed || res.Deno != "updated" {
		t.Errorf("result = %+v, want yt-dlp unchanged and Deno updated", *res)
	}
	if got := rig.calls("winget"); len(got) != 2 {
		t.Errorf("winget was asked %q, want yt-dlp and Deno", got)
	}
	// A copy of another kind has no Deno of winget's to speak of.
	newUpdateRig(t, ytInstStandalone, "2026.09.15", "")
	replace(t, &lookupTool, func(string) (string, error) {
		return `C:\Users\u\AppData\Local\Microsoft\WinGet\Packages\DenoLand.Deno_x\deno.exe`, nil
	})
	if tool.status().DenoViaWinget {
		t.Error("a standalone yt-dlp is said to be updated along with a winget Deno")
	}
}

// ---------- the hint of age ----------

// The first time yt-dlp.exe is started it can take longer than a failure is to
// be held up. That read goes on after the failure has been told without the
// note, and the next failure has it.
func TestASlowFirstVersionReadStillGivesTheNoteLater(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubVersionWait, "900")
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	replace(t, &ytHintBudget, 250*time.Millisecond)
	m := NewManager(t.TempDir(), 2)

	began := time.Now()
	got := m.ytdlpFailureText(&YTDLPFailedError{"yt-dlp failed: boom"})
	if got != "yt-dlp failed: boom" || time.Since(began) > 3*time.Second {
		t.Errorf("reason = %q after %v, want it quickly and with no note while the version is unknown", got, time.Since(began))
	}
	waitFor(t, 10*time.Second, "the version read to finish in the background", func() bool {
		return m.yt.status().Version == "2026.06.01"
	})
	began = time.Now()
	got = m.ytdlpFailureText(&YTDLPFailedError{"yt-dlp failed: boom"})
	if !strings.Contains(got, "out of date (installed 2026.06.01, newest 2026.09.15)") || time.Since(began) > time.Second {
		t.Errorf("reason = %q after %v", got, time.Since(began))
	}
}

// An inspection that ran out of its time was killed, and is not yt-dlp having
// failed because of its age.
func TestAnInspectionThatRanOutOfTimeIsNotBlamedOnAge(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "idler")
	beatFile(t)
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	replace(t, &inspectBudget, 800*time.Millisecond)
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	// Known beforehand, so that nothing but the guard keeps the note away.
	s.mgr.yt.newest(context.Background(), true)
	s.mgr.yt.installedVersion(context.Background(), os.Getenv("GODM_YTDLP"), true)
	api := httptest.NewServer(s.routes())
	defer api.Close()

	body := apiCall(t, api.URL, "tok", http.MethodPost, "/api/inspect", `{"url":"https://example.test/watch?v=1","kind":"yt-dlp"}`)
	if body["ok"] != false || strings.Contains(body["error"].(string), "out of date") {
		t.Errorf("answer = %v", body)
	}
}

// ---------- time allowed ----------

// A program of an update that does not end is ended when the time allowed it is
// up, and the person is told that, not left waiting with every download held
// behind the gate.
func TestAnUpdateThatDoesNotEndIsEndedWhenItsTimeIsUp(t *testing.T) {
	t.Run("yt-dlp -U", func(t *testing.T) {
		newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
		t.Setenv(stubHang, "update")
		replace(t, &ytUpdateTimeout, 500*time.Millisecond)
		var tool ytTool
		res := updateAndWait(t, &tool).Result
		if res.OK || res.ErrorCode != "timeout-update" || !strings.Contains(res.Error, "did not finish") {
			t.Errorf("result = %+v", *res)
		}
		if ytdlpGate.isUpdating() {
			t.Error("the gate stayed shut")
		}
	})
	t.Run("winget", func(t *testing.T) {
		rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
		rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {Hang: true}})
		replace(t, &ytUpdateTimeout, 500*time.Millisecond)
		var tool ytTool
		res := updateAndWait(t, &tool).Result
		if res.OK || res.ErrorCode != "timeout-winget" || !strings.Contains(res.Error, "winget did not finish") {
			t.Errorf("result = %+v", *res)
		}
	})
}

// yt-dlp that does not say its version is not waited for for ever either.
func TestAVersionThatIsNotSaidInTimeIsSaidSo(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubHang, "version")
	replace(t, &ytVersionBudget, 400*time.Millisecond)
	var tool ytTool
	began := time.Now()
	v, err := tool.installedVersion(context.Background(), rig.bin, true)
	if err == nil || v != "" || time.Since(began) > 8*time.Second {
		t.Fatalf("version = %q, %v after %v", v, err, time.Since(began))
	}
	if st := tool.status(); st.VersionErrorCode != "version-timeout" || !strings.Contains(st.VersionError, "in time") {
		t.Errorf("status = error %q, code %q", st.VersionError, st.VersionErrorCode)
	}
	if n := ytdlpGate.inUse(); n != 0 {
		t.Errorf("%d users of yt-dlp are left after the version read timed out", n)
	}
}

// A check answers within its budget when GitHub does not answer.
func TestACheckIsAnsweredWithinItsBudgetWhenGitHubDoesNotAnswer(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	var gh fakeGitHub
	gh.hold = make(chan struct{})
	gh.install(t)
	defer close(gh.hold)
	replace(t, &ytCheckBudget, 300*time.Millisecond)
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()

	answered := make(chan map[string]any, 1)
	go func() { answered <- apiCall(t, api.URL, "tok", http.MethodPost, "/api/ytdlp/check", "") }()
	select {
	case body := <-answered:
		if body["ok"] != true || body["version"] != "2026.06.01" {
			t.Errorf("answer = %v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check was not answered while GitHub did not answer")
	}
}
