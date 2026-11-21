package config

import (
	"os"
	"path/filepath"
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
