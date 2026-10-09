//go:build !windows

package main

// readSystemProxy has nothing to read here: on macOS and Linux the proxy
// environment variables are the convention, and those are honoured already.
func readSystemProxy() *systemProxy { return nil }
