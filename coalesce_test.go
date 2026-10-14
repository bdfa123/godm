package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// feed answers the engine's requests itself, the way stall_test's transports
// do: it serves payload by range, chunk bytes a read, as TLS hands them over.
// Before each read, step is told how many bytes of this response have gone;
// an error from it ends the body instead.
type feed struct {
	payload []byte
	chunk   int
	step    func(r *http.Request, sent int) error

	sent atomic.Int64 // over every response
}

func (fd *feed) RoundTrip(r *http.Request) (*http.Response, error) {
	start, end := rangeOf(r, len(fd.payload))
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(fd.payload)))
	return &http.Response{
		StatusCode: http.StatusPartialContent, Status: "206 Partial Content", Header: h,
		Body: &feedBody{fd: fd, r: r, rest: fd.payload[start : end+1]}, Request: r,
	}, nil
}

type feedBody struct {
	fd   *feed
	r    *http.Request
	rest []byte
	sent int
}

func (b *feedBody) Read(p []byte) (int, error) {
	if b.fd.step != nil {
		if err := b.fd.step(b.r, b.sent); err != nil {
			return 0, err
		}
	}
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), b.fd.chunk)], b.rest)
	b.rest = b.rest[n:]
	b.sent += n
	b.fd.sent.Add(int64(n))
	return n, nil
}

func (b *feedBody) Close() error { return nil }

// fileWrite is one write into a download's file.
type fileWrite struct{ off, n int64 }

// watchWrites records every write into a download's file until the test
// ends.
func watchWrites(t *testing.T) func() []fileWrite {
	t.Helper()
	var mu sync.Mutex
	var got []fileWrite
	old := writeFileAt
	writeFileAt = func(f *os.File, b []byte, off int64) (int, error) {
		mu.Lock()
		got = append(got, fileWrite{off, int64(len(b))})
		mu.Unlock()
		return old(f, b, off)
	}
	t.Cleanup(func() { writeFileAt = old })
	return func() []fileWrite {
		mu.Lock()
		defer mu.Unlock()
		return append([]fileWrite(nil), got...)
	}
}

// holdFor sets how long bytes may wait to be written, for one test.
func holdFor(t *testing.T, d time.Duration) {
	old := coalesceFor
	coalesceFor = d
	t.Cleanup(func() { coalesceFor = old })
}

// rangeTask is a download of fd's payload with the given ranges, enough of
// one to run a connection on with trySegment, writing into a file of its own.
func rangeTask(t *testing.T, fd *feed, segs ...*Segment) *task {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "f.bin"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(int64(len(fd.payload))); err != nil {
		t.Fatal(err)
	}
	return &task{
		opts: Options{
			Connections: 2, MinSplit: 64 << 10, StreamPiece: 256 << 10,
		},
		probe:  &ProbeResult{FinalURL: "http://files.example/f.bin", Size: int64(len(fd.payload)), Resumable: true},
		file:   f,
		client: &http.Client{Transport: fd},
		segs:   segs,
	}
}

// fileHolds fails unless the task's file holds payload from lo to hi and
// nothing but zeros from hi to the end, which nobody else wrote.
func fileHolds(t *testing.T, tk *task, payload []byte, lo, hi int64) {
	t.Helper()
	got, err := os.ReadFile(tk.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[lo:hi], payload[lo:hi]) {
		t.Errorf("the file differs from the source between %d and %d", lo, hi)
	}
	if i := bytes.IndexFunc(got[hi:], func(r rune) bool { return r != 0 }); i >= 0 {
		t.Errorf("byte %d, past the end of the range at %d, was written", hi+int64(i), hi)
	}
}

