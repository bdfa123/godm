//go:build !windows

package main

const canBrowseFolders = false

// There is no folder chooser outside Windows, so nothing needs the foreground.
func allowSetForegroundWindow(pid int) error { return nil }
