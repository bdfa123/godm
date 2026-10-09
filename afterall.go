package main

import (
	"fmt"
	"log"
	"time"
)

// afterAction is what the computer does once the downloads have all finished.
// It is a one-shot choice: once armed it runs a single time and goes back to
// nothing, and it is not kept in tasks.json, so a restart can never lead to a
// shutdown nobody asked for today.
//
// It runs when no download is running or waiting its turn and at least one has
// finished since it was armed. Which states count:
//
//   - Done is what finishing means. Nothing else is, so a lone failure never
//     turns the machine off or puts it to sleep.
//   - Running and queued downloads hold the action back.
//   - A download waiting for a fresh link holds it back too, because the user
//     is in the middle of fixing it, but only until its wait runs out
//     (refreshWindow at most), so it cannot hold the action back for ever.
//   - Paused, failed and expired-link downloads do not hold it back. They will
//     not move without the user, and waiting for them would mean the action
//     never comes.
type afterAction string

const (
	afterNothing  afterAction = "nothing"
	afterSleep    afterAction = "sleep"
	afterShutdown afterAction = "shutdown"
)

// shutdownDelay is the time between the notice and the shutdown: enough to read
// the notice and run "shutdown /a".
const shutdownDelay = 60 * time.Second

// checkAfterAll runs the armed action if nothing holds it back. It is called
// when a run ends by itself, as done, failed or expired (finished says whether
// it was done), and again when a wait for a new link runs out. Pausing or
// removing a download never sets the action off: whoever does that is at the
// machine, and finishing is not what they did.
func (m *Manager) checkAfterAll(finished bool) {
	m.mu.Lock()
	if finished {
		m.afterSeen = true
	}
	act := m.after
	if act == afterNothing || !m.afterSeen {
		m.mu.Unlock()
		return
	}
	blocked, retry := m.afterBlockedLocked()
	if blocked {
		m.mu.Unlock()
		if !retry.IsZero() {
			time.AfterFunc(time.Until(retry)+50*time.Millisecond, func() { m.checkAfterAll(false) })
		}
		return
	}
	m.after, m.afterSeen = afterNothing, false
	if m.checkAfterChoiceLocked(act) != nil {
		m.mu.Unlock() // the connection to the power state was replaced since it was armed
		return
	}
	ops := m.power
	m.mu.Unlock()
	go m.runAfter(act, ops)
}

// afterBlockedLocked says whether anything still holds the action back. When
// the only thing doing so is a wait for a new link, retry is when that wait
// ends. Callers hold m.mu.
func (m *Manager) afterBlockedLocked() (blocked bool, retry time.Time) {
	now := time.Now()
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		st, until := t.view.State, t.view.RefreshUntil
		t.mu.Unlock()
		switch st {
		case StateRunning, StateQueued:
			// Its own end will look again.
			return true, time.Time{}
		case StateAwaitingRefresh:
			if now.Before(until) {
				blocked = true
				if retry.IsZero() || until.Before(retry) {
					retry = until
				}
			}
		}
	}
	return blocked, retry
}

func (m *Manager) runAfter(act afterAction, ops powerOps) {
	// The list is normally written every couple of seconds; do it now so that
	// what finished a moment ago is not lost to a shutdown.
	if err := m.save(); err != nil {
		log.Printf("saving task list: %v", err)
	}
	switch act {
	case afterSleep:
		log.Printf("all downloads finished: putting the computer to sleep")
		if err := ops.sleep(); err != nil {
			log.Printf("sleep: %v", err)
			m.warn("Could not put the computer to sleep", err.Error())
		}
	case afterShutdown:
		log.Printf("all downloads finished: shutting down in %s", shutdownDelay)
		if err := ops.shutdown(shutdownDelay); err != nil {
			log.Printf("shutdown: %v", err)
			m.warn("Could not shut the computer down", err.Error())
			return
		}
		m.warn(fmt.Sprintf("Shutting down in %d seconds", int(shutdownDelay/time.Second)),
			"All downloads finished. To cancel, press Win+R, type shutdown /a and press Enter.")
	}
}

// warn tells the user something about the machine's power. Unlike the notice
// that a download finished it ignores the notifications switch: a warning that
// the computer is about to turn off is not one to leave out.
func (m *Manager) warn(title, text string) {
	if m.announce != nil {
		m.announce(title, text, "app")
	}
}

// AfterAll is the action that will run when the downloads finish.
func (m *Manager) AfterAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.after)
}

// AfterOptions lists what this computer can be asked to do once the downloads
// finish, in the order the dialog and the tray menu show them.
func (m *Manager) AfterOptions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.afterOptionsLocked()
}

// afterOptionsLocked is AfterOptions for callers that hold m.mu.
func (m *Manager) afterOptionsLocked() []string {
	opts := []string{string(afterNothing)}
	if m.power.sleep != nil {
		opts = append(opts, string(afterSleep))
	}
	if m.power.shutdown != nil {
		opts = append(opts, string(afterShutdown))
	}
	return opts
}

// checkAfterChoiceLocked refuses an action that is unknown or that this
// computer cannot carry out, so the user hears about it now rather than
// finding out when the downloads are done. Callers hold m.mu.
func (m *Manager) checkAfterChoiceLocked(act afterAction) error {
	switch act {
	case afterNothing:
		return nil
	case afterSleep:
		if m.power.sleep == nil {
			return fmt.Errorf("godm cannot put this computer to sleep")
		}
		return nil
	case afterShutdown:
		if m.power.shutdown == nil {
			return fmt.Errorf("godm cannot shut this computer down")
		}
		return nil
	}
	return fmt.Errorf("unknown action %q: use nothing, sleep or shutdown", string(act))
}