// A connection reading a range sixteen kilobytes at a time writes it a
// megabyte at a time, and only the last write, at the end of the range, is
// short.
func TestAConnectionWritesAMegabyteAtATime(t *testing.T) {
	holdFor(t, time.Minute) // no write here is down to time
	writes := watchWrites(t)
	payload := makePayload(3<<20 + 512<<10 + 123)
	fd := &feed{payload: payload, chunk: 16 << 10}
	seg := newSegment(0, int64(len(payload))-1)
	tk := rangeTask(t, fd, seg)
	if err := tk.trySegment(context.Background(), seg); err != nil {
		t.Fatal(err)
	}
	got := writes()
	var next int64
	short := 0
	for i, w := range got {
		if w.off != next {
			t.Fatalf("write %d is at %d, want %d", i, w.off, next)
		}
		if last := i == len(got)-1; !last && w.n != coalesceSize {
			short++
		}
		next += w.n
	}
	if short > 0 {
		t.Errorf("%d of the %d writes before the last were not %d bytes; the first was %d", short, len(got)-1, coalesceSize, got[0].n)
	}
	if want := (len(payload) + coalesceSize - 1) / coalesceSize; len(got) != want {
		t.Errorf("%d writes, want %d", len(got), want)
	}
	if next != int64(len(payload)) || seg.done.Load() != next || seg.held.Load() != 0 || tk.received.Load() != next {
		t.Errorf("wrote %d, done %d, held %d, received %d, want all %d", next, seg.done.Load(), seg.held.Load(), tk.received.Load(), len(payload))
	}
	fileHolds(t, tk, payload, 0, int64(len(payload)))
}

// Bytes from a slow connection do not wait for a megabyte to fill: the
// progress, the speed and the sidecar would sit still meanwhile.
func TestHeldBytesAreWrittenOnceTheyHaveWaitedLongEnough(t *testing.T) {
	holdFor(t, 100*time.Millisecond)
	writes := watchWrites(t)
	payload := makePayload(1 << 20)
	seg := newSegment(0, int64(len(payload))-1)
	var midway atomic.Int64 // what was on disk once half the range had come
	fd := &feed{payload: payload, chunk: 16 << 10, step: func(_ *http.Request, sent int) error {
		time.Sleep(20 * time.Millisecond)
		if sent == len(payload)/2 {
			midway.Store(seg.done.Load())
		}
		return nil
	}}
	tk := rangeTask(t, fd, seg)
	if err := tk.trySegment(context.Background(), seg); err != nil {
		t.Fatal(err)
	}
	if midway.Load() == 0 {
		t.Errorf("nothing was written while the first half of the range took %v to come", 32*20*time.Millisecond)
	}
	if n := len(writes()); n < 3 {
		t.Errorf("%d writes for a range that took over a second, want one about every tenth of a second", n)
	}
	fileHolds(t, tk, payload, 0, int64(len(payload)))
}

// While a player waits on what a connection reads, every read is written at
// once, starting with what the connection held when the player began to wait.
func TestAPlayerWaitingOnARangeHasEachReadAtOnce(t *testing.T) {
	holdFor(t, time.Minute)
	payload := makePayload(1 << 20)
	seg := newSegment(0, int64(len(payload))-1)
	var pb Playback
	var late []string
	fd := &feed{payload: payload, chunk: 16 << 10, step: func(_ *http.Request, sent int) error {
		const wants = 256 << 10
		switch {
		case sent == wants:
			// The player seeks to a byte this connection already holds.
			pb.NewReader().Want(100 << 10)
		case sent > wants:
			if done := seg.done.Load(); done != int64(sent) {
				late = append(late, fmt.Sprintf("%d of %d", done, sent))
			}
		}
		return nil
	}}
	tk := rangeTask(t, fd, seg)
	tk.opts.Playback = &pb
	if err := tk.trySegment(context.Background(), seg); err != nil {
		t.Fatal(err)
	}
	if len(late) > 0 {
		t.Errorf("with a player waiting, the bytes on disk lagged what had come (%d times; first %s)", len(late), late[0])
	}
	fileHolds(t, tk, payload, 0, int64(len(payload)))
}

