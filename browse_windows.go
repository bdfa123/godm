//go:build windows

package main

import (
	"errors"
	"syscall"
)

const canBrowseFolders = true

var (
	procAllowSetForeground = user32.NewProc("AllowSetForegroundWindow")
	procBringWindowToTop   = user32.NewProc("BringWindowToTop")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
)

const (
	hwndTop   = 0
	swpNoSize = 0x0001
	swpNoMove = 0x0002
)

// allowSetForegroundWindow gives the process with this id the right to bring
// a window to the front. Windows only lets a process pass that right on if it
// may take the foreground itself, so the caller has to be the foreground
// process or something it started: the native host, which Chrome starts, and
// not the daemon.
func allowSetForegroundWindow(pid int) error {
	ok, _, err := procAllowSetForeground.Call(uintptr(pid))
	if ok == 0 {
		if errno, isErrno := err.(syscall.Errno); isErrno && errno != 0 {
			return errno
		}
		return errors.New("AllowSetForegroundWindow failed")
	}
	return nil
}

// bringToFront puts a window on top and makes it the foreground window. It
// only works in a process that has been allowed the foreground, which for the
// folder chooser means the native host called allowSetForegroundWindow first.
func bringToFront(hwnd uintptr) {
	procSetForegroundWin.Call(hwnd)
	procBringWindowToTop.Call(hwnd)
	procSetWindowPos.Call(hwnd, hwndTop, 0, 0, 0, 0, swpNoMove|swpNoSize)
}
