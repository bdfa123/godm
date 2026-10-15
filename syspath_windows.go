//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Where Windows keeps the two halves of the environment: the machine's, for
// everyone, and the current user's. A process is given the machine's followed
// by the user's when it is started, which is the order they are read in here.
type environmentHive struct {
	root syscall.Handle
	key  string
}

var environmentHives = []environmentHive{
	{syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
	{syscall.HKEY_CURRENT_USER, `Environment`},
}

// registryPathDirs reads PATH from the registry as it is now, through the same
// advapi32 calls the proxy settings are read with.
func registryPathDirs() []string {
	var dirs []string
	for _, h := range environmentHives {
		if v, expandable, ok := h.value("Path"); ok {
			if expandable {
				v = expandPercent(v, lookupEnvironment)
			}
			dirs = append(dirs, filepath.SplitList(v)...)
		}
	}
	return cleanPathDirs(dirs)
}

// value reads one variable kept in the hive as it is written. expandable says
// it is kept as an expandable string, which a PATH that mentions %USERPROFILE%
// is, and that the variables named in it are still to be replaced.
func (h environmentHive) value(name string) (v string, expandable, ok bool) {
	path, err := syscall.UTF16PtrFromString(h.key)
	if err != nil {
		return "", false, false
	}
	var k syscall.Handle
	if syscall.RegOpenKeyEx(h.root, path, 0, syscall.KEY_QUERY_VALUE, &k) != nil {
		return "", false, false
	}
	defer syscall.RegCloseKey(k)
	typ, data, found := regQuery(k, name)
	if !found || (typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ) || len(data) < 2 {
		return "", false, false
	}
	v = syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&data[0])), len(data)/2))
	return v, typ == syscall.REG_EXPAND_SZ, true
}

// lookupEnvironment is a variable of this process, and failing that one the
// registry holds. An entry like %JAVA_HOME%\bin is only as good as the
// variable, and one set the same day as the program that uses it is no more in
// a process that has been running since before than the program is on its
// PATH. A variable of the registry's that is itself defined in terms of others
// is expanded with this process's alone.
func lookupEnvironment(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	for i := len(environmentHives) - 1; i >= 0; i-- { // the user's before the machine's
		if v, expandable, ok := environmentHives[i].value(name); ok {
			if expandable {
				v = expandPercent(v, os.Getenv)
			}
			return v
		}
	}
	return ""
}
