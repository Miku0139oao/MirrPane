package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miku0139/airplay-go/backend/internal/config"
)

// UIPrefs mirrors frontend/lib/ui_prefs.dart (config/ui_prefs.json) with
// identical JSON keys, defaults, and clamp ranges.
type UIPrefs struct {
	FilterQuality  string  `json:"filterQuality"`
	Brightness     float64 `json:"brightness"`
	Contrast       float64 `json:"contrast"`
	Saturation     float64 `json:"saturation"`
	FlipHorizontal bool    `json:"flipHorizontal"`
	FillViewport   bool    `json:"fillViewport"`
	ShowHud        bool    `json:"showHud"`
	HudCorner      string  `json:"hudCorner"`
	BarHideSeconds int     `json:"barHideSeconds"`
	Muted          bool    `json:"muted"`
	Volume         float64 `json:"volume"`
}

var (
	allowedFilterQualities = []string{"none", "low", "medium", "high"}
	allowedHudCorners      = []string{"topLeft", "topRight", "bottomLeft", "bottomRight"}
	allowedBarHideSeconds  = []int{0, 2, 4}
)

// DefaultUIPrefs returns the canonical default frontend preferences.
func DefaultUIPrefs() UIPrefs {
	return UIPrefs{
		FilterQuality:  "medium",
		Brightness:     0.0,
		Contrast:       1.0,
		Saturation:     1.0,
		FlipHorizontal: false,
		FillViewport:   false,
		ShowHud:        false,
		HudCorner:      "bottomLeft",
		BarHideSeconds: 2,
		Muted:          false,
		Volume:         1.0,
	}
}

// Sanitize clamps numeric values and replaces invalid enum fields with defaults.
func (p UIPrefs) Sanitize() UIPrefs {
	if !slices.Contains(allowedFilterQualities, p.FilterQuality) {
		p.FilterQuality = "medium"
	}
	if !slices.Contains(allowedHudCorners, p.HudCorner) {
		p.HudCorner = "bottomLeft"
	}
	if !slices.Contains(allowedBarHideSeconds, p.BarHideSeconds) {
		p.BarHideSeconds = 2
	}
	p.Brightness = round3(clampFloat(p.Brightness, -0.3, 0.3))
	p.Contrast = round3(clampFloat(p.Contrast, 0.7, 1.4))
	p.Saturation = round3(clampFloat(p.Saturation, 0.5, 1.5))
	p.Volume = round3(clampFloat(p.Volume, 0.0, 1.0))
	return p
}

// IsColorDefault reports whether brightness, contrast and saturation are at
// their neutral values, matching the old frontend's isDefaultColor.
func (p UIPrefs) IsColorDefault() bool {
	return math.Abs(p.Brightness) < 0.001 &&
		math.Abs(p.Contrast-1.0) < 0.001 &&
		math.Abs(p.Saturation-1.0) < 0.001
}

// UIPrefsPath returns <installDir>\config\ui_prefs.json.
func UIPrefsPath(installDir string) string {
	return filepath.Join(installDir, "config", "ui_prefs.json")
}

// LoadUIPrefs loads and clamps config/ui_prefs.json, falling back to defaults on
// missing or unreadable files.
func LoadUIPrefs(installDir string) UIPrefs {
	def := DefaultUIPrefs()
	data, err := os.ReadFile(UIPrefsPath(installDir))
	if err != nil {
		return def
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return def
	}
	return UIPrefsFromMap(raw)
}

// UIPrefsFromMap parses a decoded JSON object into a sanitized UIPrefs,
// matching frontend/lib/ui_prefs.dart field-for-field.
func UIPrefsFromMap(raw map[string]any) UIPrefs {
	p := DefaultUIPrefs()
	if raw == nil {
		return p
	}
	if v, ok := raw["filterQuality"].(string); ok && slices.Contains(allowedFilterQualities, v) {
		p.FilterQuality = v
	}
	if v, ok := asFloat(raw["brightness"]); ok {
		p.Brightness = clampFloat(v, -0.3, 0.3)
	}
	if v, ok := asFloat(raw["contrast"]); ok {
		p.Contrast = clampFloat(v, 0.7, 1.4)
	}
	if v, ok := asFloat(raw["saturation"]); ok {
		p.Saturation = clampFloat(v, 0.5, 1.5)
	}
	if v, ok := raw["flipHorizontal"].(bool); ok {
		p.FlipHorizontal = v
	}
	if v, ok := raw["fillViewport"].(bool); ok {
		p.FillViewport = v
	}
	if v, ok := raw["showHud"].(bool); ok {
		p.ShowHud = v
	}
	if v, ok := raw["hudCorner"].(string); ok && slices.Contains(allowedHudCorners, v) {
		p.HudCorner = v
	}
	if v, ok := asFloat(raw["barHideSeconds"]); ok {
		iv := int(v)
		if slices.Contains(allowedBarHideSeconds, iv) {
			p.BarHideSeconds = iv
		}
	}
	if v, ok := raw["muted"].(bool); ok {
		p.Muted = v
	}
	if v, ok := asFloat(raw["volume"]); ok {
		p.Volume = clampFloat(v, 0.0, 1.0)
	}
	return p.Sanitize()
}

// Save writes sanitized preferences to <installDir>\config\ui_prefs.json.
func (p UIPrefs) Save(installDir string) error {
	clean := p.Sanitize()
	path := UIPrefsPath(installDir)
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return config.WritePrivateFile(path, data)
}

// ConfigPath returns <installDir>\config\config.json.
func ConfigPath(installDir string) string {
	return filepath.Join(installDir, "config", "config.json")
}

// SaveConfigPatch merges patch keys into config/config.json, creating the file
// if it does not exist yet.
func SaveConfigPatch(installDir string, patch map[string]any) error {
	path := ConfigPath(installDir)
	owner, err := config.LockReceiver(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer owner.Close()
	dataMap := map[string]json.RawMessage{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &dataMap); err != nil {
			return fmt.Errorf("read receiver settings: %w", err)
		}
		if dataMap == nil {
			return errors.New("receiver settings must be a JSON object")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for k, v := range patch {
		value, err := json.Marshal(v)
		if err != nil {
			return err
		}
		dataMap[k] = value
	}
	encoded, err := json.MarshalIndent(dataMap, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return config.WritePrivateFile(path, encoded)
}

// SaveReceiverName updates the advertised receiver name in config/config.json.
func SaveReceiverName(installDir, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("receiver name cannot be empty")
	}
	return SaveConfigPatch(installDir, map[string]any{"name": trimmed})
}

// ResetIdentity removes config/config.json so the sidecar generates a fresh
// AirPlay identity on its next start.
func ResetIdentity(installDir string) error {
	path := ConfigPath(installDir)
	owner, err := config.LockReceiver(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer owner.Close()
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
