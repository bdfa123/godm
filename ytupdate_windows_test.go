package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// The daemon has no console, so a console program it starts would be given a
// window of its own to show: for the minute winget takes, say. Every program of
// an update is started without one.
func TestProgramsOfAnUpdateAreStartedWithoutAConsoleWindow(t *testing.T) {
	has := func(c *exec.Cmd) bool {
		return c.SysProcAttr != nil && c.SysProcAttr.HideWindow && c.SysProcAttr.CreationFlags&createNoWindow != 0
	}
	c := exec.Command(os.Args[0])
	hideWindow(c)
	if !has(c) {
		t.Errorf("hideWindow left %+v", c.SysProcAttr)
	}
	// And runCaptured, which runs each of them, asks for it.
	c = exec.CommandContext(context.Background(), os.Args[0])
	c.Env = append(os.Environ(), stubEnv+"=refuse")
	runCaptured(c)
	if !has(c) {
		t.Errorf("runCaptured started a program with %+v", c.SysProcAttr)
	}
}
