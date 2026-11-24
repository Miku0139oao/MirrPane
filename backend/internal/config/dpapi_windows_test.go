//go:build windows

package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func seedPermissiveDirACL(t *testing.T, dir string) {
	t.Helper()
	permissiveSD, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString: %v", err)
	}
	permissiveDACL, _, err := permissiveSD.DACL()
	if err != nil {
		t.Fatalf("permissiveSD.DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		permissiveDACL,
		nil,
	); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
			t.Skipf("SetNamedSecurityInfo unavailable: %v", err)
		}
		t.Fatalf("seed permissive dir DACL: %v", err)
	}
}

func TestDPAPIRoundTripAndNonDisclosureOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg1, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load fresh: %v", err)
	}
	seed1, err := cfg1.Seed()
	if err != nil || len(seed1) != 32 {
		t.Fatalf("cfg1.Seed() = len %d, err=%v", len(seed1), err)
	}

	rawDisk, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var diskJSON struct {
		KeySeed string `json:"keySeed"`
	}
	if err := json.Unmarshal(rawDisk, &diskJSON); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if !strings.HasPrefix(diskJSON.KeySeed, dpapiPrefix) {
		t.Fatalf("disk keySeed = %q, want %q prefix", diskJSON.KeySeed, dpapiPrefix)
	}
	rawSeedB64 := base64.StdEncoding.EncodeToString(seed1)
	if bytes.Contains(rawDisk, []byte(rawSeedB64)) {
		t.Fatalf("raw 32-byte base64 keySeed leaked in config.json on disk")
	}

	cfg2, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	seed2, err := cfg2.Seed()
	if err != nil {
		t.Fatalf("cfg2.Seed(): %v", err)
	}
	if !bytes.Equal(seed1, seed2) {
		t.Fatalf("seed mismatch after reload: got %x, want %x", seed2, seed1)
	}
	if cfg2.PairingID != cfg1.PairingID || cfg2.DeviceID != cfg1.DeviceID {
		t.Fatalf("identity changed across reload: got (%q, %q), want (%q, %q)", cfg2.PairingID, cfg2.DeviceID, cfg1.PairingID, cfg1.DeviceID)
	}
}

func TestDPAPILegacyPlaintextMigration(t *testing.T) {
	dir := t.TempDir()
	seedPermissiveDirACL(t, dir)

	origSeed := make([]byte, 32)
	for i := range origSeed {
		origSeed[i] = byte(i + 1)
	}
	legacyB64 := base64.StdEncoding.EncodeToString(origSeed)
	legacyPath := filepath.Join(dir, FileName)
	legacyJSON := `{"name":"Legacy","deviceId":"AA:BB:CC:DD:EE:FF","pairingId":"11111111-2222-3333-4444-555555555555","keySeed":"` + legacyB64 + `","width":1920,"height":1080,"maxFps":60,"decoderMode":"auto","colorRange":"auto","scaleFilter":"fast","audioBuffer":"balanced"}`
	if err := os.WriteFile(legacyPath, []byte(legacyJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load legacy plaintext config: %v", err)
	}
	gotSeed, err := cfg.Seed()
	if err != nil {
		t.Fatalf("cfg.Seed(): %v", err)
	}
	if !bytes.Equal(gotSeed, origSeed) {
		t.Fatalf("migrated seed = %x, want %x", gotSeed, origSeed)
	}

	migratedBytes, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(migratedBytes, []byte(legacyB64)) {
		t.Fatalf("legacy plaintext keySeed remained on disk after migration")
	}
	var diskJSON struct {
		KeySeed string `json:"keySeed"`
	}
	if err := json.Unmarshal(migratedBytes, &diskJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(diskJSON.KeySeed, dpapiPrefix) {
		t.Fatalf("migrated keySeed = %q, want %q prefix", diskJSON.KeySeed, dpapiPrefix)
	}

	assertProtectedDirDACL(t, dir)
	assertProtectedUserAndSystemDACL(t, legacyPath)

	reloaded, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load migrated config: %v", err)
	}
	reloadedSeed, err := reloaded.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reloadedSeed, origSeed) {
		t.Fatalf("reloaded seed = %x, want %x", reloadedSeed, origSeed)
	}
}

func TestDPAPITamperAndFailClosed(t *testing.T) {
	validSeed := bytes.Repeat([]byte{0x42}, 32)
	validProtected, err := protectKeySeed(validSeed)
	if err != nil {
		t.Fatalf("protectKeySeed: %v", err)
	}
	validBlob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(validProtected, dpapiPrefix))
	if err != nil {
		t.Fatal(err)
	}
	corruptBlob := append([]byte(nil), validBlob...)
	corruptBlob[len(corruptBlob)-1] ^= 0xff
	corruptProtected := dpapiPrefix + base64.StdEncoding.EncodeToString(corruptBlob)

	shortBlob, err := dpapiProtect(bytes.Repeat([]byte{0x07}, 16))
	if err != nil {
		t.Fatal(err)
	}
	longBlob, err := dpapiProtect(bytes.Repeat([]byte{0x08}, 33))
	if err != nil {
		t.Fatal(err)
	}
	noEntropyBlob, err := dpapiProtectWithEntropy(validSeed, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongEntropyBlob, err := dpapiProtectWithEntropy(validSeed, []byte("WrongDomain.v1"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		keySeed string
	}{
		{"corrupt ciphertext", corruptProtected},
		{"truncated base64", dpapiPrefix + "YWJjZA"},
		{"empty payload", dpapiPrefix},
		{"wrong length 16 bytes", dpapiPrefix + base64.StdEncoding.EncodeToString(shortBlob)},
		{"wrong length 33 bytes", dpapiPrefix + base64.StdEncoding.EncodeToString(longBlob)},
		{"missing domain entropy", dpapiPrefix + base64.StdEncoding.EncodeToString(noEntropyBlob)},
		{"wrong domain entropy", dpapiPrefix + base64.StdEncoding.EncodeToString(wrongEntropyBlob)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			raw := []byte(`{"name":"Tampered","keySeed":"` + tc.keySeed + `"}`)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir, "鏡屏"); err == nil {
				t.Fatalf("Load(%s) succeeded, want fail-closed error", tc.name)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, after) {
				t.Fatalf("Load(%s) overwrote corrupt config.json", tc.name)
			}
			if _, err := (&Config{KeySeed: tc.keySeed}).Seed(); err == nil {
				t.Fatalf("Config.Seed(%s) succeeded, want error", tc.name)
			}
		})
	}
}

func TestDPAPISettingsUpdateAndRenamePreservesSeed(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load fresh: %v", err)
	}
	origSeed, err := cfg.Seed()
	if err != nil {
		t.Fatal(err)
	}

	cfg.AudioBuffer = "stable"
	cfg.Name = "Renamed Receiver"
	if err := cfg.Save(dir); err != nil {
		t.Fatalf("Save updated: %v", err)
	}

	rawDisk, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawDisk, []byte(base64.StdEncoding.EncodeToString(origSeed))) {
		t.Fatal("raw keySeed leaked to disk after Save")
	}

	reloaded, err := Load(dir, "鏡屏")
	if err != nil {
		t.Fatalf("Load updated: %v", err)
	}
	reloadedSeed, err := reloaded.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origSeed, reloadedSeed) {
		t.Fatalf("seed changed across Save/Load: got %x, want %x", reloadedSeed, origSeed)
	}
	if reloaded.AudioBuffer != "stable" || reloaded.Name != "Renamed Receiver" {
		t.Fatalf("updated settings lost: %+v", reloaded)
	}
}
