//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The registry read is checked against reg.exe reading the same key, which
// works whatever the machine's settings are and never changes them.
func TestInternetSettingsMatchWhatRegShows(t *testing.T) {
	out, err := exec.Command("reg", "query", `HKCU\`+internetSettingsKey).Output()
	if err != nil {
		t.Skipf("reg.exe could not read the key: %v", err)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		// "    ProxyServer    REG_SZ    127.0.0.1:7897"
		f := strings.SplitN(strings.TrimSpace(line), "    ", 3)
		if len(f) == 3 && strings.HasPrefix(f[1], "REG_") {
			vals[f[0]] = strings.TrimSpace(f[2])
		}
	}

	enabled, server, override, ok := readInternetSettings()
	if !ok {
		t.Fatal("the key reg.exe just read could not be opened")
	}
	n, _ := strconv.ParseUint(vals["ProxyEnable"], 0, 64)
	if want := n != 0; enabled != want {
		t.Errorf("ProxyEnable = %v, reg.exe says %q", enabled, vals["ProxyEnable"])
	}
	if server != vals["ProxyServer"] {
		t.Errorf("ProxyServer = %q, reg.exe says %q", server, vals["ProxyServer"])
	}
	if override != vals["ProxyOverride"] {
		t.Errorf("ProxyOverride = %q, reg.exe says %q", override, vals["ProxyOverride"])
	}
	t.Logf("enabled=%v server=%q, %d bypass entries", enabled, server, strings.Count(override, ";")+1)
}
