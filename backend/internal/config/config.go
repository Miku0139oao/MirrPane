// Package config stores the receiver's persistent settings beside the
// executable, so the whole product stays portable: nothing outside its own
// folder is written.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// FileName is the settings file inside the data directory.
const FileName = "config.json"

// Config is everything that survives a restart.
type Config struct {
	// Name is the receiver name shown in the sender's list.
	Name string `json:"name"`
	// DeviceID is the MAC-style identifier the receiver presents.
	DeviceID string `json:"deviceId"`
	// PairingID is the stable `pi` UUID advertised over mDNS.
	PairingID string `json:"pairingId"`
	// KeySeed is the base64 Ed25519 seed for the pairing identity.
	KeySeed string `json:"keySeed"`

	// Graphics and audio runtime preferences (applied on receiver restart).
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	MaxFPS      int    `json:"maxFps,omitempty"`
	DecoderMode string `json:"decoderMode,omitempty"` // auto | software
	ColorRange  string `json:"colorRange,omitempty"`  // auto | full | limited
	ScaleFilter string `json:"scaleFilter,omitempty"` // fast | bilinear | bicubic
	AudioBuffer string `json:"audioBuffer,omitempty"` // low | balanced | stable
}

// Load reads the config from dir, creating it on first run. The defaults are
// derived from the machine so that two installations do not collide.
func Load(dir, defaultName string) (*Config, error) {
	path := filepath.Join(dir, FileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
		if err := c.fill(defaultName); err != nil {
			return nil, err
		}
		if err := c.Save(dir); err != nil {
			return nil, err
		}
		return &c, nil
	case errors.Is(err, os.ErrNotExist):
		c := &Config{}
		if err := c.fill(defaultName); err != nil {
			return nil, err
		}
		if err := c.Save(dir); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
}

// fill supplies anything a hand-edited or older file is missing.
func (c *Config) fill(defaultName string) error {
	if c.Name == "" {
		c.Name = defaultName
	}
	if c.DeviceID == "" {
		c.DeviceID = protocol.HardwareAddress()
	}
	if c.PairingID == "" {
		c.PairingID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("airplay-go/"+c.DeviceID)).String()
	}
	if c.KeySeed == "" {
		seed, err := deriveKeySeed(c.DeviceID)
		if err != nil {
			return err
		}
		c.KeySeed = seed
	}
	if c.Width <= 0 {
		c.Width = 1920
	} else if c.Width < 640 {
		c.Width = 640
	} else if c.Width > 4096 {
		c.Width = 4096
	}
	if c.Height <= 0 {
		c.Height = 1080
	} else if c.Height < 480 {
		c.Height = 480
	} else if c.Height > 2304 {
		c.Height = 2304
	}
	if c.MaxFPS != 30 && c.MaxFPS != 60 {
		c.MaxFPS = 60
	}
	switch c.DecoderMode {
	case "auto", "software":
	default:
		c.DecoderMode = "auto"
	}
	switch c.ColorRange {
	case "auto", "full", "limited":
	default:
		c.ColorRange = "auto"
	}
	switch c.ScaleFilter {
	case "fast", "bilinear", "bicubic":
	default:
		c.ScaleFilter = "fast"
	}
	switch c.AudioBuffer {
	case "low", "balanced", "stable":
	default:
		c.AudioBuffer = "balanced"
	}
	return nil
}

// deriveKeySeed produces the pairing seed for a fresh installation.
//
// The device identifier and the pairing UUID are both derived from the machine,
// so two copies of the product on one PC present the same `pi` over mDNS. If the
// seed were random, the two copies would advertise the same identity with
// different keys, and an iPhone that had paired with one would refuse the other
// with no way to tell them apart. Deriving the seed the same way keeps one
// machine to one AirPlay identity, which is what a user copying the folder
// expects.
//
// A machine without a readable MachineGuid falls back to a random seed.
func deriveKeySeed(deviceID string) (string, error) {
	if secret := machineSecret(); secret != "" {
		sum := sha256.Sum256([]byte("airplay-go/key-seed/v1|" + secret + "|" + deviceID))
		return base64.StdEncoding.EncodeToString(sum[:]), nil
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("config: generate key seed: %w", err)
	}
	return base64.StdEncoding.EncodeToString(seed), nil
}

// Save writes the config atomically enough for our purposes: a crash mid-write
// leaves a truncated file, which Load reports rather than silently replacing.
func (c *Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// Seed decodes the stored Ed25519 seed.
func (c *Config) Seed() ([]byte, error) {
	seed, err := base64.StdEncoding.DecodeString(c.KeySeed)
	if err != nil {
		return nil, fmt.Errorf("config: decode keySeed: %w", err)
	}
	return seed, nil
}
