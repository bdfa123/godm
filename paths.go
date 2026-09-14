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
