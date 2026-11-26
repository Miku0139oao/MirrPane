package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestExclusiveReceiverIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	first, err := LockReceiver(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	other, err := LockReceiver(t.TempDir())
	if err != nil {
		t.Fatalf("independent install: %v", err)
	}
	defer other.Close()
	if second, err := LockReceiver(dir); !errors.Is(err, ErrReceiverRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("competing owner: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := LockReceiver(dir)
	if err != nil {
		t.Fatalf("restart after release: %v", err)
	}
	restarted.Close()
}
