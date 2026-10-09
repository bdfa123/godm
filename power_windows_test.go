//go:build windows

package main

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	procSleep              = kernel32.NewProc("Sleep")
)

func currentThreadID() uint32 {
	id, _, _ := procGetCurrentThreadId.Call()
	return uint32(id)
}

// Windows keeps the request on the thread that made it, so every call has to
// come from one thread however often the goroutine is parked and woken in
// between. The keeper blocks on a channel between calls, which is exactly when
// the runtime is free to resume it somewhere else if it is not pinned.
func TestKeepAwakeRequestsAllComeFromOneThread(t *testing.T) {
	// One keeper may happen to stay put by luck. Many of them, woken from
	// goroutines that are themselves being moved about by a busy scheduler,
	// will not unless they are pinned.
	const keepers = 24
	var mu sync.Mutex
	threads := make([]map[uint32]int, keepers)
	ks := make([]*awakeKeeper, keepers)
	for i := range ks {
		i := i
		threads[i] = map[uint32]int{}
		ks[i] = newAwakeKeeper(func(on bool) error {
			// A call that blocks in the system lets the runtime give the processor
			// to someone else, and the goroutine comes back on whichever thread
			// has one to spare. That is the move a pinned goroutine cannot make.
			procSleep.Call(2)
			mu.Lock()
			threads[i][currentThreadID()]++
			mu.Unlock()
			return nil
		})
	}

	stop := make(chan struct{})
	var busy, senders sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		busy.Add(1)
		go func() {
			defer busy.Done()
			for {
				select {
				case <-stop:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}
	for _, k := range ks {
		senders.Add(1)
		go func(k *awakeKeeper) {
			defer senders.Done()
			for n := 0; n < 100; n++ {
				k.Want(n%2 == 0)
				time.Sleep(5 * time.Millisecond) // longer than a call, so none are merged away
			}
		}(k)
	}
	senders.Wait()
	close(stop)
	busy.Wait()
	for _, k := range ks {
		k.Stop()
	}

	mu.Lock()
	defer mu.Unlock()
	moved := 0
	for i, seen := range threads {
		calls := 0
		for _, n := range seen {
			calls += n
		}
		if calls < 20 {
			t.Fatalf("keeper %d made only %d calls; the test did not exercise it", i, calls)
		}
		if len(seen) != 1 {
			moved++
		}
	}
	if moved > 0 {
		t.Errorf("%d of %d keepers made their requests from more than one OS thread", moved, keepers)
	}
}

// This makes a real keep-awake request, for as long as it takes to read it
// back, so it only runs when asked for. SetThreadExecutionState answers with
// the state it replaced, which is the one way to see what a thread holds.
func TestRealKeepAwakeRequestIsHeldAndWithdrawn(t *testing.T) {
	if os.Getenv("GODM_REAL_POWER") == "" {
		t.Skip("set GODM_REAL_POWER=1 to make a real, momentary keep-awake request")
	}
	const want = esContinuous | esSystemRequired
	result := make(chan [3]uintptr, 1)
	go func() {
		// The thread is never released, so it ends with this goroutine and takes
		// whatever it held with it.
		runtime.LockOSThread()
		defer procSetThreadExecutionState.Call(esContinuous)
		if err := setAwake(true); err != nil {
			t.Error(err)
		}
		held, _, _ := procSetThreadExecutionState.Call(want) // reads what is standing
		if err := setAwake(false); err != nil {
			t.Error(err)
		}
		after, _, _ := procSetThreadExecutionState.Call(esContinuous)
		result <- [3]uintptr{held, after}
	}()
	got := <-result
	if got[0] != want {
		t.Errorf("state after asking = %#x, want %#x", got[0], uintptr(want))
	}
	if got[1] != esContinuous {
		t.Errorf("state after withdrawing = %#x, want %#x", got[1], uintptr(esContinuous))
	}
}
