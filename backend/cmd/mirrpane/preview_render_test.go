package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestRenderUIPreviews draws the app's screens to PNG files without opening a
// window, for design review. Set MIRRPANE_UI_PREVIEW to an output directory
// to run it; ordinary test runs skip it.
func TestRenderUIPreviews(t *testing.T) {
	dir := os.Getenv("MIRRPANE_UI_PREVIEW")
	if dir == "" {
		t.Skip("set MIRRPANE_UI_PREVIEW=<dir> to render UI previews")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	liveState := func() ReceiverState {
		st := defaultReceiverState()
		st.Status = "live"
		st.Name = "鏡屏"
		st.IP = "192.168.1.42"
		st.Port = 7000
		st.Sender = "Miku 的 iPhone"
		st.Width, st.Height = 2560, 1178
		st.TargetWidth, st.TargetHeight, st.MaxFPS = 2560, 1440, 60
		st.Decoder = "D3D11VA"
		st.DecoderMode = "auto"
		st.Version = "220.68"
		st.FPS = 59.8
		st.DecodeAvgMs, st.DecodeP95Ms = 2.4, 4.1
		st.LagFrames, st.Dropped = 1, 0
		st.AudioOccupancy = 38.5
		st.Pictures = 12873
		st.AudioFrames = 170284
		return st
	}
	waitingState := func() ReceiverState {
		st := defaultReceiverState()
		st.Status = "idle"
		st.Name = "鏡屏"
		st.IP = "192.168.1.42"
		st.Port = 7000
		st.TargetWidth, st.TargetHeight, st.MaxFPS = 2560, 1440, 60
		st.Decoder = "D3D11VA"
		st.Version = "220.68"
		return st
	}

	render := func(name string, w, h int, setup func(a *mirrpaneApp)) {
		t.Helper()
		a := newMirrPaneApp(t.TempDir(), NewSyntheticFrameSource(1920, 1080), true)
		a.demo = true
		a.barVisible = true
		setup(a)
		savePreview(t, a, name, w, h)
	}
	// renderBlank keeps the video surface empty, so overlays are visible in
	// the headless compositor.
	renderBlank := func(name string, w, h int, setup func(a *mirrpaneApp)) {
		t.Helper()
		a := newMirrPaneApp(t.TempDir(), &stillPicture{}, true)
		a.demo = true
		a.barVisible = true
		setup(a)
		savePreview(t, a, name, w, h)
	}

	render("01-waiting", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
	})
	render("02-waiting-pending", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		st.PendingSender = &PendingSender{Label: "Miku 的 iPhone", Remote: "192.168.1.77",
			Fingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", AgeSeconds: 14}
		a.snap = Snapshot{Phase: PhaseConnecting, State: &st}
	})
	render("03-booting", 1280, 800, func(a *mirrpaneApp) {
		a.snap = Snapshot{Phase: PhaseBooting, Message: "正在初始化 FFmpeg 執行環境…"}
	})
	render("04-connecting", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		st.Reason = "配對與金鑰交換中"
		a.snap = Snapshot{Phase: PhaseConnecting, State: &st}
	})
	render("05-live", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
	})
	render("06-live-audio-only", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		st.Reason = "audio"
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
	})
	render("07-interrupted", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		st.Reason = "等待傳送端恢復串流或重新連線…"
		a.snap = Snapshot{Phase: PhaseInterrupted, State: &st}
	})
	render("08-failed", 1280, 800, func(a *mirrpaneApp) {
		a.snap = Snapshot{Phase: PhaseFailed, Error: "接收器無法啟動：連接埠 7000 已被其他程式使用"}
	})
	render("09-settings", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
		a.openSettings()
	})
	render("10-logs", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
		a.showLogs = true
	})
	render("11-hud", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		st.Reason = "audio"
		a.prefs.ShowHud = true
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
	})
	render("12-settings-graphics", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
		a.openSettings()
		a.settingsTab = 1
	})
	render("13-settings-picture", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
		a.openSettings()
		a.settingsTab = 2
	})
	render("14-android-modal", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
		a.showAndroid = true
		a.androidDevices = []AndroidDevice{
			{Serial: "R58M12345", State: "device", Model: "Pixel 9"},
			{Serial: "192.168.1.50:5555", State: "unauthorized", Model: "Galaxy S25"},
			{Serial: "emulator-5554", State: "offline", Model: "Pixel_7_API_34"},
		}
	})
	renderBlank("15-android-live", 1280, 800, func(a *mirrpaneApp) {
		st := liveState()
		st.Source = "android"
		st.Sender = "Pixel 9"
		st.Decoder = "ADB"
		st.Android = &AndroidState{
			Phase: "live", Serial: "R58M12345", Device: "Pixel 9", Width: 1920, Height: 1080,
		}
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
	})
	render("16-android-error", 1280, 800, func(a *mirrpaneApp) {
		st := waitingState()
		a.snap = Snapshot{Phase: PhaseWaiting, State: &st}
		a.showAndroid = true
		a.androidErr = "找不到 adb.exe：請安裝 Android Platform Tools"
	})
}

// savePreview renders one screen to <dir>/<name>.png.
func savePreview(t *testing.T, a *mirrpaneApp, name string, w, h int) {
	t.Helper()
	img := ui.Render(a.view, w, h, 1)
	if img == nil {
		t.Fatalf("%s: Render returned nil", name)
	}
	dir := os.Getenv("MIRRPANE_UI_PREVIEW")
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	a.shutdown()
}
