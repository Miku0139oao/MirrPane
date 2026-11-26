package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
)

func TestActiveReceiverProtectsSettingsAndIdentity(t *testing.T) {
	install := t.TempDir()
	if err := SaveReceiverName(install, "既有設定"); err != nil {
		t.Fatal(err)
	}
	path := ConfigPath(install)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := config.LockReceiver(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := ResetIdentity(install); !errors.Is(err, config.ErrReceiverRunning) {
		t.Fatalf("reset while running: %v", err)
	}
	if err := SaveReceiverName(install, "另一個視窗"); !errors.Is(err, config.ErrReceiverRunning) {
		t.Fatalf("edit while running: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("live identity changed: %v", err)
	}
}
