package main

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func liveTestApp(t *testing.T) (*mirrpaneApp, *ui.Tester) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	a := newMirrPaneApp(t.TempDir(), &stillPicture{img}, true)
	t.Cleanup(a.shutdown)
	a.applySnapshot(Snapshot{Phase: PhaseLive, State: &ReceiverState{Status: "live"}})
	view := ui.NewTester(a.view, 900, 540)
	view.Frame()
	return a, view
}

// P2.6: one press plus auto-repeats must run a shortcut action once. The
// frames are genuine MyGo key events, so a repeat is the same contract the
// window sees.
func TestHeldShortcutRunsOnce(t *testing.T) {
	a, view := liveTestApp(t)

	view.KeyDown(ui.KeyR)
	view.KeyRepeat(0, ui.KeyR)
	view.KeyRepeat(0, ui.KeyR)
	view.KeyUp(ui.KeyR)

	if a.quarterTurns != 1 {
		t.Fatalf("a held shortcut fired %d times, want once", a.quarterTurns)
	}

	view.Key(0, ui.KeyH)
	if !a.prefs.FlipHorizontal {
		t.Fatal("a fresh press did not toggle the flip")
	}
}

// P2.8: a save while the sidecar is restarting must keep the settings dialog
// (and its draft) open and write nothing, instead of closing and discarding.
func TestSettingsSaveDuringRestartKeepsDraft(t *testing.T) {
	dir := t.TempDir()
	a := newMirrPaneApp(dir, nil, false)
	defer a.shutdown()

	a.openSettings()
	a.configDraft.maxFps = 30
	a.restarting = true
	a.saveConfigAndRestart()

	if !a.showSettings {
		t.Fatal("a rejected save closed the settings dialog and dropped the draft")
	}
	if !strings.Contains(a.pendingToast, "重新啟動") {
		t.Fatalf("rejected save showed no explanation: %q", a.pendingToast)
	}
	if _, err := os.Stat(ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("rejected save wrote config.json anyway: %v", err)
	}
}

