//go:build !windows

package main

import "fmt"

// There is no notification-area equivalent wired up outside Windows yet, so
// the daemon simply runs without one.
func startTray(*Manager) {}

func trayNotify(title, text, open string) {}

func browseFolder(current string) (string, error) {
	return "", fmt.Errorf("choosing a folder is only supported on Windows; type the path instead")
}