// Bytes a connection has read are good however it comes to stop: they are
// written before it lets go of the range, and the next connection carries on
// after them instead of fetching them again.
func TestBytesAConnectionHoldsAreKeptHoweverItStops(t *testing.T) {
	const stopAt = 300 << 10
	cases := []struct {
		name string
		want error
		// setup readies the task and returns what the feed does before each
		// read once stopAt bytes have come.
		setup func(tk *task, cancel context.CancelFunc) func(r *http.Request) error
	}{
		{"the connection breaks", io.ErrUnexpectedEOF, func(*task, context.CancelFunc) func(*http.Request) error {
			return func(*http.Request) error { return io.ErrUnexpectedEOF }
		}},
		{"the download is stopped", context.Canceled, func(_ *task, cancel context.CancelFunc) func(*http.Request) error {
			return func(*http.Request) error { cancel(); return context.Canceled }
		}},
		{"the server goes quiet", &stallError{}, func(tk *task, _ context.CancelFunc) func(*http.Request) error {
			tk.opts.StallTimeout = 100 * time.Millisecond
			return func(r *http.Request) error { <-r.Context().Done(); return r.Context().Err() }
		}},
		{"the connection limit is lowered", errYield, func(tk *task, _ context.CancelFunc) func(*http.Request) error {
			var limit atomic.Int32
			limit.Store(2)
			tk.workers.Store(2)
			tk.opts.ConnLimit = func() int { return int(limit.Load()) }
			return func(*http.Request) error { limit.Store(1); return nil }
		}},
		{"a player wants it elsewhere", errMove, func(tk *task, _ context.CancelFunc) func(*http.Request) error {
			// The player reads in the other range, so nothing this connection
			// holds is what it waits for.
			var pb Playback
			tk.opts.Playback = &pb
			rd := pb.NewReader()
			return func(*http.Request) error {
				rd.Want(3 << 20)
				time.Sleep(20 * time.Millisecond) // until the connection next looks
				return nil
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			holdFor(t, time.Minute)
			payload := makePayload(4 << 20)
			seg := newSegment(0, 2<<20-1)
			other := newSegment(2<<20, 4<<20-1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fd := &feed{payload: payload, chunk: 16 << 10}
			tk := rangeTask(t, fd, seg, other)
			seg.busy = true
			then := c.setup(tk, cancel)
			fd.step = func(r *http.Request, sent int) error {
				if sent >= stopAt {
					return then(r)
				}
				return nil
			}
			err := tk.trySegment(ctx, seg)
			if se, ok := c.want.(*stallError); ok {
				if !errors.As(err, &se) {
					t.Fatalf("the connection ended with %v, want a stall", err)
				}
			} else if !errors.Is(err, c.want) {
				t.Fatalf("the connection ended with %v, want %v", err, c.want)
			}
			read := fd.sent.Load()
			if read < stopAt {
				t.Fatalf("only %d bytes came before the connection stopped", read)
			}
			if done, held := seg.done.Load(), seg.held.Load(); done != read || held != 0 {
				t.Errorf("%d bytes were read, and %d kept with %d still held", read, done, held)
			}
			fileHolds(t, tk, payload, 0, read)
		})
	}
}

// A split while a connection holds bytes it has not written yet leaves those
// bytes to it, and the range it is left with still ends where the split says:
// what the server sends past the new end is dropped, as an overshoot is.
func TestASplitWhileBytesAreHeldLeavesThemAndCutsTheRest(t *testing.T) {
	holdFor(t, time.Minute)
	writes := watchWrites(t)
	payload := makePayload(4 << 20)
	seg := newSegment(0, int64(len(payload))-1)
	const minPiece = 64 << 10
	var (
		half     *Segment
		cutInto  *Segment
		heldThen int64
		end      int64
	)
	fd := &feed{payload: payload, chunk: 16 << 10, step: func(_ *http.Request, sent int) error {
		switch sent {
		case 192 << 10:
			// What claim does for an idle connection.
			heldThen = seg.held.Load()
			half = seg.splitInHalf(minPiece)
			// And what a player could ask for: the bytes held here.
			cutInto = seg.splitAt(150<<10, 1, 1)
		case 1<<20 + 48<<10:
			// A cut that the next read goes past.
			ns := seg.splitAt(seg.readTo()+70000, 1, 1)
			if ns == nil {
				t.Error("a split after the bytes held was refused")
				return nil
			}
			end = ns.Start
		}
		return nil
	}}
	tk := rangeTask(t, fd, seg)
	if err := tk.trySegment(context.Background(), seg); err != nil {
		t.Fatal(err)
	}
	if heldThen != 192<<10 {
		t.Fatalf("the connection held %d bytes at the split, want the %d that had come", heldThen, 192<<10)
	}
	if half == nil || half.Start < 192<<10 {
		t.Fatalf("the split took the range from %v on, want it past the bytes held", half)
	}
	if cutInto != nil {
		t.Errorf("a split took the bytes from %d on, which the connection held", cutInto.Start)
	}
	if end == 0 {
		t.Fatal("the second split never happened")
	}
	if done := seg.done.Load(); done != end || seg.End() != end-1 {
		t.Errorf("the range ends at %d with %d bytes done, want %d", seg.End(), done, end)
	}
	for _, w := range writes() {
		if w.off+w.n > end {
			t.Errorf("a write from %d to %d went past the end of the range at %d", w.off, w.off+w.n, end)
		}
	}
	fileHolds(t, tk, payload, 0, end)
}
