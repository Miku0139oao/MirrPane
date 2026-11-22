package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConfigDefaultsAndNormalization(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load fresh: %v", err)
	}
	if cfg.Name != "鏡屏" {
		t.Fatalf("Name = %q, want 鏡屏", cfg.Name)
	}
	if cfg.Width != 1920 || cfg.Height != 1080 || cfg.MaxFPS != 60 {
		t.Fatalf("display = %dx%d@%d, want 1920x1080@60", cfg.Width, cfg.Height, cfg.MaxFPS)
	}
	if cfg.DecoderMode != "auto" || cfg.ColorRange != "auto" ||
		cfg.ScaleFilter != "fast" || cfg.AudioBuffer != "balanced" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	// Write extreme / invalid values and verify Load normalizes them safely.
	bad := `{
		"name": "Custom",
		"width": 99999,
		"height": 100,
		"maxFps": 240,
		"decoderMode": "bogus",
		"colorRange": "bogus",
		"scaleFilter": "bogus",
		"audioBuffer": "bogus"
	}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load invalid: %v", err)
	}
	if cfg2.Name != "Custom" || cfg2.Width != 4096 || cfg2.Height != 480 || cfg2.MaxFPS != 60 {
		t.Fatalf("clamping failed: %+v", cfg2)
	}
	if cfg2.DecoderMode != "auto" || cfg2.ColorRange != "auto" ||
		cfg2.ScaleFilter != "fast" || cfg2.AudioBuffer != "balanced" {
		t.Fatalf("enum fallback failed: %+v", cfg2)
	}

	// Valid custom settings survive round-trip.
	cfg2.Width = 2560
	cfg2.Height = 1440
	cfg2.MaxFPS = 30
	cfg2.DecoderMode = "software"
	cfg2.ColorRange = "full"
	cfg2.ScaleFilter = "bicubic"
	cfg2.AudioBuffer = "low"
	if err := cfg2.Save(dir); err != nil {
		t.Fatal(err)
	}
	cfg3, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.Width != 2560 || cfg3.Height != 1440 || cfg3.MaxFPS != 30 ||
		cfg3.DecoderMode != "software" || cfg3.ColorRange != "full" ||
		cfg3.ScaleFilter != "bicubic" || cfg3.AudioBuffer != "low" {
		t.Fatalf("round-trip mismatch: %+v", cfg3)
	}
}

func TestRandomKeySeedAndPreservedExistingSeed(t *testing.T) {
	cfg1, err := Load(t.TempDir(), "鏡屏")
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	cfg2, err := Load(t.TempDir(), "鏡屏")
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	seed1, err := cfg1.Seed()
	if err != nil || len(seed1) != 32 {
		t.Fatalf("seed1 = %d bytes, err=%v", len(seed1), err)
	}
	seed2, err := cfg2.Seed()
	if err != nil || len(seed2) != 32 {
		t.Fatalf("seed2 = %d bytes, err=%v", len(seed2), err)
	}
	if cfg1.KeySeed == cfg2.KeySeed {
		t.Fatal("fresh configs generated identical key seeds; expected random CSPRNG seeds")
	}

	// Existing valid keySeed must be preserved without forced rotation.
	dir := t.TempDir()
	legacySeed := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"name":"Legacy","keySeed":"`+legacySeed+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if loaded.KeySeed != legacySeed {
		t.Fatalf("existing keySeed rotated: got %q, want %q", loaded.KeySeed, legacySeed)
	}
}

func TestConfigSaveAtomicAndMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"name":"Existing"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("unexpected files after atomic Save: %v", entries)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestLoadDoesNotRewriteUnchangedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	seed := base64.StdEncoding.EncodeToString(make([]byte, 32))
	raw := []byte(`{"name":"Stable","deviceId":"AA:BB:CC:DD:EE:FF","pairingId":"11111111-2222-3333-4444-555555555555","keySeed":"` + seed + `","width":1920,"height":1080,"maxFps":60,"decoderMode":"auto","colorRange":"auto","scaleFilter":"fast","audioBuffer":"balanced"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load unchanged: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatalf("Load rewrote unchanged config.json:\n got: %s\nwant: %s", after, raw)
	}
}
