//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// spawnApp starts the manager window. Unlike spawnDetached it must not ask for
// a hidden window: that flag applies to the first window the process shows,
// which here is the window the user asked for.
func spawnApp(exe string) error {
	cmd := exec.Command(exe, "app")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcGroup,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// playerPaths lists where the installers put a player that is not on PATH.
func playerPaths(name string) []string {
	var rel []string
	switch name {
	case "mpv":
		rel = []string{`MPV Player\mpv.exe`, `mpv\mpv.exe`, `Programs\mpv\mpv.exe`}
	case "vlc":
		rel = []string{`VideoLAN\VLC\vlc.exe`}
	}
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		if root := os.Getenv(env); root != "" {
			for _, r := range rel {
				out = append(out, root+`\`+r)
			}
		}
	}
	return out
}

func nativeHostRegRoots() []string {
	return []string{chromeRegRoot, edgeRegRoot, braveRegRoot, chromiumRegRoot}
}

// machineScope switches registration from HKCU to HKLM. Managed browsers can
// set the NativeMessagingUserLevelHosts policy to false, which makes Chrome
// ignore every HKCU host and report "Specified native messaging host not
// found". HKLM entries still work, but writing them needs elevation.
var machineScope bool

func nativeHostRegRootsScoped() []string {
	roots := nativeHostRegRoots()
	if !machineScope {
		return roots
	}
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = strings.Replace(r, `HKCU\`, `HKLM\`, 1)
	}
	return out
}

// registerNativeHost writes the manifest pointer into the registry. Shelling
// out to reg.exe keeps this binary dependency-free.
func registerNativeHost(manifestPath string) []string {
	var done []string
	for _, key := range keysFor() {
		cmd := exec.Command("reg", "add", key, "/ve", "/t", "REG_SZ", "/d", manifestPath, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if strings.Contains(msg, "Access is denied") {
				msg = "access denied - run this from an Administrator terminal"
			}
			fmt.Fprintf(os.Stderr, "  skip %s: %s\n", key, msg)
			continue
		}
		done = append(done, key)
	}
	return done
}

func keysFor() []string {
	roots := nativeHostRegRootsScoped()
	keys := make([]string, len(roots))
	for i, r := range roots {
		keys[i] = r + `\` + nativeHostAppName
	}
	return keys
}

// unregisterNativeHost clears both scopes so a cleanup never leaves half of a
// previous install behind.
func unregisterNativeHost() []string {
	var done []string
	for _, root := range nativeHostRegRoots() {
		for _, scoped := range []string{root, strings.Replace(root, `HKCU\`, `HKLM\`, 1)} {
			key := scoped + `\` + nativeHostAppName
			cmd := exec.Command("reg", "delete", key, "/f")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if err := cmd.Run(); err == nil {
				done = append(done, key)
			}
		}
	}
	return done
}

func openInBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// showInFolder opens Explorer with the file selected, or the folder itself.
// The command line is built by hand because Explorer wants /select,"path" as a
// single token and does not follow the usual argument quoting rules.
func showInFolder(path string) error {
	line := `explorer.exe "` + path + `"`
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		line = `explorer.exe /select,"` + path + `"`
	}
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
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

func setMachineScope(v bool) { machineScope = v }

var procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")

const (
	esContinuous     = 0x80000000 // keep the setting until changed, not for one idle period
	esSystemRequired = 0x00000001
)

// setAwake keeps Windows from sleeping on its own while on is true. The state
// belongs to the calling thread. It asks for the system only: without
// ES_DISPLAY_REQUIRED the screen may still turn off.
func setAwake(on bool) error {
	flags := uintptr(esContinuous)
	if on {
		flags |= esSystemRequired
	}
	if r, _, err := procSetThreadExecutionState.Call(flags); r == 0 {
		return fmt.Errorf("SetThreadExecutionState: %v", err)
	}
	return nil
}

// osPower is the real connection to the computer's power state.
func osPower() powerOps {
	return powerOps{keepAwake: setAwake}
}
