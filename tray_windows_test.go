//go:build windows

package main

import (
	"reflect"
	"syscall"
	"testing"
	"unsafe"
)

var (
	procGetMenuItemCount = user32.NewProc("GetMenuItemCount")
	procGetMenuItemID    = user32.NewProc("GetMenuItemID")
	procGetMenuState     = user32.NewProc("GetMenuState")
	procGetMenuString    = user32.NewProc("GetMenuStringW")
)

const mfByPosition = 0x0400

type menuEntry struct {
	cmd     uintptr
	label   string
	checked bool
}

// readMenu reads a menu back through the system, without showing it.
func readMenu(menu uintptr) []menuEntry {
	n, _, _ := procGetMenuItemCount.Call(menu)
	var out []menuEntry
	for i := 0; i < int(int32(n)); i++ {
		id, _, _ := procGetMenuItemID.Call(menu, uintptr(i))
		state, _, _ := procGetMenuState.Call(menu, uintptr(i), mfByPosition)
		var buf [64]uint16
		procGetMenuString.Call(menu, uintptr(i), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), mfByPosition)
		out = append(out, menuEntry{id, syscall.UTF16ToString(buf[:]), state&mfChecked != 0})
	}
	return out
}

func TestTrayMenuOffersWhatThisComputerCanDoAndTicksTheArmedChoice(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	arm(t, m, "sleep")
	menu := afterMenu(m)
	if menu == 0 {
		t.Fatal("no menu was made")
	}
	defer procDestroyMenu.Call(menu)

	want := []menuEntry{
		{menuAfterNothing, "Do nothing", false},
		{menuAfterSleep, "Sleep", true},
		{menuAfterShutdown, "Shut down", false},
	}
	if got := readMenu(menu); !reflect.DeepEqual(got, want) {
		t.Errorf("menu = %+v\nwant   %+v", got, want)
	}
}

func TestTrayMenuLeavesOutWhatCannotBeDone(t *testing.T) {
	m := NewManager(t.TempDir(), 2) // nothing connected to the power state
	menu := afterMenu(m)
	defer procDestroyMenu.Call(menu)
	want := []menuEntry{{menuAfterNothing, "Do nothing", true}}
	if got := readMenu(menu); !reflect.DeepEqual(got, want) {
		t.Errorf("menu = %+v\nwant   %+v", got, want)
	}
}

// The menu is built each time it is opened, so a change of language shows the
// next time, with no restart.
func TestTrayMenuFollowsTheLanguage(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	arm(t, m, "shutdown")
	setLanguage(t, m, langZH)

	menu := afterMenu(m)
	defer procDestroyMenu.Call(menu)
	want := []menuEntry{
		{menuAfterNothing, msg(langZH, "after.nothing"), false},
		{menuAfterSleep, msg(langZH, "after.sleep"), false},
		{menuAfterShutdown, msg(langZH, "after.shutdown"), true},
	}
	got := readMenu(menu)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("menu = %+v\nwant   %+v", got, want)
	}
	for _, e := range got {
		if !hasHan(e.label) {
			t.Errorf("%q is not in Chinese", e.label)
		}
	}

	setLanguage(t, m, langEN)
	if got := readMenu(afterMenu(m)); got[0].label != "Do nothing" {
		t.Errorf("after switching back the first entry is %q", got[0].label)
	}
}

func TestTrayTooltipIsInTheChosenLanguage(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	if got := trayTip(m); got != "godm — idle" {
		t.Errorf("tooltip = %q", got)
	}
	setLanguage(t, m, langZH)
	idle := trayTip(m)
	if idle != msg(langZH, "tray.tip.idle") || !hasHan(idle) {
		t.Errorf("tooltip = %q, want the Chinese idle text", idle)
	}
	busy := taskIn(m, "a", StateRunning)
	busy.mu.Lock()
	busy.view.Speed = 2 << 20
	busy.mu.Unlock()
	taskIn(m, "b", StateQueued)
	if got, want := trayTip(m), msg(langZH, "tray.tip.running", "n", 1, "speed", "2.0 MiB")+msg(langZH, "tray.tip.more", "n", 1); got != want {
		t.Errorf("tooltip = %q, want %q", got, want)
	}
}

func TestPickingFromTheTrayMenuArmsTheAction(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	tr := &tray{mgr: m}
	tr.chooseAfter(menuAfterShutdown)
	if got := m.AfterAll(); got != "shutdown" {
		t.Fatalf("after picking Shut down the choice is %q", got)
	}
	tr.chooseAfter(menuAfterNothing)
	if got := m.AfterAll(); got != "nothing" {
		t.Fatalf("after picking Do nothing the choice is %q", got)
	}
}

// The command line is the one the feature promises: Windows counts down for a
// minute with a reason on screen, and "shutdown /a" cancels it.
func TestShutdownCommandLine(t *testing.T) {
	want := []string{"/s", "/t", "60", "/c", "godm: all downloads finished"}
	if got := shutdownArgs(shutdownDelay, "godm: all downloads finished"); !reflect.DeepEqual(got, want) {
		t.Errorf("shutdown.exe %q, want %q", got, want)
	}
}
