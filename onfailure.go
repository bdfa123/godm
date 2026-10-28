package main

import (
	"fmt"
	"time"
)

// failPolicy is what a download that fails does next: a run that ends by
// itself, failed or with its link expired. A pause is not a failure. The choice
// is kept in the settings and applies at all times, and it also decides when
// the action for when the downloads finish (afterall.go) goes ahead:
//
//   - failGiveUp leaves the download failed. An action armed counts it as
//     over, as it does a finished one.
//   - failRetry tries it again after a while, up to a number of times, and
//     then gives up as failGiveUp does. A try that gets further than any run
//     before it starts the count again, so a long download that loses its
//     connection now and then through the night is not given up on.
//   - failForever goes on trying it, and an action armed waits for it.
//
// An expired link is tried again too. One that has really expired fails at the
// first request, which costs next to nothing, and some servers answer 403 or
// 404 for a while and then serve the file again.
type failPolicy string

const (
	failGiveUp  failPolicy = "give_up"
	failRetry   failPolicy = "retry"
	failForever failPolicy = "forever"
)

const (
	// By default a failure is tried again three times, five minutes apart.
	defaultRetryEvery = 5 // minutes
	defaultRetryTimes = 3
	maxRetryEvery     = 24 * 60
	maxRetryTimes     = 100
)

// retryChoice is the part of the settings a failing run reads. It is kept
// apart from them, behind an atomic pointer, so that finishRun can read it
// under the task's own lock and decide in the same breath as it marks the
// download failed: an after-all check in between would otherwise see a
// failure with nothing planned for it.
type retryChoice struct {
	policy failPolicy
	every  time.Duration
	times  int
}

func (s Settings) retryChoice() *retryChoice {
	return &retryChoice{
		policy: failPolicy(s.OnFailure),
		every:  time.Duration(s.RetryEvery) * time.Minute,
		times:  s.RetryTimes,
	}
}

// settleRetry fills in what a list saved before these settings existed, or
// edited by hand, leaves out or gets wrong.
func (s *Settings) settleRetry() {
	if checkFailPolicy(s.OnFailure) != nil {
		s.OnFailure = string(failRetry)
	}
	if checkRetryEvery(s.RetryEvery) != nil {
		s.RetryEvery = defaultRetryEvery
	}
	if checkRetryTimes(s.RetryTimes) != nil {
		s.RetryTimes = defaultRetryTimes
	}
}

func checkFailPolicy(p string) error {
	switch failPolicy(p) {
	case failGiveUp, failRetry, failForever:
		return nil
	}
	return fmt.Errorf("unknown choice %q for a failed download: use %s, %s or %s", p, failGiveUp, failRetry, failForever)
}

func checkRetryEvery(minutes int) error {
	if minutes < 1 || minutes > maxRetryEvery {
		return fmt.Errorf("the time between tries must be 1 to %d minutes, not %d", maxRetryEvery, minutes)
	}
	return nil
}

func checkRetryTimes(n int) error {
	if n < 1 || n > maxRetryTimes {
		return fmt.Errorf("the number of tries must be 1 to %d, not %d", maxRetryTimes, n)
	}
	return nil
}

// isFailure says whether a state is one that a try again starts from.
func isFailure(st TaskState) bool { return st == StateError || st == StateNeedsRefresh }

// retryPlan is what became of a run that failed.
type retryPlan struct {
	at    time.Time     // when it is tried again, zero when it is not
	every time.Duration // the time until then
	try   int           // which try again that will be, from 1
	of    int           // out of how many, 0 when there is no end to them
	// gaveUp says it is not tried again, and is over as far as the action
	// for when the downloads finish goes; tries is how many tries again in a
	// row came to nothing before that.
	gaveUp bool
	tries  int
	// auto says the run that failed was itself a try again.
	auto bool
}

// planRetryLocked decides, as run gen ends failed, whether and when the
// download is tried again. Callers hold mt.mu.
func (m *Manager) planRetryLocked(mt *managedTask, gen int, now time.Time) retryPlan {
	c := m.failing.Load()
	p := retryPlan{auto: mt.autoGen == gen}
	if mt.view.Received > mt.furthest {
		mt.furthest, mt.view.Retries = mt.view.Received, 0
	}
	if c.policy == failGiveUp || (c.policy == failRetry && mt.view.Retries >= c.times) || mt.wrongFile == gen {
		mt.view.RetryAt = time.Time{}
		p.gaveUp, p.tries = true, mt.view.Retries
		return p
	}
	mt.view.RetryAt = now.Add(c.every)
	p.at, p.every, p.try = mt.view.RetryAt, c.every, mt.view.Retries+1
	if c.policy == failRetry {
		p.of = c.times
	}
	return p
}

// replanRetriesLocked applies a new choice to the downloads failed now. Under
// failGiveUp none is tried again. Turning tries again on, from failGiveUp,
// has every failed download tried after the wait; any other change moves only
// those that were to be tried anyway, so one given up on, or stopped by the
// user, stays as it is. Those moved count afresh. It says whether any that
// were to be tried again no longer are. Callers hold m.mu.
func (m *Manager) replanRetriesLocked(was failPolicy, now time.Time) (dropped bool) {
	c := m.failing.Load()
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		planned := !t.view.RetryAt.IsZero()
		switch {
		case !isFailure(t.view.State):
		case c.policy == failGiveUp:
			dropped = dropped || planned
			t.view.RetryAt = time.Time{}
		case planned || was == failGiveUp:
			t.view.RetryAt, t.view.Retries = now.Add(c.every), 0
		}
		t.mu.Unlock()
	}
	return dropped
}

// stopRetriesLocked stops every try again planned, for Pause all: nothing is
// to start by itself after that. Callers hold m.mu.
func (m *Manager) stopRetriesLocked() {
	for _, id := range m.order {
		if t := m.tasks[id]; t != nil {
			t.mu.Lock()
			t.view.RetryAt = time.Time{}
			t.mu.Unlock()
		}
	}
}

// retryTick tries again every failed download whose time has come. It runs on
// the list's clock.
func (m *Manager) retryTick(now time.Time) {
	type due struct {
		id string
		at time.Time
	}
	var dues []due
	m.mu.Lock()
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		if at := t.view.RetryAt; !at.IsZero() && !now.Before(at) && isFailure(t.view.State) {
			dues = append(dues, due{id, at})
		}
		t.mu.Unlock()
	}
	m.mu.Unlock()
	for _, d := range dues {
		m.rerun(d.id, false, d.at)
	}
}

// RetryChoice is what a failed download does next, and the minutes between
// tries and how many there are before giving up.
func (m *Manager) RetryChoice() (policy string, every, times int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.OnFailure, m.settings.RetryEvery, m.settings.RetryTimes
}
