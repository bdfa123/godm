package main

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// defaultStallTimeout is how long a connection may deliver nothing before it
// is treated as dead. Long enough for a server catching its breath, short
// enough that a download does not sit at 97% for good.
const defaultStallTimeout = 30 * time.Second

func stallTimeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultStallTimeout
	}
	return d
}

// stallError ends a connection that stopped sending without closing. Nothing
// is wrong with what already arrived, and a new request usually gets going
// again, so it is always worth a retry.
type stallError struct{ after time.Duration }

func (e *stallError) Error() string {
	return fmt.Sprintf("no data from the server for %s", e.after)
}

// stallGuard watches a response body. Once headers are in, nothing else bounds
// how long a Read may block: a server can send part of a range and then go
// quiet with the socket still open, and the connection waits forever. The
// guard cancels the request instead, which makes the blocked Read return.
//
// Only time spent inside Read counts. A connection held back by a speed limit,
// or busy writing what it got, is not mistaken for a dead one.
type stallGuard struct {
	r     io.Reader
	after time.Duration
	timer *time.Timer
	fired atomic.Bool
}

// newStallGuard wraps r; cancel must end the request r belongs to. Bytes that
// come back with a stall are real, so whatever the caller does with them next,
// such as paying a speed limit, must not wait on the request's context: by
// then it has been cancelled, and the stall would come out as a cancellation.
func newStallGuard(r io.Reader, after time.Duration, cancel context.CancelFunc) *stallGuard {
	g := &stallGuard{r: r, after: stallTimeoutOr(after)}
	g.timer = time.AfterFunc(g.after, func() {
		g.fired.Store(true)
		cancel()
	})
	g.timer.Stop()
	return g
}

func (g *stallGuard) Read(p []byte) (int, error) {
	g.timer.Reset(g.after)
	n, err := g.r.Read(p)
	g.timer.Stop()
	if g.fired.Load() {
		// Whatever the cancelled read reports, the cause is the silence.
		return n, &stallError{after: g.after}
	}
	return n, err
}

// stop disarms the guard for good once the body is no longer being read.
func (g *stallGuard) stop() { g.timer.Stop() }
