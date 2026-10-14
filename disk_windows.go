//go:build windows

package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	fsctlSetSparse   = 0x000900c4 // FSCTL_SET_SPARSE
	fileStandardInfo = 1          // FileStandardInfo, for GetFileInformationByHandleEx
)

var (
	procGetDiskFreeSpaceEx           = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetFileInformationByHandleEx = kernel32.NewProc("GetFileInformationByHandleEx")
)

// setSparse makes f a sparse file, or with on false an ordinary one again.
//
// NTFS keeps, for every file, how far into it something has been written,
// and before a write further on completes it fills everything in between
// with zeros, holding up every other write to the file meanwhile. A download
// cut into ranges writes near the end of the file within seconds of starting,
// so its first write there waits for the whole file's worth of zeros: on a
// hard disk, a quarter of an hour for 85 GB, with every connection stuck
// behind it. In a sparse file the ranges not yet written are holes that take
// no space and need no zeros. Once every byte is written there are no holes,
// and making the file ordinary again costs nothing.
//
// A file system that has no sparse files, FAT32 or exFAT, refuses, and the
// file stays as it was.
func setSparse(f *os.File, on bool) error {
	var b byte // FILE_SET_SPARSE_BUFFER, a single BOOLEAN
	if on {
		b = 1
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := rc.Control(func(fd uintptr) {
		var n uint32
		ioErr = syscall.DeviceIoControl(syscall.Handle(fd), fsctlSetSparse, &b, 1, nil, 0, &n, nil)
	}); err != nil {
		return err
	}
	return ioErr
}

// diskFree is how many bytes this user may still write to the volume dir is
// on, which takes quotas into account. ok is false when it cannot be told.
func diskFree(dir string) (free int64, ok bool) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	// A share has to be named with the backslash at its end, and any other
	// folder may be.
	if !strings.HasSuffix(dir, `\`) {
		dir += `\`
	}
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if int32(r) == 0 { // a BOOL: the upper half of the register is not part of it
		return 0, false
	}
	return int64(min(avail, math.MaxInt64)), true
}

// fileStandard is FILE_STANDARD_INFO.
type fileStandard struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  byte
	Directory      byte
}

// allocated is how much of the disk f takes up now. For a sparse file that is
// what has been written, as far as it has reached the disk, and for an
// ordinary one its whole length.
func allocated(f *os.File) (int64, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, false
	}
	var info fileStandard
	var r uintptr
	if err := rc.Control(func(fd uintptr) {
		r, _, _ = procGetFileInformationByHandleEx.Call(fd, fileStandardInfo,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	}); err != nil || int32(r) == 0 {
		return 0, false
	}
	return info.AllocationSize, true
}
