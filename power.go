package main

import (
	"log"
	"runtime"
	"sync"
)

// powerOps is how the daemon reaches the computer's power state. The real
// calls come from osPower. NewManager starts with none, so a test can never
// keep a machine awake or put it to sleep; only RunDaemon installs them.
type powerOps struct {
	// keepAwake asks the system not to sleep while on is true and withdraws
	// the request when it is false. An awakeKeeper makes every call from the
	// same OS thread.
	keepAwake func(on bool) error
}

// awakeKeeper owns the request to keep the system awake. Windows ties that
// request to the thread that made it and drops it when the thread exits, so
// one goroutine pinned to its thread makes every call. Asking from whichever
// goroutine noticed a download starting would spread requests over threads the
// runtime reuses and retires as it likes, and a later "stop" could land on a
// thread that never asked.
type awakeKeeper struct {
	set  func(on bool) error
	poke chan struct{} // something changed; look at want
	done chan struct{} // closed when the goroutine has let go

	mu   sync.Mutex
	want bool
}

func newAwakeKeeper(set func(on bool) error) *awakeKeeper {
	k := &awakeKeeper{set: set, poke: make(chan struct{}, 1), done: make(chan struct{})}
	go k.run()
	return k
}

// Want records whether a request should be standing. It never blocks, and
// several changes made before the thread looks collapse into the last one.
func (k *awakeKeeper) Want(on bool) {
	k.mu.Lock()
	k.want = on
	k.mu.Unlock()
	select {
	case k.poke <- struct{}{}:
	default:
	}
}

func (k *awakeKeeper) run() {
	defer close(k.done)
	// Never unlocked. A goroutine that ends while pinned takes its thread with
	// it, which also ends the request if clearing it below somehow failed.
	runtime.LockOSThread()

	held := false
	for range k.poke {
		k.mu.Lock()
		on := k.want
		k.mu.Unlock()
		if on == held {
			continue
		}
		if err := k.set(on); err != nil {
			// Leave held as it was so the next change tries again.
			log.Printf("keep awake: %v", err)
			continue
		}
		held = on
	}
	if held {
		k.set(false)
	}
}

// Stop withdraws any request and waits until the thread has done so.
func (k *awakeKeeper) Stop() {
	close(k.poke)
	<-k.done
}

// SetPower connects the manager to the computer's power state, replacing
// whatever was connected before.
func (m *Manager) SetPower(p powerOps) {
	m.mu.Lock()
	old := m.awake
	m.power, m.awake = p, nil
	if p.keepAwake != nil {
		m.awake = newAwakeKeeper(p.keepAwake)
	}
	m.syncAwakeLocked()
	m.mu.Unlock()
	if old != nil {
		old.Stop()
	}
}

// syncAwakeLocked makes the standing request match the facts: a download holds
// a slot, and the user has not switched the feature off. Callers hold m.mu.
func (m *Manager) syncAwakeLocked() {
	if m.awake != nil {
		m.awake.Want(m.settings.KeepAwake && m.running > 0)
	}
}
