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

// startTree starts cmd so that cancelling its context ends everything it went
// on to start as well, not only the one process: yt-dlp runs ffmpeg, and
// ending yt-dlp alone leaves that going. The process is made the leader of a
// group of its own, which its children join by being born. cmd must come from
// exec.CommandContext. The returned function is called once cmd.Wait has
// returned.
func startTree(cmd *exec.Cmd) (release func(), err error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// The process itself first: that says whether it had already ended
		// by itself, in which case what it left running stays.
		err := cmd.Process.Kill()
		if err == nil {
			// A negative id names the group.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// hideWindow has nothing to do where a program does not open a window by
// being started.
func hideWindow(cmd *exec.Cmd) {}

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

func spawnApp(exe string) error { return spawnDetached(exe, "app") }

// playerPaths covers the macOS app bundles; elsewhere players are on PATH.
func playerPaths(name string) []string {
	switch name {
	case "mpv":
		return []string{"/Applications/mpv.app/Contents/MacOS/mpv"}
	case "vlc":
		return []string{"/Applications/VLC.app/Contents/MacOS/VLC"}
	}
	return nil
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

// osPower has nothing to offer here: nothing keeps the machine awake, and the
// settings dialog hides what cannot work.
func osPower() powerOps { return powerOps{} }
