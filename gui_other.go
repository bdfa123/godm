//go:build !windows

package main

import "os"

// No embedded window outside Windows yet; fall back to the browser so the
// command still does something sensible.
func RunApp() error { return cmdUI() }

func guiAvailable() bool { return false }

// showManager brings the manager up for a caller that must not be given its
// address, which is the browser extension asking through the native host. It
// starts "godm app" on its own rather than opening the page from here: the
// host's standard output is its line to the browser, and cmdUI prints to it.
func showManager() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return spawnApp(exe)
}
