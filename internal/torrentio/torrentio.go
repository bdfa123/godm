// Package torrentio chooses how the torrent library reads and writes files.
// It exists only for its init, which must run before the library's storage
// package reads the setting in its own init. Go initialises packages in
// import-path order among those whose imports are ready: this one needs only
// os, while storage needs net/http and log/slog, which sort after "godm/" and
// so cannot be initialised until this package has been.
package torrentio

import "os"

func init() {
	// The library's default memory-maps every file a torrent touches and
	// never unmaps it, even after the torrent is dropped. On Windows a mapped
	// file can be neither deleted nor truncated, so a paused or removed
	// torrent would keep its files locked until godm quits, and resuming it
	// would fail to reopen them. Plain reads and writes close the file after
	// every operation.
	os.Setenv("TORRENT_STORAGE_DEFAULT_FILE_IO", "classic")
}
