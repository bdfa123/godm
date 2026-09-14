//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

const (
	nativeHostAppName   = "com.godm.host"
	installTargetsLabel = "Chrome, Edge, Chromium"
)

func spawnDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

// On macOS and Linux the manifest is a file in a per-browser directory rather
// than a registry value.
func nativeHostDirs() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		lib := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(lib, "Google", "Chrome", "NativeMessagingHosts"),
			filepath.Join(lib, "Microsoft Edge", "NativeMessagingHosts"),
			filepath.Join(lib, "Chromium", "NativeMessagingHosts"),
		}
	}
	cfg := filepath.Join(home, ".config")
	return []string{
		filepath.Join(cfg, "google-chrome", "NativeMessagingHosts"),
		filepath.Join(cfg, "microsoft-edge", "NativeMessagingHosts"),
		filepath.Join(cfg, "chromium", "NativeMessagingHosts"),
	}
}

func registerNativeHost(manifestPath string) []string {
	var done []string
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil
	}
	for _, dir := range nativeHostDirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		dst := filepath.Join(dir, nativeHostAppName+".json")
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			continue
		}
		done = append(done, dst)
	}
	return done
}

func unregisterNativeHost() []string {
	var done []string
	for _, dir := range nativeHostDirs() {
		dst := filepath.Join(dir, nativeHostAppName+".json")
		if err := os.Remove(dst); err == nil {
			done = append(done, dst)
		}
	}
	return done
}

func openInBrowser(url string) error {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	return exec.Command(bin, url).Start()
}

func showInFolder(path string) error {
	dir := path
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		dir = filepath.Dir(path)
	}
	return openInBrowser(dir)
}

// There is no Explorer double-click equivalent to detect here.
func launchedByDoubleClick() bool { return false }
func hideConsole()                {}
func alert(title, body string)    { fmt.Fprintf(os.Stderr, "%s: %s\n", title, body) }

func setMachineScope(bool) {}
