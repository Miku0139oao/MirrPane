package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUIPrefsDefaultsClampsAndPersistence(t *testing.T) {
	dir := t.TempDir()

	// Missing file returns defaults.
	got := LoadUIPrefs(dir)
	def := DefaultUIPrefs()
	if got != def {
		t.Fatalf("LoadUIPrefs(missing) = %+v, want %+v", got, def)
	}

	// Older preference files can contain out-of-range values or unknown enum names.
	raw := `{
  "filterQuality": "ultra",
  "brightness": 0.9,
  "contrast": 0.1,
  "saturation": 3.0,
  "flipHorizontal": true,
  "fillViewport": true,
  "showHud": true,
  "hudCorner": "middle",
  "barHideSeconds": 7,
  "muted": true,
  "volume": -0.5
}`
	path := UIPrefsPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	clamped := LoadUIPrefs(dir)
	if clamped.FilterQuality != "medium" ||
		clamped.Brightness != 0.3 ||
		clamped.Contrast != 0.7 ||
		clamped.Saturation != 1.5 ||
		!clamped.FlipHorizontal ||
		!clamped.FillViewport ||
		!clamped.ShowHud ||
		clamped.HudCorner != "bottomLeft" ||
		clamped.BarHideSeconds != 2 ||
		!clamped.Muted ||
		clamped.Volume != 0.0 {
		t.Fatalf("unexpected clamped prefs: %+v", clamped)
	}

	clamped.HudCorner = "topRight"
	clamped.BarHideSeconds = 4
	clamped.Volume = 0.4567
	if err := clamped.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := LoadUIPrefs(dir)
	if reloaded.HudCorner != "topRight" || reloaded.BarHideSeconds != 4 || reloaded.Volume != 0.457 {
		t.Fatalf("reloaded = %+v", reloaded)
	}
}

func TestConfigPatchAndResetIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := SaveConfigPatch(dir, map[string]any{"name": "客廳", "maxFps": 60}); err != nil {
		t.Fatalf("SaveConfigPatch initial: %v", err)
	}
	if err := SaveReceiverName(dir, "  書房  "); err != nil {
		t.Fatalf("SaveReceiverName: %v", err)
	}

	data, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "書房" {
		t.Fatalf("name = %v, want 書房", m["name"])
	}
	if m["maxFps"] != float64(60) {
		t.Fatalf("maxFps = %v, want 60", m["maxFps"])
	}

	if err := ResetIdentity(dir); err != nil {
		t.Fatalf("ResetIdentity: %v", err)
	}
	if _, err := os.Stat(ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected config.json removed, err=%v", err)
	}
}

func TestSidecarStdoutParsingAndAPIHelpers(t *testing.T) {
	var mu sync.Mutex
	var snaps []Snapshot
	s := NewSidecar(t.TempDir(), func(sn Snapshot) {
		mu.Lock()
		snaps = append(snaps, sn)
		mu.Unlock()
	})

	s.handleStdoutLine(`not json`)
	s.handleStdoutLine(`{"event":"ffmpeg_setup","message":"下載中","done":false,"failed":false}`)
	s.handleStdoutLine(`{"event":"ffmpeg_setup","message":"初始化完成","done":true,"failed":false}`)

	mu.Lock()
	if len(snaps) != 2 || snaps[0].Phase != PhaseBooting || snaps[1].Message != "初始化完成" {
		t.Fatalf("unexpected startup snapshots: %+v", snaps)
	}
	mu.Unlock()

	// Spin up mock sidecar HTTP server.
	var lastAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/state":
			_, _ = w.Write([]byte(`{"status":"idle","name":"測試機","ip":"192.168.1.10","port":7000,"targetWidth":1920,"targetHeight":1080,"maxFps":60,"decoderMode":"auto","version":"220.68"}`))
		case "/api/disconnect":
			lastAction = "disconnect"
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/audio":
			lastAction = "audio"
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/snapshot":
			lastAction = "snapshot"
			_, _ = w.Write([]byte(`{"path":"C:\\shots\\MirrPane-1.png"}`))
		case "/api/pairing/trust/approve":
			lastAction = "approve"
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/pairing/trust/reject":
			lastAction = "reject"
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/pairing/trust/reset":
			lastAction = "reset"
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s.mu.Lock()
	s.baseURL = srv.URL
	s.mu.Unlock()

	s.tick(t.Context())
	mu.Lock()
	last := snaps[len(snaps)-1]
	mu.Unlock()
	if last.Phase != PhaseWaiting || last.State == nil || last.State.Name != "測試機" {
		t.Fatalf("unexpected polled snapshot: %+v", last)
	}

	muted := true
	vol := 0.5
	if err := s.SetAudio(&muted, &vol); err != nil || lastAction != "audio" {
		t.Fatalf("SetAudio err=%v action=%s", err, lastAction)
	}
	if path, err := s.TakeSnapshot(); err != nil || !strings.HasSuffix(path, "MirrPane-1.png") || lastAction != "snapshot" {
		t.Fatalf("TakeSnapshot path=%q err=%v", path, err)
	}
	if err := s.ApproveSender("abc"); err != nil || lastAction != "approve" {
		t.Fatalf("ApproveSender err=%v", err)
	}
	if err := s.RejectSender("abc"); err != nil || lastAction != "reject" {
		t.Fatalf("RejectSender err=%v", err)
	}
	if err := s.ResetTrustedSenders(); err != nil || lastAction != "reset" {
		t.Fatalf("ResetTrustedSenders err=%v", err)
	}
	if err := s.Disconnect(); err != nil || lastAction != "disconnect" {
		t.Fatalf("Disconnect err=%v", err)
	}
}

func TestSyntheticFrameSourceAndLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "airplayd.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("l1\r\nl2\r\nl3\r\nl4\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSidecar(dir, nil)
	tail := s.LogTail(2)
	if len(tail) != 2 || tail[0] != "l3" || tail[1] != "l4" {
		t.Fatalf("LogTail = %v", tail)
	}

	src := NewSyntheticFrameSource(64, 32)
	img1, ok := src.Latest()
	if !ok || img1 == nil || img1.Bounds().Dx() != 64 || img1.Bounds().Dy() != 32 {
		t.Fatalf("first Latest failed: ok=%v img=%v", ok, img1)
	}
	time.Sleep(18 * time.Millisecond)
	img2, ok := src.Latest()
	if !ok || img2 == nil || img1 == img2 {
		t.Fatalf("expected double-buffered second frame, ok=%v same=%v", ok, img1 == img2)
	}
}
