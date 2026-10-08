package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonStartupLeaseIsExclusiveAndRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.starting")
	release, claimed, err := claimDaemonStartupAt(path)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	if _, claimed, err := claimDaemonStartupAt(path); err != nil || claimed {
		t.Fatalf("concurrent claim = %v, %v; want unclaimed", claimed, err)
	}
	release()

	release, claimed, err = claimDaemonStartupAt(path)
	if err != nil || !claimed {
		t.Fatalf("claim after release = %v, %v", claimed, err)
	}
	release()

	if err := os.WriteFile(path, []byte("crashed starter"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-staleStartupLease - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, claimed, err = claimDaemonStartupAt(path)
	if err != nil || !claimed {
		t.Fatalf("stale recovery = %v, %v", claimed, err)
	}
	release()
}
