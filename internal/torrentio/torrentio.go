// Package torrentio chooses how the torrent library reads and writes files.
//
// The library's storage package reads TORRENT_STORAGE_DEFAULT_FILE_IO once,
// in its own init, so the variable has to be set before that runs, and only
// another package's init can run that early. Go has no way to say which goes
// first; what makes it hold is the order Go initialises packages in: sorted
// by import path, each as soon as everything it imports is ready. This
// package imports only os, so it is ready long before storage, which needs
// log/slog and net/http among much else, and "godm/internal/torrentio" sorts
// before both, so it is always taken first. That stays true only while the
// module path sorts before "log/slog": renaming the module to one that does
// not would quietly break it. TestTorrentFilesAreWrittenWithPlainFileIO in
// package main fails if it ever does.
package torrentio

import "os"

const fileIOVar = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// inherited is what the variable held when godm started, if anything.
var inherited, wasSet = os.LookupEnv(fileIOVar)

func init() {
	// The library's default memory-maps every file a torrent touches and
	// never unmaps it, even after the torrent is dropped. On Windows a mapped
	// file can be neither deleted nor truncated, so a paused or removed
	// torrent would keep its files locked until godm quits, and resuming it
	// would fail to reopen them. Plain reads and writes close the file after
	// every operation.
	os.Setenv(fileIOVar, "classic")
}

// Restore puts the variable back as godm found it. Package main calls it from
// its own init, by which time the library has read it, so that yt-dlp,
// ffmpeg and players started later do not inherit a setting meant for godm.
func Restore() {
	if wasSet {
		os.Setenv(fileIOVar, inherited)
	} else {
		os.Unsetenv(fileIOVar)
	}
}
