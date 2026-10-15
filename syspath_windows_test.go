//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// regPath reads a PATH with reg.exe, which shows an expandable string as it is
// written, with the variables in it unexpanded.
func regPath(t *testing.T, key string) (value string, expandable bool) {
	t.Helper()
	out, err := exec.Command("reg", "query", key, "/v", "Path").Output()
	if err != nil {
		t.Skipf("reg.exe could not read %s: %v", key, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "    Path    REG_EXPAND_SZ    C:\Windows;C:\Windows\system32"
		f := strings.SplitN(strings.TrimSpace(line), "    ", 3)
		if len(f) == 3 && f[0] == "Path" {
			return strings.TrimSpace(f[2]), f[1] == "REG_EXPAND_SZ"
		}
	}
	t.Skipf("%s holds no Path", key)
	return "", false
}

// The registry read is checked against reg.exe reading the same keys, which
// works whatever the machine has installed and never changes anything.
func TestRegistryPathMatchesWhatRegShows(t *testing.T) {
	var want []string
	for _, key := range []string{
		`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
		`HKCU\Environment`,
	} {
		v, expandable := regPath(t, key)
		if expandable {
			v = expandPercent(v, lookupEnvironment)
		}
		want = append(want, filepath.SplitList(v)...)
	}
	want = cleanPathDirs(want)

	got := registryPathDirs()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("registryPathDirs() =\n %q\nreg.exe shows\n %q", got, want)
	}
	// Every Windows has its own folder on the machine's PATH.
	system32 := filepath.Join(os.Getenv("SystemRoot"), "System32")
	found := false
	for _, d := range got {
		found = found || strings.EqualFold(filepath.Clean(d), system32)
	}
	if !found {
		t.Errorf("%s is not among the %d folders read", system32, len(got))
	}
	for _, d := range got {
		if strings.Contains(d, "%") {
			t.Errorf("%q still has a variable in it", d)
		}
		if !filepath.IsAbs(d) {
			t.Errorf("%q is not spelled out in full", d)
		}
	}
	t.Logf("%d folders on the system's PATH", len(got))
}

// A program is found by the extensions that make a file a command, as a shell
// would find it.
func TestACommandIsFoundUnderThePathExtensions(t *testing.T) {
	dir := t.TempDir()
	exe := fakeTool(t, dir, "sometool")
	if err := os.WriteFile(filepath.Join(dir, "sometool"), []byte("no extension, no command"), 0o644); err != nil {
		t.Fatal(err)
	}
	useSystemPath(t, dir)
	for _, name := range []string{"sometool", "sometool.exe", "SOMETOOL"} {
		got, err := lookPathNow(name)
		if err != nil || !strings.EqualFold(got, exe) {
			t.Errorf("lookPathNow(%q) = %q, %v, want %s", name, got, err, exe)
		}
	}
	if got, err := lookPathNow("nothere"); err == nil {
		t.Errorf("found %q", got)
	}
	// With no extension a file is not a command on Windows, however it is named.
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if got, err := lookPathNow("sometool"); err == nil {
		t.Errorf("a file with no extension was taken for a command: %s", got)
	}
}
