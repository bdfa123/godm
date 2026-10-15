//go:build !windows

package main

// registryPathDirs has nothing to read: on macOS and Linux a program's PATH is
// put together by the shell that starts it, and there is no system-wide list to
// look at again.
func registryPathDirs() []string { return nil }