// P2.8, accepted path: once no restart owns the sidecar, the same save closes
// the dialog and persists the edit.
func TestSettingsSaveAcceptedWritesConfig(t *testing.T) {
	dir := t.TempDir()
	a := newMirrPaneApp(dir, nil, false)
	defer a.shutdown()

	a.openSettings()
	a.configDraft.maxFps = 30
	a.saveConfigAndRestart()
	if a.showSettings {
		t.Fatal("accepted save left the settings dialog open")
	}
	waitFor(t, "config.json written", func() bool {
		_, err := os.Stat(ConfigPath(dir))
		return err == nil
	})
	data, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"maxFps": 30`) {
		t.Fatalf("saved config lost the edit: %s", data)
	}
}

// P2.16: a download that keeps reporting progress must not trip the idle
// deadline, no matter how long ago the launch began.
func TestProvisioningProgressExtendsStartupDeadline(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 1
	s.launchAt = time.Now().Add(-12 * time.Minute)
	s.lastProgress = time.Now().Add(-30 * time.Second)
	s.runtimeSetup = true
	s.mu.Unlock()

	s.handlePollError(1)
	if col.count() != 0 {
		t.Fatalf("a progressing download was reported dead after 12 minutes: %v", col.list)
	}

	// A progress line is a sign of life and refreshes the idle deadline.
	s.mu.Lock()
	s.lastProgress = time.Now().Add(-12 * time.Minute)
	s.mu.Unlock()
	s.handleStartupLine(1, `{"event":"ffmpeg_setup","message":"下載 FFmpeg 60%","done":false}`)
	s.mu.Lock()
	fresh := time.Since(s.lastProgress)
	s.mu.Unlock()
	if fresh > time.Second {
		t.Fatalf("progress did not refresh lastProgress (age %v)", fresh)
	}
	before := col.count() // the progress line itself is a booting snapshot
	s.handlePollError(1)
	if col.count() != before {
		t.Fatalf("fresh progress did not extend the deadline: %v", col.list)
	}

	// With no progress for longer than the idle deadline it fails, once.
	s.mu.Lock()
	s.lastProgress = time.Now().Add(-startupIdleDeadline - time.Minute)
	s.mu.Unlock()
	before = col.count()
	s.handlePollError(1)
	s.handlePollError(1)
	if col.count() != before+1 {
		t.Fatalf("idle startup reported %d failures, want 1", col.count()-before)
	}
	if last, _ := col.last(); !strings.Contains(last.Error, "沒有回應") {
		t.Fatalf("unexpected idle failure: %+v", last)
	}
}

// P2.16: the UI's whole-startup cap matches the backend's 30-minute
// provisioning cap, even while progress keeps arriving.
func TestStartupTotalDeadlineMatchesBackend(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 1
	s.launchAt = time.Now().Add(-startupTotalDeadline - time.Minute)
	s.lastProgress = time.Now()
	s.runtimeSetup = true
	s.mu.Unlock()

	s.handlePollError(1)
	if col.count() != 1 {
		t.Fatalf("setup past the backend cap emitted %d failures", col.count())
	}
	if last, _ := col.last(); !strings.Contains(last.Error, "30 分鐘") {
		t.Fatalf("total startup timeout has a misleading message: %+v", last)
	}
	if startupTotalDeadline != 30*time.Minute {
		t.Fatalf("startup cap %v does not match the backend's 30 minutes", startupTotalDeadline)
	}
}

// A duplicate data-directory instance check reports itself before the API
// exists; the UI must show that message (not a generic timeout) and an exit
// that follows must not replace it.
func TestDuplicateInstanceFailureSurfacesOnce(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 2
	s.mu.Unlock()

	const msg = "此資料夾正由另一個 MirrPane 接收器使用，請關閉該視窗後再試"
	s.handleStartupLine(2, `{"event":"ffmpeg_setup","message":"`+msg+`","done":true,"failed":true}`)
	if col.count() != 1 {
		t.Fatalf("duplicate-instance failure emitted %d snapshots", col.count())
	}
	last, _ := col.last()
	if last.Phase != PhaseFailed || last.Error != msg {
		t.Fatalf("duplicate-instance message was not surfaced: %+v", last)
	}

	// The process then exits; the failure must stay as it was.
	s.handleExit(2, nil)
	if col.count() != 1 {
		t.Fatalf("exit after the lock failure replaced it: %v", col.list)
	}

	// A stray progress line from the dying process must not flip the failure
	// screen back to booting.
	s3col := &snapshots{}
	s3 := NewSidecar(t.TempDir(), s3col.add)
	s3.mu.Lock()
	s3.gen = 4
	s3.mu.Unlock()
	s3.handleStartupLine(4, `{"event":"ffmpeg_setup","message":"此資料夾已被使用","failed":true}`)
	s3.handleStartupLine(4, `{"event":"ffmpeg_setup","message":"清理中"}`)
	if s3col.count() != 1 {
		t.Fatalf("progress after failure replaced the failure: %v", s3col.list)
	}

	// A failure line without text still names the problem on the failed screen.
	col2 := &snapshots{}
	s2 := NewSidecar(t.TempDir(), col2.add)
	s2.mu.Lock()
	s2.gen = 3
	s2.mu.Unlock()
	s2.handleStartupLine(3, `{"event":"ffmpeg_setup","failed":true}`)
	last2, _ := col2.last()
	if col2.count() != 1 || last2.Error != "接收器無法啟動" {
		t.Fatalf("silent startup failure was not reported: %+v (%d)", last2, col2.count())
	}
}

func TestFailedScreenShowsReceiverError(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), nil, true)
	defer a.shutdown()
	const msg = "此資料夾正由另一個 MirrPane 接收器使用"
	a.applySnapshot(Snapshot{Phase: PhaseFailed, Error: msg})
	view := ui.NewTester(a.view, 820, 520)
	view.Frame()
	if !view.HasText(msg) || !view.HasText("接收器無法啟動") {
		t.Fatalf("failed screen hid the reason: %v", view.Texts())
	}
}

// P2.18: a failed disconnect reports itself and leaves the window untouched;
// an acknowledged one exits fullscreen as the old UI did.
func TestDisconnectReportsFailureAndHandlesFullscreen(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), nil, false)
	defer a.shutdown()
	a.fullscreen = true

	if ok := a.applyDisconnect(os.ErrDeadlineExceeded); ok {
		t.Fatal("applyDisconnect reported success for a failed call")
	}
	if !a.fullscreen {
		t.Fatal("a failed disconnect left fullscreen")
	}
	if !strings.Contains(a.pendingToast, "中斷連線失敗") {
		t.Fatalf("failed disconnect showed no message: %q", a.pendingToast)
	}

	a.pendingToast = ""
	if ok := a.applyDisconnect(nil); !ok {
		t.Fatal("applyDisconnect rejected a successful call")
	}
	if a.fullscreen {
		t.Fatal("a successful disconnect stayed fullscreen")
	}
	if !strings.Contains(a.pendingToast, "已中斷連線") {
		t.Fatalf("successful disconnect showed no message: %q", a.pendingToast)
	}
}

// P3.4: a fresh portable install has no screenshots folder; opening it must
// create it first and report a failure instead of handing Explorer a missing
// path.
func TestScreenshotFolderIsCreatedBeforeOpening(t *testing.T) {
	dir := t.TempDir()
	a := &mirrpaneApp{installDir: dir}
	if err := a.ensureScreenshotsDir(); err != nil {
		t.Fatalf("ensureScreenshotsDir: %v", err)
	}
	if info, err := os.Stat(a.screenshotsDir()); err != nil || !info.IsDir() {
		t.Fatalf("screenshots folder was not created: %v", err)
	}

	// A file where the folder belongs is reported, not passed to Explorer.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "screenshots"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a2 := &mirrpaneApp{installDir: blocked}
	a2.openScreenshots()
	if !strings.Contains(a2.pendingToast, "無法建立截圖資料夾") {
		t.Fatalf("uncreatable screenshots folder went unreported: %q", a2.pendingToast)
	}
}

// P3.5: holding the pointer over the toolbar pauses auto-hide; leaving it
// hides the bar again. Both transitions use MyGo pointer events.
func TestToolbarHoverPausesAutoHide(t *testing.T) {
	a, view := liveTestApp(t)
	a.prefs.BarHideSeconds = 2
	a.barVisible = true
	a.barLastMove = time.Now()

	shots, ok := view.AccessByRole(ui.RoleButton, "截圖 (Ctrl+S)")
	if !ok {
		t.Fatalf("toolbar is missing after a live frame: %v", view.Texts())
	}
	view.Move(shots.Bounds.X+shots.Bounds.W/2, shots.Bounds.Y+shots.Bounds.H/2)
	if !a.barHovered {
		t.Fatal("pointer over the toolbar did not stop the hide timer")
	}
	a.barLastMove = time.Now().Add(-3 * time.Second)
	view.Frame()
	if !a.barVisible {
		t.Fatal("the toolbar disappeared under the pointer")
	}

	view.Move(450, 500)
	if a.barHovered {
		t.Fatal("pointer away from the toolbar kept the hover flag")
	}
	a.barLastMove = time.Now().Add(-3 * time.Second)
	view.Frame()
	if a.barVisible {
		t.Fatal("the toolbar did not hide after the pointer left")
	}
}

// The interrupted card carries every action the toolbar would offer, so the
// toolbar stays hidden behind its backdrop; the HUD takes the top corner.
func TestInterruptedBarAndHudShareVisibility(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), &stillPicture{image.NewRGBA(image.Rect(0, 0, 64, 36))}, true)
	defer a.shutdown()
	a.prefs.ShowHud = true
	a.prefs.BarHideSeconds = 2
	a.prefs.HudCorner = "topLeft"
	a.barVisible = false
	a.applySnapshot(Snapshot{Phase: PhaseInterrupted, State: &ReceiverState{
		Status: "interrupted", Width: 2560, Height: 1178, Decoder: "synthetic", Reason: "訊號中斷",
	}})
	view := ui.NewTester(a.view, 900, 540)
	view.Frame()

	if !view.HasText("畫面暫時中斷") || !view.HasText("返回等待畫面") {
		t.Fatalf("interruption card missing its actions: %v", view.Texts())
	}
	if view.HasText("AirPlay") {
		t.Fatal("the toolbar is still drawn behind the interruption backdrop")
	}
	hud, ok := view.Find("效能 HUD")
	if !ok {
		t.Fatalf("HUD card missing: %v", view.Texts())
	}
	if hud.Y > 60 {
		t.Fatalf("HUD did not move to the top with the bar hidden: y=%v", hud.Y)
	}
}

// P3.7: the stored legacy vocabulary still loads, but the settings offer the
// two modes the renderer can actually produce.
func TestSamplingModesAreMeaningfulAndReadLegacyValues(t *testing.T) {
	if !sharpSampling("none") || sharpSampling("low") || sharpSampling("medium") || sharpSampling("high") {
		t.Fatal("sharpSampling disagrees with the smooth renderer")
	}
	if samplingChoice("none") != samplingChoices[0] {
		t.Fatal("legacy 'none' is not shown as sharp")
	}
	for _, legacy := range []string{"low", "medium", "high"} {
		if samplingChoice(legacy) != samplingChoices[1] {
			t.Fatalf("legacy %q is not shown as smooth", legacy)
		}
	}
	if filterQualityForChoice(samplingChoices[0]) != "none" ||
		filterQualityForChoice(samplingChoices[1]) != "medium" {
		t.Fatal("settings choices do not round-trip to stored values")
	}
	if legacy := (UIPrefs{FilterQuality: "high"}).Sanitize(); legacy.FilterQuality != "high" {
		t.Fatalf("a legacy stored value no longer loads: %+v", legacy)
	}

	a := newMirrPaneApp(t.TempDir(), nil, true)
	defer a.shutdown()
	a.prefs.FilterQuality = "none"
	a.applySnapshot(Snapshot{Phase: PhaseWaiting})
	a.openSettings()
	a.settingsTab = 2 // the picture tab holds the sampling choice
	view := ui.NewTester(a.view, 640, 420)
	view.Frame()
	if !view.HasText("縮放取樣") || !view.HasText(samplingChoices[0]) {
		t.Fatalf("settings do not show the sharp choice: %v", view.Texts())
	}

	a.prefs.FilterQuality = "high"
	view.Frame()
	if !view.HasText(samplingChoices[1]) {
		t.Fatalf("legacy 'high' is not shown as smooth: %v", view.Texts())
	}
}

// Audio-only support: a live state with the "audio" reason names the sender
// and says there is no picture instead of looking like a broken stream.
func TestAudioOnlyLiveStateNamesSender(t *testing.T) {
	a, view := liveTestApp(t)
	if w, h := a.bitmap.Size(); w == 1 && h == 1 {
		t.Fatal("test needs a presented picture before the audio session")
	}
	a.applySnapshot(Snapshot{Phase: PhaseLive, State: &ReceiverState{
		Status: "live", Reason: "audio", Sender: "iPhone 客廳",
	}})
	view.Frame()
	if !view.HasText("僅音訊") || !view.HasText("來源裝置：iPhone 客廳") {
		t.Fatalf("audio-only session is not named: %v", view.Texts())
	}
	if !view.HasText("iPhone 客廳") {
		t.Fatal("sender missing from the live status bar")
	}
	if w, h := a.bitmap.Size(); w != 1 || h != 1 {
		t.Fatalf("audio-only session kept the previous picture: %dx%d", w, h)
	}
}

// Parity: double-clicking the picture toggles fullscreen, while a double click
// on a toolbar control does not.
func TestDoubleClickVideoTogglesFullscreen(t *testing.T) {
	a, view := liveTestApp(t)
	a.fullscreen = false

	view.Move(450, 300)
	view.ClickAt(450, 300)
	view.ClickAt(450, 300)
	if !a.fullscreen {
		t.Fatal("double-clicking the picture did not enter fullscreen")
	}

	a.fullscreen = false
	display, ok := view.AccessByRole(ui.RoleButton, "畫面調整")
	if !ok {
		t.Fatalf("toolbar control missing: %v", view.Texts())
	}
	x, y := display.Bounds.X+display.Bounds.W/2, display.Bounds.Y+display.Bounds.H/2
	view.Move(x, y)
	view.ClickAt(x, y)
	view.ClickAt(x, y)
	if a.fullscreen {
		t.Fatal("a double click on the toolbar entered fullscreen")
	}
}

// Parity: the waiting screen offers a rebroadcast and the screenshots folder,
// and the log window shows the path plus copy actions.
func TestRestoredWaitingAndLogParity(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), nil, true)
	defer a.shutdown()
	a.applySnapshot(Snapshot{Phase: PhaseWaiting})
	view := ui.NewTester(a.view, 900, 620)
	view.Frame()
	if !view.HasText("重新廣播") || !view.HasText("截圖資料夾") {
		t.Fatalf("waiting screen is missing the restored controls: %v", view.Texts())
	}

	a.openLogs()
	view.Frame()
	if !view.HasText(a.sidecar.LogPath()) || !view.HasText("複製全部") {
		t.Fatalf("log window is missing the path or copy actions: %v", view.Texts())
	}
}
