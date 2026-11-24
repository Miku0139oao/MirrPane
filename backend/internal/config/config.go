// Package config stores the receiver's persistent settings beside the
// executable, so the whole product stays portable: nothing outside its own
// folder is written.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

const (
	// FileName is the settings file inside the data directory.
	FileName = "config.json"

	dpapiPrefix  = "dpapi:"
	keySeedBytes = 32
)

// Config is everything that survives a restart.
type Config struct {
	// Name is the receiver name shown in the sender's list.
	Name string `json:"name"`
	// DeviceID is the MAC-style identifier the receiver presents.
	DeviceID string `json:"deviceId"`
	// PairingID is the stable `pi` UUID advertised over mDNS.
	PairingID string `json:"pairingId"`
	// KeySeed is the base64 Ed25519 seed for the pairing identity. On Windows,
	// Save persists this field DPAPI-encrypted with a "dpapi:" prefix while
	// keeping the decoded raw base64 seed in memory.
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
		wasProtected := false
		if c.KeySeed != "" {
			seed, protected, err := decodeKeySeed(c.KeySeed)
			if err != nil {
				return nil, err
			}
			wasProtected = protected
			c.KeySeed = base64.StdEncoding.EncodeToString(seed)
			clear(seed)
		}
		before := c
		if err := c.fill(defaultName); err != nil {
			return nil, err
		}
		if c != before || (!wasProtected && dpapiProtectAtRest) {
			if err := c.Save(dir); err != nil {
				return nil, err
			}
		} else {
			if err := RestrictDirACL(dir); err != nil {
				return nil, fmt.Errorf("config: restrict dir %s: %w", dir, err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, fmt.Errorf("config: chmod %s: %w", path, err)
			}
			if err := RestrictFileACL(path); err != nil {
				return nil, fmt.Errorf("config: restrict %s: %w", path, err)
			}
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
		// PairingID is the advertised identity; it must be unique per install so
		// two copies of the product on one machine cannot advertise the same
		// identity with different keys.
		id, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("config: generate pairing id: %w", err)
		}
		c.PairingID = id.String()
	}
	if c.KeySeed == "" {
		seed, err := generateKeySeed()
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

// generateKeySeed creates a fresh 32-byte random pairing seed.
func generateKeySeed() (string, error) {
	seed := make([]byte, keySeedBytes)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("config: generate key seed: %w", err)
	}
	defer clear(seed)
	return base64.StdEncoding.EncodeToString(seed), nil
}

// Save writes the config atomically with mode 0600 via temp file + rename.
func (c *Config) Save(dir string) error {
	seed, err := c.Seed()
	if err != nil {
		return err
	}
	defer clear(seed)
	diskKeySeed, err := protectKeySeed(seed)
	if err != nil {
		return fmt.Errorf("config: protect keySeed: %w", err)
	}
	diskCfg := *c
	diskCfg.KeySeed = diskKeySeed

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	if err := RestrictDirACL(dir); err != nil {
		return fmt.Errorf("config: restrict dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(&diskCfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	tmp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: chmod %s: %w", tmpName, err)
	}
	if err := RestrictFileACL(tmpName); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: restrict %s: %w", tmpName, err)
	}
	_, writeErr := tmp.Write(append(data, '\n'))
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("config: write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename %s: %w", path, err)
	}
	cleanup = false
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: chmod %s: %w", path, err)
	}
	if err := RestrictFileACL(path); err != nil {
		return fmt.Errorf("config: restrict %s: %w", path, err)
	}
	return nil
}

// Seed decodes the stored 32-byte Ed25519 seed.
func (c *Config) Seed() ([]byte, error) {
	if c.KeySeed == "" {
		return nil, errors.New("config: empty keySeed")
	}
	seed, _, err := decodeKeySeed(c.KeySeed)
	if err != nil {
		return nil, err
	}
	return seed, nil
}

func decodeKeySeed(raw string) ([]byte, bool, error) {
	if strings.HasPrefix(raw, dpapiPrefix) {
		seed, err := unprotectKeySeed(raw)
		if err != nil {
			return nil, false, err
		}
		if len(seed) != keySeedBytes {
			clear(seed)
			return nil, false, fmt.Errorf("config: keySeed must decode to %d bytes, got %d", keySeedBytes, len(seed))
		}
		return seed, true, nil
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, false, fmt.Errorf("config: decode keySeed: %w", err)
	}
	if len(seed) != keySeedBytes {
		clear(seed)
		return nil, false, fmt.Errorf("config: keySeed must decode to %d bytes, got %d", keySeedBytes, len(seed))
	}
	return seed, false, nil
}
