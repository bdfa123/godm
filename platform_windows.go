//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
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

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	user32                  = syscall.NewLazyDLL("user32.dll")
	procGetConsoleProcList  = kernel32.NewProc("GetConsoleProcessList")
	procGetConsoleWindow    = kernel32.NewProc("GetConsoleWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procAllocConsoleMessage = user32.NewProc("MessageBoxW")
)

const swHide = 0

// launchedByDoubleClick reports whether Explorer started us rather than a
// shell. A console started just for us has exactly one process attached: this
// one. Run from cmd or PowerShell, the shell is attached too, so the count is
// at least two.
func launchedByDoubleClick() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// hideConsole tucks away the black window Explorer opened for us, so a
// double-click looks like launching an app rather than running a script.
func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

// alert shows a message box, the only way to report a startup failure once the
// console is hidden.
func alert(title, body string) {
	t, _ := syscall.UTF16PtrFromString(title)
	b, _ := syscall.UTF16PtrFromString(body)
	procAllocConsoleMessage.Call(0,
		uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), 0x10) // MB_ICONERROR
}
