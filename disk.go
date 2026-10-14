package main

import (
	"fmt"
	"sync"
)

// freeSpace and diskOf are diskFree and volumeOf; the tests replace them to
// fill a disk of their own.
var (
	freeSpace = diskFree
	diskOf    = volumeOf
)

// A claim is the room a running download still needs on a disk: what left
// says it has yet to write there.
type claim struct {
	vol  string
	left func() int64
}

// claims are the downloads writing now, on every disk.
var claims struct {
	sync.Mutex
	all map[*claim]struct{}
}

// claimRoom checks that the disk dir is on has room for what left says a
// download still has to write, and holds that room for it until release,
// which is due once it stops writing: done, paused or failed.
//
// A file given its whole length used to take all its space the moment it was
// made, so a disk too small for the second of two big downloads refused it at
// once and the first went on to finish. A sparse file takes its space only as
// it is written, and every download starting while the others have written
// little would see the same free space: three of 50 GB on a disk with 100 GB
// free would all start, and all fail at a third of the way with none of them
// finished. So what the downloads running on the same disk still have to
// write counts as taken. Each one's left is asked afresh, as it goes down
// while they write and can also go up (see task.unwritten).
//
// When the free space cannot be told it says nothing: the download then
// finds out at the write that fills the disk, as it would have anyway.
func claimRoom(dir string, left func() int64) (release func(), err error) {
	vol := diskOf(dir)
	claims.Lock()
	defer claims.Unlock()
	if need := left(); need > 0 {
		if free, ok := freeSpace(dir); ok {
			var others int64
			for c := range claims.all {
				if c.vol == vol {
					others += max(c.left(), 0)
				}
			}
			if need > free-others {
				if others == 0 {
					return nil, fmt.Errorf("not enough free space in %s: the download needs %s more and %s is free",
						dir, humanBytes(need), humanBytes(free))
				}
				return nil, fmt.Errorf("not enough free space in %s: the download needs %s more, "+
					"and of the %s free the downloads already running on the same disk still need %s",
					dir, humanBytes(need), humanBytes(free), humanBytes(others))
			}
		}
	}
	c := &claim{vol: vol, left: left}
	if claims.all == nil {
		claims.all = map[*claim]struct{}{}
	}
	claims.all[c] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			claims.Lock()
			delete(claims.all, c)
			claims.Unlock()
		})
	}, nil
}
