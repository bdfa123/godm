//go:build windows

package main

import (
	"encoding/binary"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// internetSettingsKey is where the Settings app, Internet Options and the
// proxy tools keep the current user's proxy.
const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// readSystemProxy returns the manual proxy from the current user's settings.
// A setup script (AutoConfigURL) is not evaluated; when one is set as well,
// the manual proxy is used if it is switched on, and nothing otherwise.
func readSystemProxy() *systemProxy {
	enabled, server, override, ok := readInternetSettings()
	if !ok {
		return nil
	}
	return parseSystemProxy(enabled, server, override)
}

// readInternetSettings reads the raw values through advapi32, which the
// syscall package already wraps, rather than adding a registry dependency.
func readInternetSettings() (enabled bool, server, override string, ok bool) {
	path, err := syscall.UTF16PtrFromString(internetSettingsKey)
	if err != nil {
		return false, "", "", false
	}
	var k syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &k) != nil {
		return false, "", "", false
	}
	defer syscall.RegCloseKey(k)
	return regNumber(k, "ProxyEnable") != 0, regString(k, "ProxyServer"), regString(k, "ProxyOverride"), true
}

// regQuery returns a value's type and bytes; ok is false if it is missing.
func regQuery(k syscall.Handle, name string) (typ uint32, data []byte, ok bool) {
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, nil, false
	}
	// The value can grow between asking its size and reading it, if a proxy
	// tool rewrites it at that moment; ask again rather than read half.
	for range 3 {
		var size uint32
		if syscall.RegQueryValueEx(k, n, nil, &typ, nil, &size) != nil {
			return 0, nil, false
		}
		if size == 0 {
			return typ, nil, true
		}
		buf := make([]byte, size)
		err := syscall.RegQueryValueEx(k, n, nil, &typ, &buf[0], &size)
		if err == nil {
			return typ, buf[:size], true
		}
		if err != syscall.ERROR_MORE_DATA {
			return 0, nil, false
		}
	}
	return 0, nil, false
}

func regString(k syscall.Handle, name string) string {
	typ, data, ok := regQuery(k, name)
	if !ok || (typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ) || len(data) < 2 {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&data[0])), len(data)/2)
	return syscall.UTF16ToString(u16)
}

// regNumber reads ProxyEnable, which is a DWORD everywhere it is written
// properly; a string "1" from a hand-made .reg file counts too.
func regNumber(k syscall.Handle, name string) uint64 {
	typ, data, ok := regQuery(k, name)
	if !ok {
		return 0
	}
	switch {
	case typ == syscall.REG_DWORD && len(data) >= 4:
		return uint64(binary.LittleEndian.Uint32(data))
	case typ == syscall.REG_QWORD && len(data) >= 8:
		return binary.LittleEndian.Uint64(data)
	case typ == syscall.REG_SZ:
		n, _ := strconv.ParseUint(strings.TrimSpace(regString(k, name)), 0, 64)
		return n
	}
	return 0
}
