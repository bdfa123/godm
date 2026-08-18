//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const (
	detachedProcess     = 0x00000008
	createNewProcGroup  = 0x00000200
	createNoWindow      = 0x08000000
	chromeRegRoot       = `HKCU\Software\Google\Chrome\NativeMessagingHosts`
	edgeRegRoot         = `HKCU\Software\Microsoft\Edge\NativeMessagingHosts`
	braveRegRoot        = `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts`
	chromiumRegRoot     = `HKCU\Software\Chromium\NativeMessagingHosts`
	nativeHostAppName   = "com.godm.host"
	installTargetsLabel = "Chrome, Edge, Brave, Chromium"
)

// spawnDetached starts the daemon so it outlives whoever launched it. Without
// DETACHED_PROCESS the daemon dies with the native host when Chrome closes the
// port, which would kill every in-flight download.
func spawnDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcGroup | createNoWindow,
	}
	logFile, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func nativeHostRegRoots() []string {
	return []string{chromeRegRoot, edgeRegRoot, braveRegRoot, chromiumRegRoot}
}

// registerNativeHost writes the manifest pointer into the registry. Shelling
// out to reg.exe keeps this binary dependency-free.
func registerNativeHost(manifestPath string) []string {
	var done []string
	for _, root := range nativeHostRegRoots() {
		key := root + `\` + nativeHostAppName
		cmd := exec.Command("reg", "add", key, "/ve", "/t", "REG_SZ", "/d", manifestPath, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "  skip %s: %v %s\n", key, err, out)
			continue
		}
		done = append(done, key)
	}
	return done
}

func unregisterNativeHost() []string {
	var done []string
	for _, root := range nativeHostRegRoots() {
		key := root + `\` + nativeHostAppName
		cmd := exec.Command("reg", "delete", key, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Run(); err == nil {
			done = append(done, key)
		}
	}
	return done
}

func openInBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}
