//go:build !windows

package main

import "os"

// setSparse has nothing to do here: on the file systems of Linux and macOS a
// file grown by Truncate is already all holes, and nothing is filled in ahead
// of a write.
func setSparse(*os.File, bool) error { return nil }

// diskFree does not look. Without it a download that runs out of room fails
// at the write that finds the disk full, which still names the reason.
func diskFree(string) (int64, bool) { return 0, false }

func allocated(*os.File) (int64, bool) { return 0, false }
