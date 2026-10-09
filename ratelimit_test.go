package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// consume pays for total bytes in chunk-sized pieces from several goroutines
// at once, the way a download's connections share a limit.
func consume(t *testing.T, l *RateLimiter, workers, chunk, total int) time.Duration {
	t.Helper()
	var left atomic.Int64
	left.Store(int64(total))
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for left.Add(-int64(chunk)) >= 0 {
				if err := l.WaitN(context.Background(), chunk); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func within(t *testing.T, what string, got, want time.Duration, tolerance float64) {
	t.Helper()
	lo := time.Duration(float64(want) * (1 - tolerance))
	hi := time.Duration(float64(want) * (1 + tolerance))
	if got < lo || got > hi {
		t.Errorf("%s took %v, want about %v", what, got.Round(time.Millisecond), want)
	}
}

func TestRateLimiterKeepsToTheRate(t *testing.T) {
	l := NewRateLimiter(1 << 20)
	d := consume(t, l, 4, 16<<10, 3<<19) // 1.5 MiB at 1 MiB/s
	within(t, "1.5 MiB at 1 MiB/s", d, 1500*time.Millisecond, 0.15)
}

func TestRateLimiterWithoutALimitCostsNothing(t *testing.T) {
	var zero RateLimiter
	var none *RateLimiter
	ls := rateLimits{&zero, none, NewRateLimiter(0)}
	if got := ls.chunk(readBufSize); got != readBufSize {
		t.Errorf("an unlimited transfer reads %d bytes at a time, want the whole %d buffer", got, readBufSize)
	}
	start := time.Now()
	for i := 0; i < 1_000_000; i++ {
		if err := ls.wait(context.Background(), 1<<30, false); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("a million waits with no limit took %v", d)
	}
}

func TestRateLimiterRateChangesWhileWaiting(t *testing.T) {
	waitFor := func(l *RateLimiter, n int) <-chan time.Duration {
		done := make(chan time.Duration, 1)
		start := time.Now()
		go func() {
			l.WaitN(context.Background(), n)
			done <- time.Since(start)
		}()
		return done
	}
	expect := func(what string, done <-chan time.Duration, most time.Duration) {
		t.Helper()
		select {
		case d := <-done:
			if d > most {
				t.Errorf("%s: the waiter returned after %v", what, d)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the waiter was still waiting at the old rate", what)
		}
	}

	// 1 MiB at 64 KiB/s is sixteen seconds; raised to 8 MiB/s it is gone in
	// a fraction of one.
	l := NewRateLimiter(64 << 10)
	done := waitFor(l, 1<<20)
	time.Sleep(100 * time.Millisecond)
	l.SetRate(8 << 20)
	expect("raised", done, time.Second)

	// Lifting the limit lets everyone waiting go at once.
	l = NewRateLimiter(64 << 10)
	done = waitFor(l, 1<<20)
	time.Sleep(100 * time.Millisecond)
	l.SetRate(0)
	expect("lifted", done, 500*time.Millisecond)

	// Lowering it slows what comes after.
	l = NewRateLimiter(8 << 20)
	consume(t, l, 2, 16<<10, 1<<20)
	l.SetRate(512 << 10)
	d := consume(t, l, 2, 16<<10, 512<<10)
	within(t, "512 KiB after lowering to 512 KiB/s", d, time.Second, 0.2)
}

// Bytes a player is waiting on go ahead of the connections fetching further
// on, and the limit still holds for all of them together.
func TestRateLimiterLetsWhatAPlayerWaitsOnGoFirst(t *testing.T) {
	l := NewRateLimiter(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rest atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l.WaitN(ctx, 16<<10) == nil {
				rest.Add(16 << 10)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)

	// With an equal share among five this would take two and a half seconds.
	first := time.Now()
	for got := 0; got < 512<<10; got += 16 << 10 {
		if err := l.WaitFirst(ctx, 16<<10); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(first); d > 800*time.Millisecond {
		t.Errorf("512 KiB in the first line took %v at 1 MiB/s", d)
	}
	time.Sleep(300 * time.Millisecond)
	elapsed := time.Since(start)
	total := rest.Load() + 512<<10
	cancel()
	wg.Wait()
	if rate := float64(total) / elapsed.Seconds(); rate > 1.15*(1<<20) || rate < 0.85*(1<<20) {
		t.Errorf("both lines together ran at %.0f KiB/s, want about 1024", rate/1024)
	}
}

func TestRateLimiterStopsWaitingWhenCancelled(t *testing.T) {
	l := NewRateLimiter(1 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.WaitN(ctx, 1<<20); err == nil {
		t.Fatal("a wait of seventeen minutes returned without error")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancelling took %v to be noticed", d)
	}
}

func TestDownloadKeepsToASpeedLimit(t *testing.T) {
	payload := makePayload(3 << 19)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()
	dir := t.TempDir()

	start := time.Now()
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/limited.bin", OutDir: dir, Connections: 4,
		Limiters: []*RateLimiter{NewRateLimiter(1 << 20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	within(t, "1.5 MiB at 1 MiB/s over four connections", time.Since(start), 1500*time.Millisecond, 0.25)
	checkFile(t, res.Path, payload)
}

func TestHLSKeepsToASpeedLimit(t *testing.T) {
	segs := make([][]byte, 6)
	for i := range segs {
		segs[i] = bytes.Repeat([]byte{byte(i + 1)}, 128<<10)
	}
	s := newHLSServer(t, segs)

	start := time.Now()
	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: s.URL + "/media.m3u8", OutDir: t.TempDir(), Filename: "clip.ts", Connections: 3,
		Limiters: []*RateLimiter{NewRateLimiter(512 << 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	within(t, "768 KiB of segments at 512 KiB/s", time.Since(start), 1500*time.Millisecond, 0.25)
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, joined(segs)) {
		t.Fatal("the limited stream download is not the segments in order")
	}
}

// The limit all downloads share and a download's own both apply; whichever
// is stricter decides.
func TestManagerSpeedLimitsCombine(t *testing.T) {
	payload := makePayload(768 << 10)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()

	for _, c := range []struct {
		name         string
		global, task int64
	}{
		{"global is stricter", 512 << 10, 4 << 20},
		{"task is stricter", 4 << 20, 512 << 10},
	} {
		m := NewManager(t.TempDir(), 2)
		m.announce = nil
		m.SetSpeedLimit(c.global)
		id, err := m.Add(jobRequest{URL: srv.URL + "/f.bin", Connections: 4, SpeedLimit: c.task})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 20*time.Second, "completion", stateIs(m, id, StateDone))
		v, _ := findTask(m, id)
		within(t, c.name+": 768 KiB with a 512 KiB/s limit", v.EndedAt.Sub(v.StartedAt), 1500*time.Millisecond, 0.3)
		checkFile(t, v.Path, payload)
		if v.SpeedLimit != c.task {
			t.Errorf("%s: the task shows a limit of %d, want %d", c.name, v.SpeedLimit, c.task)
		}
	}
}

func TestTaskSpeedLimitChangesWhileRunning(t *testing.T) {
	payload := makePayload(2 << 20)
	srv := httptest.NewServer(&rangedHandler{payload: payload})
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	// Eight seconds at this rate.
	id, _ := m.Add(jobRequest{URL: srv.URL + "/f.bin", Connections: 4, SpeedLimit: 256 << 10})
	waitFor(t, 10*time.Second, "some progress", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 64<<10
	})
	time.Sleep(300 * time.Millisecond)
	if v, _ := findTask(m, id); v.State == StateDone || v.Received > 1<<20 {
		t.Fatalf("%d bytes arrived under a 256 KiB/s limit before it was lifted", v.Received)
	}
	start := time.Now()
	if _, err := m.SetTaskSpeedLimit(id, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "completion", stateIs(m, id, StateDone))
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("lifting the limit mid-download still took %v to finish", d)
	}
	v, _ := findTask(m, id)
	checkFile(t, v.Path, payload)
}

func TestSpeedLimitsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	m := NewManager(dir, 2)
	m.store = store
	m.SetSpeedLimit(5 << 20)
	m.tasks["a"] = &managedTask{
		view:   TaskView{ID: "a", URL: "https://h.example/a", State: StatePaused, AddedAt: time.Now()},
		req:    jobRequest{URL: "https://h.example/a", SpeedLimit: 512 << 10},
		outDir: dir,
	}
	m.order = append(m.order, "a")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 2)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if got := m2.SpeedLimit(); got != 5<<20 {
		t.Errorf("the shared limit came back as %d, want %d", got, 5<<20)
	}
	mt := m2.tasks["a"]
	if mt == nil {
		t.Fatal("the task was not restored")
	}
	if mt.speed.Rate() != 512<<10 || mt.view.SpeedLimit != 512<<10 {
		t.Errorf("the task's limit came back as %d (shown %d), want %d", mt.speed.Rate(), mt.view.SpeedLimit, 512<<10)
	}
}

func TestSpeedEndpoint(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.mu.Lock()
	m.running = m.limit // keep tasks queued; nothing here needs the network
	m.mu.Unlock()
	id, err := m.Add(jobRequest{URL: "https://example.invalid/f.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Pause(id)
	s := &server{mgr: m, token: "tok"}
	call := func(h http.HandlerFunc, target string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, target, nil))
		var out map[string]any
		json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out
	}

	if _, r := call(s.handleSpeed, "/api/speed?n=2097152"); r["ok"] != true || r["speed_limit"] != float64(2<<20) {
		t.Errorf("setting the shared limit: %v", r)
	}
	if _, r := call(s.handleSpeed, "/api/speed?id="+id+"&n=524288"); r["ok"] != true || r["speed_limit"] != float64(512<<10) {
		t.Errorf("setting the task's limit: %v", r)
	}
	if _, r := call(s.handleSpeed, "/api/speed?id="+id+"&n=-5"); r["speed_limit"] != float64(0) {
		t.Errorf("a negative limit should mean none: %v", r)
	}
	if _, r := call(s.handleSpeed, "/api/speed?id=nope&n=1"); r["ok"] != false {
		t.Errorf("an unknown task: %v", r)
	}
	if code, _ := call(s.handleSpeed, "/api/speed?n=fast"); code != http.StatusBadRequest {
		t.Errorf("a limit that is not a number got status %d", code)
	}

	call(s.handleSpeed, "/api/speed?id="+id+"&n=1048576")
	_, r := call(s.handleTasks, "/api/tasks")
	if r["speed_limit"] != float64(2<<20) {
		t.Errorf("the task list reports a shared limit of %v", r["speed_limit"])
	}
	tasks, _ := r["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["speed_limit"] != float64(1<<20) {
		t.Errorf("the task list does not show the task's own limit: %v", tasks)
	}
}

// A player reading a limited download gets the bytes it asks for at the full
// limit while the rest arrives, not at one connection's share of it.
func TestStreamWorksUnderASpeedLimit(t *testing.T) {
	payload := makePayload(6 << 20)
	src := httptest.NewServer(&rangedHandler{payload: payload})
	defer src.Close()

	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	m.streamPiece = 128 << 10
	m.SetSpeedLimit(1 << 20) // six seconds for the whole file
	s := &server{mgr: m, token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", s.guard(s.handleStream))
	api := httptest.NewServer(mux)
	defer api.Close()

	id, err := m.Add(jobRequest{URL: src.URL + "/film.mkv", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Pause(id)

	const from, to = 4 << 20, 4<<20 + 512<<10 - 1
	req, _ := http.NewRequest(http.MethodGet, api.URL+"/stream/"+id+"/film.mkv?token=tok", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sum(body) != sum(payload[from:to+1]) {
		t.Fatalf("streamed %d bytes that differ from the source", len(body))
	}
	// Half a second at the full limit, plus moving the connections there. With
	// only its share of the limit, the one connection at the player took two.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("half a megabyte at the player took %v under a 1 MiB/s limit", d)
	}
	if v, _ := findTask(m, id); v.State == StateDone {
		t.Fatal("the range only arrived once the whole file had: nothing was streamed")
	}
}
