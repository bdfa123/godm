//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Windows only shows notifications from a desktop program, and only keeps them
// in the notification centre, when the program has an AppUserModelID and a
// Start menu shortcut carrying the same ID. Without that the balloons are
// silently dropped. The shortcut is useful in its own right: godm shows up in
// the Start menu instead of living as a loose exe.
const appUserModelID = "godm.DownloadManager"

var (
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procSetAppID         = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type propertyKey struct {
	fmtid guid
	pid   uint32
}

type propVariant struct {
	VT  uint16
	_   [6]byte
	Val uintptr
	_   [8]byte
}

var (
	clsidShellLink   = guid{0x00021401, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW    = guid{0x000214F9, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPropertyStore = guid{0x886D8EEB, 0x8CF2, 0x4446, [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}
	iidPersistFile   = guid{0x0000010B, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}

	pkeyAppUserModelID = propertyKey{
		fmtid: guid{0x9F4C2855, 0x9F79, 0x4B39, [8]byte{0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3}},
		pid:   5,
	}
)

// A COM interface pointer is a pointer to a pointer to a table of methods.
// Modelling it as a typed pointer keeps the conversions ones the compiler and
// vet accept, unlike casting a uintptr back into a pointer.
type comVTable struct{ method [64]uintptr }

type comObject struct{ vtbl *comVTable }

// comCall invokes the nth method, passing the object itself first as C++ does.
func comCall(obj *comObject, index int, args ...uintptr) uintptr {
	all := append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)
	ret, _, _ := syscall.SyscallN(obj.vtbl.method[index], all...)
	return ret
}

func comRelease(obj *comObject) {
	if obj != nil {
		comCall(obj, 2)
	}
}

// setAppID gives this process the identity the shortcut declares.
func setAppID() {
	procSetAppID.Call(uintptr(unsafe.Pointer(utf16(appUserModelID))))
}

func startMenuShortcutPath() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", fmt.Errorf("APPDATA is not set")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "godm.lnk"), nil
}

// ensureStartMenuShortcut writes the shortcut if it is missing. It is not
// rewritten on every start: the user may have moved or pinned it.
func ensureStartMenuShortcut() error {
	path, err := startMenuShortcutPath()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// A marker records which binary the shortcut points at, so a shortcut left
	// behind by a different build (or an exe that moved) is rewritten rather
	// than silently pointing nowhere.
	marker := filepath.Join(configDir(), "shortcut.txt")
	if _, err := os.Stat(path); err == nil {
		if prev, err := os.ReadFile(marker); err == nil && string(prev) == exe {
			return nil
		}
	}
	if err := writeShortcut(path, exe, "app", iconFile()); err != nil {
		return err
	}
	os.WriteFile(marker, []byte(exe), 0o644)
	return nil
}

func writeShortcut(lnk, target, args, icon string) error {
	var link *comObject
	hr := createInstance(&clsidShellLink, &iidShellLinkW, &link)
	if hr != 0 || link == nil {
		return fmt.Errorf("CoCreateInstance(ShellLink) failed: 0x%x", hr)
	}
	defer comRelease(link)

	// IShellLinkW: SetPath is 20, SetArguments 11, SetIconLocation 17,
	// SetWorkingDirectory 9, SetDescription 7.
	comCall(link, 20, uintptr(unsafe.Pointer(utf16(target))))
	comCall(link, 11, uintptr(unsafe.Pointer(utf16(args))))
	comCall(link, 9, uintptr(unsafe.Pointer(utf16(filepath.Dir(target)))))
	comCall(link, 7, uintptr(unsafe.Pointer(utf16("godm download manager"))))
	comCall(link, 17, uintptr(unsafe.Pointer(utf16(icon))), 0)

	var store *comObject
	if hr := comCall(link, 0, uintptr(unsafe.Pointer(&iidPropertyStore)), uintptr(unsafe.Pointer(&store))); hr != 0 {
		return fmt.Errorf("QueryInterface(IPropertyStore) failed: 0x%x", hr)
	}
	defer comRelease(store)

	pv := propVariant{VT: 31, Val: uintptr(unsafe.Pointer(utf16(appUserModelID)))} // VT_LPWSTR
	if hr := comCall(store, 6, uintptr(unsafe.Pointer(&pkeyAppUserModelID)), uintptr(unsafe.Pointer(&pv))); hr != 0 {
		return fmt.Errorf("set AppUserModelID failed: 0x%x", hr)
	}
	if hr := comCall(store, 7); hr != 0 { // Commit
		return fmt.Errorf("commit properties failed: 0x%x", hr)
	}

	var persist *comObject
	if hr := comCall(link, 0, uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&persist))); hr != 0 {
		return fmt.Errorf("QueryInterface(IPersistFile) failed: 0x%x", hr)
	}
	defer comRelease(persist)

	if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
		return err
	}
	if hr := comCall(persist, 6, uintptr(unsafe.Pointer(utf16(lnk))), 1); hr != 0 { // Save
		return fmt.Errorf("saving %s failed: 0x%x", lnk, hr)
	}
	return nil
}

func createInstance(clsid, iid *guid, out **comObject) uintptr {
	const clsctxInprocServer = 1
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out)))
	return hr
}
