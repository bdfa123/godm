//go:build !windows

package main

// No embedded window outside Windows yet; fall back to the browser so the
// command still does something sensible.
func RunApp() error { return cmdUI() }

func guiAvailable() bool { return false }
