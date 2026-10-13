//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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

var (
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procCreateJobObject          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procNtResumeProcess          = ntdll.NewProc("NtResumeProcess")
)

const (
	createSuspended         = 0x00000004
	processTerminate        = 0x0001
	processSetQuota         = 0x0100
	processSuspendResume    = 0x0800
	jobExtendedLimitInfo    = 9      // JobObjectExtendedLimitInformation
	jobLimitKillOnJobClose  = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	jobTerminatedExitStatus = 1
)

// jobLimits is JOBOBJECT_EXTENDED_LIMIT_INFORMATION. Only the flags are ever
// set; the rest is there because Windows checks the size.
type jobLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
	// The C structure is laid out to eight bytes, which on a 32-bit build
	// leaves a gap here that Go would not.
	_                     [8 - unsafe.Sizeof(uintptr(0))]byte
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func setJobLimitFlags(job syscall.Handle, flags uint32) bool {
	info := jobLimits{LimitFlags: flags}
	r, _, _ := procSetInformationJobObject.Call(uintptr(job), jobExtendedLimitInfo,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	return int32(r) != 0 // a BOOL: the upper half of the register is not part of it
}

// startTree starts cmd so that cancelling its context ends everything it went
// on to start as well, not only the one process. A program like yt-dlp.exe is
// a launcher whose child does the work, with ffmpeg as a child of that, and
// ending the launcher alone leaves the rest downloading. Windows has no notion
// of a process and its descendants except a job: whatever a process in a job
// starts is in the job too. cmd must come from exec.CommandContext. The
// returned function is called once cmd.Wait has returned.
//
// The job is also set to end with its last handle, which is ours, so the tree
// does not outlive a godm that died without cancelling anything.
func startTree(cmd *exec.Cmd) (release func(), err error) {
	h, _, _ := procCreateJobObject.Call(0, 0)
	job := syscall.Handle(h)
	if job == 0 || !setJobLimitFlags(job, jobLimitKillOnJobClose) {
		// No job, no tree: stop the one process as before rather than refuse
		// to run it at all.
		if job != 0 {
			syscall.CloseHandle(job)
		}
		return func() {}, cmd.Start()
	}
	// The process has to be in the job before it can start anything, so it is
	// created with its only thread held back and let go once it is in.
	suspend := procNtResumeProcess.Find() == nil
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if suspend {
		cmd.SysProcAttr.CreationFlags |= createSuspended
	}
	cmd.Cancel = func() error {
		// The process itself first: that says whether it had already ended
		// by itself, and it is all there is to do for one that never made it
		// into the job.
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return err
		}
		if r, _, _ := procTerminateJobObject.Call(uintptr(job), jobTerminatedExitStatus); int32(r) != 0 {
			return nil
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		syscall.CloseHandle(job)
		return nil, err
	}
	resumed := !suspend
	proc, err := syscall.OpenProcess(processSetQuota|processTerminate|processSuspendResume,
		false, uint32(cmd.Process.Pid))
	if err == nil {
		// A refusal leaves it outside the job, where cancelling still ends
		// the process itself.
		procAssignProcessToJobObject.Call(uintptr(job), uintptr(proc))
		if suspend {
			if status, _, _ := procNtResumeProcess.Call(uintptr(proc)); uint32(status) == 0 {
				resumed = true
			} else {
				err = fmt.Errorf("NtResumeProcess: status %#x", uint32(status))
			}
		}
		syscall.CloseHandle(proc)
	}
	if !resumed {
		// Nothing will ever wake it, so it is no use to anyone.
		cmd.Process.Kill()
		cmd.Wait()
		syscall.CloseHandle(job)
		return nil, fmt.Errorf("it was created but could not be set running: %v", err)
	}
	return func() {
		// After a cancel the job is empty. Otherwise the process ended by
		// itself, and anything it left running it left on purpose (a player
		// started by the user's own configuration, say), which only a cancel
		// is meant to end.
		setJobLimitFlags(job, 0)
		syscall.CloseHandle(job)
	}, nil
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

var (
	powrprof            = syscall.NewLazyDLL("powrprof.dll")
	procSetSuspendState = powrprof.NewProc("SetSuspendState")
)

// sleepNow puts the computer to sleep, not into hibernation, and returns when
// it wakes. The result is a one-byte BOOLEAN, so the rest of the register is
// not ours to read. The force flag does nothing on current Windows.
func sleepNow() error {
	r, _, err := procSetSuspendState.Call(0, 0, 0)
	if byte(r) == 0 {
		return fmt.Errorf("SetSuspendState: %v", err)
	}
	return nil
}

// shutdownArgs is the command line for a shutdown that waits first. The
// comment is what Windows shows while it counts down.
func shutdownArgs(delay time.Duration, reason string) []string {
	return []string{"/s", "/t", strconv.Itoa(int(delay / time.Second)), "/c", reason}
}

// shutdownIn lets shutdown.exe do the counting, so that the countdown is
// Windows' own and "shutdown /a" from any prompt cancels it, even if godm is
// gone by then.
func shutdownIn(delay time.Duration, reason string) error {
	exe := "shutdown.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		exe = filepath.Join(root, "System32", "shutdown.exe")
	}
	cmd := exec.Command(exe, shutdownArgs(delay, reason)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("shutdown.exe: %v", err)
	}
	return nil
}

// osPower is the real connection to the computer's power state.
func osPower() powerOps {
	return powerOps{keepAwake: setAwake, sleep: sleepNow, shutdown: shutdownIn}
}
