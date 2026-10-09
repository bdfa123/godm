package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimiter holds bytes to a rate shared by everyone who waits on it, so one
// limit can cover every download at once, or every connection of one.
//
// It is a token bucket that may run into debt. A connection pays for what it
// has just read and then waits for its turn: each payment takes a place in
// line, and the line moves at the rate. The average holds however the reads
// happen to be sized, and connections sharing a limit take turns instead of
// all waking at once. Changing the rate moves the line at the new speed from
// that moment, waiters included.
//
// There are two lines. Bytes a player is waiting on go in the first, which is
// served before the other: under a limit the connection fetching what is
// needed right now would otherwise get only its share, and a seek would take
// as many times longer as there are connections.
//
// The zero value has no limit and is ready to use.
type RateLimiter struct {
	rate atomic.Int64 // bytes per second; 0 means no limit

	mu      sync.Mutex
	allowed float64   // bytes the rate has let through, up to last
	last    time.Time // when allowed was brought up to date
	// What has been paid into each line, and how much of that has gone.
	firstPaid, restPaid float64
	firstOut, restOut   float64
	changed             chan struct{} // closed when the rate changes, so waiters re-plan
}

func NewRateLimiter(bytesPerSec int64) *RateLimiter {
	l := &RateLimiter{}
	l.SetRate(bytesPerSec)
	return l
}

// Rate is the limit in bytes per second, 0 when there is none.
func (l *RateLimiter) Rate() int64 {
	if l == nil {
		return 0
	}
	return l.rate.Load()
}

// SetRate changes the limit; 0 or less removes it. It takes effect at once,
// for connections already waiting too.
func (l *RateLimiter) SetRate(bytesPerSec int64) {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.rate.Load()
	if old == bytesPerSec {
		return
	}
	now := time.Now()
	if old > 0 {
		l.catchUpLocked(now, old)
	} else {
		// Nothing is owed, and nothing saved up, from a time without a limit.
		l.allowed = l.firstPaid + l.restPaid
		l.firstOut, l.restOut = l.firstPaid, l.restPaid
	}
	l.last = now
	l.rate.Store(bytesPerSec)
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

func (l *RateLimiter) catchUpLocked(now time.Time, rate int64) {
	if dt := now.Sub(l.last).Seconds(); dt > 0 {
		l.allowed += dt * float64(rate)
	}
	l.last = now
	// Credit saved up while nobody read is kept to a tenth of a second, so a
	// connection back from idle does not race ahead of the limit.
	if most := l.firstPaid + l.restPaid + float64(rate)/10; l.allowed > most {
		l.allowed = most
	}
}

// WaitN pays for n bytes and returns once the rate allows them, or when ctx
// ends. Without a limit it returns at once and costs one atomic load.
func (l *RateLimiter) WaitN(ctx context.Context, n int) error { return l.wait(ctx, n, false) }

// WaitFirst is WaitN for bytes someone is waiting on right now: they go ahead
// of everything paid with WaitN.
func (l *RateLimiter) WaitFirst(ctx context.Context, n int) error { return l.wait(ctx, n, true) }

func (l *RateLimiter) wait(ctx context.Context, n int, first bool) error {
	if l == nil || n <= 0 || l.rate.Load() == 0 {
		return nil
	}
	l.mu.Lock()
	rate := l.rate.Load()
	if rate == 0 {
		l.mu.Unlock()
		return nil
	}
	l.catchUpLocked(time.Now(), rate)
	var turn float64
	if first {
		l.firstPaid += float64(n)
		turn = l.firstPaid
	} else {
		l.restPaid += float64(n)
		turn = l.restPaid
	}
	for {
		// The first line may use whatever the rate allowed that the other has
		// not already used. The other waits for everything in the first line,
		// let through or not.
		var short float64
		if first {
			short = turn - (l.allowed - l.restOut)
		} else {
			short = turn - (l.allowed - l.firstPaid)
		}
		if short <= 0 {
			l.letThroughLocked(n, first)
			l.mu.Unlock()
			return nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed := l.changed
		l.mu.Unlock()

		timer := time.NewTimer(time.Duration(short / float64(rate) * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			// The bytes were read all the same, so they count as gone.
			l.mu.Lock()
			l.letThroughLocked(n, first)
			l.mu.Unlock()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}

		l.mu.Lock()
		if rate = l.rate.Load(); rate == 0 {
			l.mu.Unlock()
			return nil
		}
		l.catchUpLocked(time.Now(), rate)
	}
}

func (l *RateLimiter) letThroughLocked(n int, first bool) {
	if first {
		l.firstOut += float64(n)
	} else {
		l.restOut += float64(n)
	}
}

// chunk is how much a connection should read before paying. A limited one
// reads in small pieces, a thirty-second of a second's worth, so what it pays
// for at once is never big enough to make it stutter.
func (l *RateLimiter) chunk(most int) int {
	rate := l.Rate()
	if rate == 0 {
		return most
	}
	c := rate / 32
	if c < 4<<10 {
		c = 4 << 10
	}
	if c < int64(most) {
		return int(c)
	}
	return most
}

// rateLimits is every limit one transfer keeps to, such as the one shared by
// all downloads and its own. The strictest sets the pace. Nil entries mean no
// limit.
type rateLimits []*RateLimiter

func (ls rateLimits) limited() bool {
	for _, l := range ls {
		if l.Rate() > 0 {
			return true
		}
	}
	return false
}

func (ls rateLimits) chunk(most int) int {
	for _, l := range ls {
		most = l.chunk(most)
	}
	return most
}

// wait pays n bytes to every limit; first puts them ahead in each line.
func (ls rateLimits) wait(ctx context.Context, n int, first bool) error {
	for _, l := range ls {
		if err := l.wait(ctx, n, first); err != nil {
			return err
		}
	}
	return nil
}

// readPaced is io.ReadAll keeping to the limits, for a response that has to
// be held whole, as a stream segment is.
func readPaced(ctx context.Context, r io.Reader, ls rateLimits) ([]byte, error) {
	b := make([]byte, 0, 512)
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)] // let append choose how much to grow
		}
		room := b[len(b):cap(b)]
		n, err := r.Read(room[:ls.chunk(len(room))])
		b = b[:len(b)+n]
		if n > 0 {
			if werr := ls.wait(ctx, n, false); werr != nil {
				return nil, werr
			}
		}
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
