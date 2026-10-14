package main

import "fmt"

// freeSpace is diskFree; the tests replace it to fill a disk.
var freeSpace = diskFree

// roomFor fails when the volume dir is on has less than need bytes free. It
// says nothing when the free space cannot be told: the download then finds
// out at the write that fills the disk, as it would have anyway.
func roomFor(dir string, need int64) error {
	if need <= 0 {
		return nil
	}
	free, ok := freeSpace(dir)
	if !ok || need <= free {
		return nil
	}
	return fmt.Errorf("not enough free space in %s: the download needs %s more and %s is free",
		dir, humanBytes(need), humanBytes(free))
}
