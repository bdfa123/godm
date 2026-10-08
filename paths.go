package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// configDir holds the token, the port file and the native-messaging manifest.
func configDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	d := filepath.Join(base, "godm")
	os.MkdirAll(d, 0o700)
	return d
}

func defaultDownloadDir() string {
	if d := os.Getenv("GODM_DOWNLOAD_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// virtualizedBy returns the name of the app package whose private storage
// received a file we just wrote under %LOCALAPPDATA%, or "" if the write went
// where it appears to. Package identity APIs are no help here: children of a
// packaged app are redirected without carrying the identity themselves, so the
// only reliable signal is looking for the redirected copy.
func virtualizedBy(realPath string) string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	rel, err := filepath.Rel(local, realPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	packages := filepath.Join(local, "Packages")
	matches, _ := filepath.Glob(filepath.Join(packages, "*", "LocalCache", "Local", rel))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || time.Since(fi.ModTime()) > 2*time.Minute {
			continue // a stale copy from some earlier run, not this write
		}
		if inner, err := filepath.Rel(packages, m); err == nil {
			return strings.SplitN(inner, string(filepath.Separator), 2)[0]
		}
	}
	return ""
}

func tokenPath() string { return filepath.Join(configDir(), "token") }
func portPath() string  { return filepath.Join(configDir(), "port") }
func logPath() string   { return filepath.Join(configDir(), "godm.log") }
func startupPath() string {
	return filepath.Join(configDir(), "daemon.starting")
}

const staleStartupLease = 30 * time.Second

// claimDaemonStartupAt serialises the short interval between checking for an
// existing daemon and publishing a newly listening one. The marker is only a
// startup lease, not a lifetime lock: once /api/ping answers, the port file is
// the source of truth again. A crashed starter becomes recoverable after a
// short grace period.
func claimDaemonStartupAt(path string) (release func(), claimed bool, err error) {
	marker := strconv.Itoa(os.Getpid()) + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
	for attempt := 0; attempt < 2; attempt++ {
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr == nil {
			if _, err = f.WriteString(marker); err == nil {
				err = f.Close()
			} else {
				_ = f.Close()
			}
			if err != nil {
				_ = os.Remove(path)
				return nil, false, err
			}
			return func() {
				// Never remove a lease that replaced ours after stale recovery.
				if b, readErr := os.ReadFile(path); readErr == nil && string(b) == marker {
					_ = os.Remove(path)
				}
			}, true, nil
		}
		if !os.IsExist(openErr) {
			return nil, false, openErr
		}
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > staleStartupLease {
			if removeErr := os.Remove(path); removeErr == nil || os.IsNotExist(removeErr) {
				continue
			}
		}
		return nil, false, nil
	}
	return nil, false, nil
}

func claimDaemonStartup() (func(), bool, error) {
	return claimDaemonStartupAt(startupPath())
}

// loadOrCreateToken returns the shared secret that gates the local HTTP API.
// Without it any web page you visit could POST jobs to the daemon.
func loadOrCreateToken() (string, error) {
	if b, err := os.ReadFile(tokenPath()); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	if err := os.WriteFile(tokenPath(), []byte(t), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func readToken() string {
	b, _ := os.ReadFile(tokenPath())
	return strings.TrimSpace(string(b))
}

func writePort(p int) error {
	return os.WriteFile(portPath(), []byte(strconv.Itoa(p)), 0o600)
}

func readPort() int {
	b, err := os.ReadFile(portPath())
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return p
}
