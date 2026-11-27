package main

import (
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"
)

type mirrpaneApp struct {
	installDir   string
	sidecar      *Sidecar
	frameSrc     FrameSource
	bitmap       *ui.Bitmap
	videoPicture image.Image // the frame currently held for display
	videoSharp   bool
	theme        *ui.Theme

	win *mygo.Window

	// demo renders a synthetic picture and skips the sidecar, for UI testing
	// without a phone attached (`-demo`).
	demo bool
	// demoAndroid presents the demo session as an Android (scrcpy) session, so
	// its toolbar and input states can be reviewed without a device.
	demoAndroid bool

	// Main-thread UI state (mutated inside view or win.Update / RunOnMain).
	snap         Snapshot
	prefs        UIPrefs
	alwaysOnTop  bool
	fullscreen   bool
	audioSynced  bool
	showSettings bool
	showLogs     bool
	logLines     []string
	uiFPS        float64
	fpsStart     time.Time
	fpsFrames    int
	restarting   bool

	// Live-view presentation state.
	quarterTurns int
	barVisible   bool
	barLastMove  time.Time
	// barHovered pauses auto-hide while the pointer is over the top bar, so a
	// button cannot disappear under the cursor before it is clicked;
	// barFocused does the same while one of its controls has the keyboard.
	barHovered bool
	barFocused bool

	// configDraft collects the restart-required settings from the settings UI.
	configDraft   configDraft
	initialConfig configDraft
	settingsTab   int

	// Popover menus of the live bar and idle chrome.
	displayMenuOpen bool
	moreMenuOpen    bool

	// Android (scrcpy) session UI. The receiver owns the adb session; these
	// fields only mirror its phase and the device list dialog.
	showAndroid     bool
	androidDevices  []AndroidDevice
	androidErr      string
	androidLoading  bool
	androidAddress  string
	androidPairCode string
	inputCh         chan AndroidInput
	androidPointer  bool

	// Pending async feedback to show as toasts on the next build pass.
	pendingToast       string
	pendingToastAction *toastAction

	prefChanges chan prefChange
	workers     sync.WaitGroup
	closed      atomic.Bool
}

type prefChange struct {
	prefs UIPrefs
	save  bool
	audio bool
}

// configDraft holds the config.json values edited in the settings modal, which
// take effect only after the sidecar restarts.
type configDraft struct {
	name            string
	resKey          string
	maxFps          int
	decoderMode     string
	colorRange      string
	scaleFilter     string
	audioBuffer     string
	requireApproval bool
}

// toastAction is a queued toast with one action button that opens a folder.
type toastAction struct {
	msg    string
	label  string
	folder string
}

// resolutionPresets lists the display sizes offered in settings.
var resolutionPresets = map[string][2]int{
	"1280x720":  {1280, 720},
	"1920x1080": {1920, 1080},
	"2560x1440": {2560, 1440},
	"3840x2160": {3840, 2160},
}

var resolutionOrder = []string{"1280x720", "1920x1080", "2560x1440", "3840x2160"}

func newMirrPaneApp(installDir string, frameSrc FrameSource, demo bool) *mirrpaneApp {
	prefs := LoadUIPrefs(installDir)
	a := &mirrpaneApp{
		installDir:  installDir,
		frameSrc:    frameSrc,
		bitmap:      ui.NewBitmap(opaqueBlank()),
		theme:       mirrpaneTheme(),
		prefs:       prefs,
		demo:        demo,
		snap:        Snapshot{Phase: PhaseBooting, Message: "正在啟動接收器…"},
		prefChanges: make(chan prefChange, 1),
	}
	a.sidecar = NewSidecar(installDir, a.onSidecarSnapshot)
	a.inputCh = make(chan AndroidInput, 128)
	a.workers.Add(2)
	go a.writePreferences()
	go a.writeAndroidInput()
	return a
}

// writeAndroidInput drains forwarded input onto the receiver's API. A full
// queue drops events; pointer moves may be dropped, presses never block the
// UI thread.
func (a *mirrpaneApp) writeAndroidInput() {
	defer a.workers.Done()
	for in := range a.inputCh {
		if err := a.sidecar.AndroidInput(in); err != nil && a.closed.Load() == false {
			// Input errors are transient (session ended); the polled state is
			// the source of truth, so they do not become toasts per event.
			continue
		}
	}
}

// forwardInput queues one input message for the Android session.
func (a *mirrpaneApp) forwardInput(in AndroidInput) {
	if a.closed.Load() || a.demo {
		return
	}
	select {
	case a.inputCh <- in:
	default:
	}
}

func (a *mirrpaneApp) attachWindow(win *mygo.Window) {
	a.win = win

	win.OnEnterFullScreen(func() {
		a.fullscreen = true
		win.Invalidate()
	})
	win.OnLeaveFullScreen(func() {
		a.fullscreen = false
		win.Invalidate()
	})
	win.OnClose(func(*mygo.CloseEvent) {
		a.shutdown()
	})

	a.barVisible = true
	a.barLastMove = time.Now()

	if a.demo {
		// No sidecar: present a synthetic live session so the video surface and
		// its overlays can be exercised without a phone.
		st := defaultReceiverState()
		st.Status = "live"
		st.Name = "鏡屏 (Demo 預覽)"
		st.Sender = "Demo 訊號源"
		st.Width, st.Height = 2560, 1178
		st.Decoder = "synthetic"
		if a.demoAndroid {
			st.Source = "android"
			st.Sender = "Pixel 9 (Demo)"
			st.Width, st.Height = 1920, 1080
			st.Decoder = "ADB"
			st.Android = &AndroidState{
				Phase: "live", Serial: "DEMO-ADB", Device: "Pixel 9 (Demo)",
				Width: 1920, Height: 1080,
			}
		}
		a.snap = Snapshot{Phase: PhaseLive, State: &st}
	} else {
		a.sidecar.Start()
	}
}

func (a *mirrpaneApp) shutdown() {
	if a.closed.Swap(true) {
		return
	}
	close(a.prefChanges)
	close(a.inputCh)
	a.workers.Wait()
	a.sidecar.Close()
	a.clearVideoPicture()
	if a.frameSrc != nil {
		_ = a.frameSrc.Close()
	}
}

func (a *mirrpaneApp) onSidecarSnapshot(snap Snapshot) {
	win := a.win
	if win == nil || win.IsDestroyed() {
		return
	}
	win.Update(func() { a.applySnapshot(snap) })
}

func (a *mirrpaneApp) applySnapshot(snap Snapshot) {
	if a.closed.Load() {
		return
	}
	if snap.Phase == PhaseLive && a.snap.Phase != PhaseLive {
		a.barVisible = true
		a.barLastMove = time.Now()
		a.fpsStart, a.fpsFrames, a.uiFPS = time.Now(), 0, 0
	}
	if a.streaming() && snap.Phase != PhaseLive && snap.Phase != PhaseInterrupted {
		// Stop showing the prior sender when another session starts.
		a.clearVideoPicture()
	}
	if audioOnly(snap.State) && !audioOnly(a.snap.State) {
		// An audio-only session carries no picture; do not leave the previous
		// session's last frame behind its "僅音訊" card.
		a.clearVideoPicture()
	}
	prevAndroid := a.androidState()
	a.snap = snap
	if snap.State != nil && snap.State.Android != nil {
		switch snap.State.Android.Phase {
		case "live":
			a.showAndroid = false
		case "error":
			if prevAndroid != nil && prevAndroid.Phase == "connecting" {
				a.pendingToast = "Android 連線失敗：" + snap.State.Android.Error
			}
		}
	}
	if snap.Phase == PhaseBooting || snap.Phase == PhaseFailed {
		a.audioSynced = false
	}
	if snap.Phase == PhaseFailed {
		a.logLines = a.sidecar.LogTail(24)
	}
	if !a.audioSynced && snap.Phase != PhaseBooting && snap.Phase != PhaseFailed {
		a.audioSynced = true
		a.queuePrefs(prefChange{prefs: a.prefs, audio: true})
	}
}

// presentFrame runs on the renderer's thread at display refresh. The window
// stops issuing animation frames when occluded, letting the producer idle.
func (a *mirrpaneApp) presentFrame(c *ui.Context) {
	c.AnimationFrame()
	now := time.Now()
	if a.fpsStart.IsZero() {
		a.fpsStart = now
	}
	if img, ok := a.frameSrc.Latest(); ok && img != nil {
		a.presentVideoFrame(img)
		a.fpsFrames++
	}
	if elapsed := now.Sub(a.fpsStart); elapsed >= time.Second {
		a.uiFPS = float64(a.fpsFrames) / elapsed.Seconds()
		a.fpsFrames, a.fpsStart = 0, now
	}
	if a.prefs.BarHideSeconds > 0 && !a.barHovered && !a.barFocused && !a.displayMenuOpen && !a.moreMenuOpen &&
		now.Sub(a.barLastMove) >= time.Duration(a.prefs.BarHideSeconds)*time.Second {
		a.barVisible = false
	}
}

// Only the UI thread enqueues changes. Coalesce slider events while keeping
// any pending disk write or audio update, so the final settings win.
func (a *mirrpaneApp) queuePrefs(change prefChange) {
	if a.closed.Load() {
		return
	}
	select {
	case pending := <-a.prefChanges:
		change.save = change.save || pending.save
		change.audio = change.audio || pending.audio
	default:
	}
	a.prefChanges <- change
}

func (a *mirrpaneApp) writePreferences() {
	defer a.workers.Done()
	for change := range a.prefChanges {
		if change.save {
			if err := change.prefs.Save(a.installDir); err != nil {
				a.toastAsync("儲存偏好失敗：" + err.Error())
			}
		}
		if change.audio && !a.demo && !a.closed.Load() {
			if err := a.sidecar.SetAudio(&change.prefs.Muted, &change.prefs.Volume); err != nil {
				a.toastAsync("套用音量失敗：" + err.Error())
			}
		}
	}
}

func (a *mirrpaneApp) savePrefs(next UIPrefs) {
	next = next.Sanitize()
	if next == a.prefs {
		return
	}
	audio := next.Muted != a.prefs.Muted || next.Volume != a.prefs.Volume
	a.prefs = next
	a.queuePrefs(prefChange{prefs: next, save: true, audio: audio})
}

func (a *mirrpaneApp) toggleAlwaysOnTop() {
	if a.win == nil {
		return
	}
	a.alwaysOnTop = !a.alwaysOnTop
	a.win.SetAlwaysOnTop(a.alwaysOnTop)
}

func (a *mirrpaneApp) setFullScreen(v bool) {
	a.fullscreen = v
	if a.win == nil {
		return
	}
	a.win.SetFullScreen(v)
}

func (a *mirrpaneApp) toggleFullScreen() {
	a.setFullScreen(!a.fullscreen)
}

// shortcut reports whether the key was pressed this frame for the first time.
// MyGo sets the auto-repeat flag when Shortcut consumes the key, so asking
// ShortcutRepeated right after the match distinguishes a held key from a
// fresh press.
func shortcut(c *ui.Context, mods ui.Modifiers, key ui.Key) bool {
	return c.Shortcut(mods, key) && !c.ShortcutRepeated()
}

// disconnect asks the receiver to drop the session. Only an acknowledged
// disconnect returns to the waiting screen; a receiver that cannot be reached
// leaves the window as it was and the failure is shown to the user.
func (a *mirrpaneApp) disconnect() {
	if a.demo {
		a.pendingToast = "預覽模式使用模擬訊號"
		return
	}
	go func() { a.applyDisconnect(a.sidecar.Disconnect()) }()
}

// applyDisconnect turns one disconnect attempt into what the user sees. A
// failure is reported and leaves fullscreen alone, so a still-live picture is
// not silently windowed.
func (a *mirrpaneApp) applyDisconnect(err error) bool {
	if err != nil {
		a.toastAsync("中斷連線失敗：" + err.Error())
		return false
	}
	a.afterDisconnect()
	return true
}

// afterDisconnect leaves fullscreen and shows the waiting screen, matching the
// old UI's behaviour on a successful disconnect.
func (a *mirrpaneApp) afterDisconnect() {
	if a.closed.Load() {
		return
	}
	a.dispatchUI(func() {
		a.setFullScreen(false)
		a.pendingToast = "已中斷連線，等待重新連接"
	})
}

func (a *mirrpaneApp) openLogs() {
	a.logLines = a.sidecar.LogTail(40)
	a.showSettings = false
	a.showLogs = true
}

// ---------- Android (scrcpy) session ----------

// androidState is the receiver's Android session state, if it reports one.
func (a *mirrpaneApp) androidState() *AndroidState {
	if st := a.snap.State; st != nil {
		return st.Android
	}
	return nil
}

// androidLive reports a running Android session that owns the display.
func (a *mirrpaneApp) androidLive() bool {
	st := a.snap.State
	s := a.androidState()
	return st != nil && st.Source == "android" && s != nil && s.Phase == "live"
}

// androidActive reports a session that is starting or running.
func (a *mirrpaneApp) androidActive() bool {
	s := a.androidState()
	return s != nil && (s.Phase == "connecting" || s.Phase == "live")
}

func (a *mirrpaneApp) openAndroid() {
	a.showAndroid = true
	a.androidErr = ""
	a.refreshAndroidDevices()
}

func (a *mirrpaneApp) refreshAndroidDevices() {
	if a.demo {
		a.androidErr = "預覽模式無法使用 ADB；請從正式版連接 Android 裝置"
		return
	}
	if a.androidLoading {
		return
	}
	a.androidLoading = true
	a.androidErr = ""
	go func() {
		devices, err := a.sidecar.AndroidDevices()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		a.dispatchUI(func() {
			a.androidLoading = false
			a.androidDevices = devices
			a.androidErr = msg
		})
	}()
}

func (a *mirrpaneApp) connectAndroid(serial string) {
	if a.demo {
		a.pendingToast = "預覽模式無法連接 Android 裝置"
		return
	}
	a.androidErr = ""
	go func() {
		if err := a.sidecar.AndroidConnect(serial, 0, 0, 0); err != nil {
			a.toastAsync("連接 Android 失敗：" + err.Error())
		}
	}()
}

func (a *mirrpaneApp) disconnectAndroid() {
	if a.demo {
		return
	}
	go func() {
		if err := a.sidecar.AndroidDisconnect(); err != nil {
			a.toastAsync("中斷 Android 失敗：" + err.Error())
		}
	}()
}

func (a *mirrpaneApp) pairAndroidWireless() {
	if a.demo {
		return
	}
	address := strings.TrimSpace(a.androidAddress)
	code := strings.TrimSpace(a.androidPairCode)
	if address == "" || code == "" {
		a.pendingToast = "請輸入配對位址與 6 位數配對碼"
		return
	}
	go func() {
		if err := a.sidecar.AndroidPair(address, code); err != nil {
			a.toastAsync("配對失敗：" + err.Error())
			return
		}
		a.toastAsync("配對成功，正在連線…")
		if err := a.sidecar.AndroidWireless(address); err != nil {
			a.toastAsync("無線連線失敗：" + err.Error())
			return
		}
		a.refreshAndroidDevices()
	}()
}

func (a *mirrpaneApp) connectAndroidWireless() {
	if a.demo {
		return
	}
	address := strings.TrimSpace(a.androidAddress)
	if address == "" {
		a.pendingToast = "請輸入「IP:連接埠」"
		return
	}
	go func() {
		if err := a.sidecar.AndroidWireless(address); err != nil {
			a.toastAsync("無線連線失敗：" + err.Error())
			return
		}
		a.refreshAndroidDevices()
	}()
}

// androidDeviceLabel maps adb's state words onto the chips the dialog shows.
func androidDeviceLabel(state string) (string, ui.Color) {
	switch state {
	case "device":
		return "已授權", colOK
	case "unauthorized":
		return "待授權", colWarn
	case "offline":
		return "離線", colDanger
	default:
		return state, colMuted
	}
}

func (a *mirrpaneApp) streaming() bool {
	return a.snap.Phase == PhaseLive || a.snap.Phase == PhaseInterrupted
}

func (a *mirrpaneApp) rotateRight() {
	a.quarterTurns = (a.quarterTurns + 1) % 4
	a.pendingToast = fmt.Sprintf("畫面旋轉 %d°", a.quarterTurns*90)
}

func (a *mirrpaneApp) toggleFlip() {
	next := a.prefs
	next.FlipHorizontal = !next.FlipHorizontal
	a.savePrefs(next)
	if next.FlipHorizontal {
		a.pendingToast = "已開啟水平鏡像翻轉"
	} else {
		a.pendingToast = "已關閉水平鏡像翻轉"
	}
}

func (a *mirrpaneApp) toggleFill() {
	next := a.prefs
	next.FillViewport = !next.FillViewport
	a.savePrefs(next)
	if next.FillViewport {
		a.pendingToast = "縮放模式：填滿視窗"
	} else {
		a.pendingToast = "縮放模式：完整比例"
	}
}

func (a *mirrpaneApp) toggleHud() {
	next := a.prefs
	next.ShowHud = !next.ShowHud
	a.savePrefs(next)
}

func (a *mirrpaneApp) toggleMute() {
	next := a.prefs
	next.Muted = !next.Muted
	a.savePrefs(next)
	if next.Muted {
		a.pendingToast = "已靜音"
	} else {
		a.pendingToast = "已恢復聲音"
	}
}

func (a *mirrpaneApp) screenshotsDir() string {
	return filepath.Join(a.installDir, "screenshots")
}

// ensureScreenshotsDir creates the screenshots folder. A fresh portable
// install ships without it, and Explorer cannot open a path that does not
// exist.
func (a *mirrpaneApp) ensureScreenshotsDir() error {
	return os.MkdirAll(a.screenshotsDir(), 0o755)
}

// openScreenshots creates the screenshots folder first and reports a failure
// to create or open it instead of doing nothing.
func (a *mirrpaneApp) openScreenshots() {
	if err := a.ensureScreenshotsDir(); err != nil {
		a.pendingToast = "無法建立截圖資料夾：" + err.Error()
		return
	}
	if err := openFolder(a.screenshotsDir()); err != nil {
		a.pendingToast = "無法開啟截圖資料夾：" + err.Error()
	}
}

// openInstallDir opens the program folder and reports a failure.
func (a *mirrpaneApp) openInstallDir() {
	if err := openFolder(a.installDir); err != nil {
		a.pendingToast = "無法開啟程式資料夾：" + err.Error()
	}
}

// barShown reports whether the live top bar is on screen. It is shared by the
// bar and the HUD so the HUD's top offset cannot disagree with the bar.
func (a *mirrpaneApp) barShown(interrupted bool, st *ReceiverState) bool {
	if interrupted {
		// The interrupted card carries every action the bar would offer, and
		// its full-window backdrop would dim the bar into unclickable ghosts.
		return false
	}
	return a.barVisible || a.prefs.BarHideSeconds == 0 || audioOnly(st) || a.displayMenuOpen || a.moreMenuOpen
}

// audioOnly reports a live session that carries sound but no picture, which
// the receiver marks with the "audio" reason.
func audioOnly(st *ReceiverState) bool {
	return st != nil && st.Reason == "audio"
}

func (a *mirrpaneApp) takeSnapshot() {
	go func() {
		path, err := a.sidecar.TakeSnapshot()
		if err != nil {
			a.toastAsync("截圖失敗：" + err.Error())
			return
		}
		a.toastActionAsync("已儲存截圖："+filepath.Base(path), "開啟資料夾", filepath.Dir(path))
	}()
}

func (a *mirrpaneApp) toastActionAsync(msg, label, folder string) {
	win := a.win
	if win == nil || win.IsDestroyed() {
		return
	}
	win.Update(func() {
		a.pendingToastAction = &toastAction{msg: msg, label: label, folder: folder}
	})
}

// openSettings seeds the config.json drafts from the latest receiver state.
func (a *mirrpaneApp) openSettings() {
	if a.showSettings {
		return
	}
	a.showLogs = false
	draft := configDraft{
		name:        "鏡屏",
		resKey:      "1920x1080",
		maxFps:      60,
		decoderMode: "auto",
		colorRange:  "auto",
		scaleFilter: "fast",
		audioBuffer: "balanced",
	}
	if st := a.snap.State; st != nil {
		if st.TargetWidth > 0 && st.TargetHeight > 0 {
			draft.resKey = fmt.Sprintf("%dx%d", st.TargetWidth, st.TargetHeight)
		}
		if st.MaxFPS == 30 {
			draft.maxFps = 30
		}
		if st.DecoderMode != "" {
			draft.decoderMode = st.DecoderMode
		}
		if st.ColorRange != "" {
			draft.colorRange = st.ColorRange
		}
		if st.ScaleFilter != "" {
			draft.scaleFilter = st.ScaleFilter
		}
		if st.AudioBuffer != "" {
			draft.audioBuffer = st.AudioBuffer
		}
		draft.requireApproval = st.RequireApproval
		draft.name = st.Name
	}
	a.configDraft = draft
	a.initialConfig = draft
	a.showSettings = true
}

// saveConfigAndRestart writes the graphics/audio drafts into config.json and
// restarts the sidecar so they take effect, mirroring the old UI's
// "儲存並重啟" button.
//
// The settings dialog is closed only once the restart is accepted; while a
// previous restart is still running the draft stays open so an edit cannot be
// dropped without the user seeing it.
func (a *mirrpaneApp) saveConfigAndRestart() {
	if a.demo {
		a.pendingToast = "預覽模式使用模擬訊號"
		return
	}
	if a.restarting || a.closed.Load() {
		a.pendingToast = "接收器正在重新啟動，請稍候再儲存"
		return
	}
	patch := a.configDraft.changes(a.initialConfig)
	a.showSettings = false
	if len(patch) == 0 {
		a.restartReceiver(nil, "")
		return
	}
	a.restartReceiver(func() error { return SaveConfigPatch(a.installDir, patch) }, "接收設定已儲存")
}

// An offline or starting receiver may not have supplied its real settings.
// Only fields the user edited should be merged into its existing config file.
func (draft configDraft) changes(original configDraft) map[string]any {
	patch := map[string]any{}
	if name := strings.TrimSpace(draft.name); name != "" && name != original.name {
		patch["name"] = name
	}
	if res, ok := resolutionPresets[draft.resKey]; ok && draft.resKey != original.resKey {
		patch["width"], patch["height"] = res[0], res[1]
	}
	if draft.maxFps != original.maxFps {
		patch["maxFps"] = draft.maxFps
	}
	if draft.decoderMode != original.decoderMode {
		patch["decoderMode"] = draft.decoderMode
	}
	if draft.colorRange != original.colorRange {
		patch["colorRange"] = draft.colorRange
	}
	if draft.scaleFilter != original.scaleFilter {
		patch["scaleFilter"] = draft.scaleFilter
	}
	if draft.audioBuffer != original.audioBuffer {
		patch["audioBuffer"] = draft.audioBuffer
	}
	if draft.requireApproval != original.requireApproval {
		patch["requireApproval"] = draft.requireApproval
	}
	return patch
}

// Only one restart can own the settings and sidecar at a time. A failed edit
// still brings the receiver back with the original configuration. It reports
// whether this call took ownership, so a caller can keep its draft open when a
// previous restart is still running.
func (a *mirrpaneApp) restartReceiver(edit func() error, message string) bool {
	if a.demo {
		a.pendingToast = "預覽模式使用模擬訊號"
		return false
	}
	if a.restarting || a.closed.Load() {
		return false
	}
	a.restarting = true
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		a.sidecar.Stop()
		if edit != nil {
			if err := edit(); err != nil {
				message = "更新設定失敗：" + err.Error()
			}
		}
		if !a.closed.Load() {
			a.sidecar.Start()
			win := a.win
			if win == nil || win.IsDestroyed() {
				a.restarting = false
				a.pendingToast = message
				return
			}
			win.Update(func() {
				a.restarting = false
				a.pendingToast = message
			})
		}
	}()
	return true
}

func (a *mirrpaneApp) view(c *ui.Context) {
	if a.closed.Load() {
		return
	}
	c.SetTheme(a.theme)

	if a.pendingToast != "" {
		c.Toast(a.pendingToast)
		a.pendingToast = ""
	}
	if ta := a.pendingToastAction; ta != nil {
		a.pendingToastAction = nil
		folder := ta.folder
		c.ToastAction(ta.msg, ta.label, func() {
			if err := openFolder(folder); err != nil {
				a.pendingToast = "無法開啟資料夾：" + err.Error()
			}
		})
	}

	// Global keyboard shortcuts. Modifier shortcuts are checked first; the
	// single-letter ones are skipped while a text field has focus.
	//
	// MyGo only sets ShortcutRepeated when Shortcut consumes the key, so the
	// repeat check has to follow each match. Checking it once before the
	// matches let every auto-repeat frame run the action again.
	if shortcut(c, 0, ui.KeyF11) {
		a.toggleFullScreen()
	}
	if shortcut(c, 0, ui.KeyEscape) {
		switch {
		case a.showSettings, a.showLogs, a.showAndroid:
			a.showSettings, a.showLogs, a.showAndroid = false, false, false
		case a.fullscreen:
			a.setFullScreen(false)
		case a.androidLive() && !c.FocusTakesText():
			// Escape is Android's Back while a device owns the picture.
			a.forwardInput(AndroidInput{Kind: "back"})
		}
	}
	if shortcut(c, ui.Ctrl, ui.KeyT) {
		a.toggleAlwaysOnTop()
		if a.alwaysOnTop {
			a.pendingToast = "視窗已置頂"
		} else {
			a.pendingToast = "已取消視窗置頂"
		}
	}
	if shortcut(c, ui.Ctrl, ui.KeyL) {
		a.openLogs()
	}
	if shortcut(c, ui.Ctrl, ui.KeyComma) {
		a.openSettings()
	}
	if shortcut(c, ui.Ctrl, ui.KeyK) {
		// Alias for Ctrl+, because an active IME can swallow punctuation
		// chords before the app sees them.
		a.openSettings()
	}
	if shortcut(c, ui.Ctrl, ui.KeyS) && a.streaming() {
		a.takeSnapshot()
	}
	if !a.showSettings && !a.showLogs && !a.showAndroid && !c.FocusTakesText() && a.streaming() {
		if shortcut(c, 0, ui.KeyR) {
			a.rotateRight()
		}
		if shortcut(c, 0, ui.KeyH) {
			a.toggleFlip()
		}
		if shortcut(c, 0, ui.KeyZ) {
			a.toggleFill()
		}
		if shortcut(c, 0, ui.KeyM) {
			a.toggleMute()
		}
		if shortcut(c, 0, ui.KeyTab) && !a.androidLive() {
			a.toggleHud()
		}
	}
	if a.androidLive() && !a.showSettings && !a.showLogs && !a.showAndroid && !c.FocusTakesText() {
		a.forwardAndroidKeys(c)
	}

	root := ui.Box(c).Fill().Background(colGround)
	// mygo reports punctuation keys as KeyUnknown while Ctrl is held, so the
	// settings shortcut is also matched by the physical key code.
	root.ObserveInput(func(ev ui.InputEvent) {
		if ev.Kind == ui.InputKeyDown && !ev.Repeat && ev.Mods&ui.Ctrl != 0 &&
			(ev.Key == ui.KeyComma || ev.Code == ui.CodeComma) && !a.showSettings {
			a.openSettings()
			if a.win != nil && !a.win.IsDestroyed() {
				a.win.Invalidate()
			}
		}
	})
	root.Children(func() {
		switch a.snap.Phase {
		case PhaseBooting:
			a.buildBootingScreen(c)
		case PhaseWaiting:
			a.buildWaitingScreen(c)
		case PhaseConnecting:
			if a.snap.State != nil && a.snap.State.PendingSender != nil {
				a.buildWaitingScreen(c)
			} else {
				a.buildConnectingScreen(c)
			}
		case PhaseLive:
			a.presentFrame(c)
			a.buildLiveScreen(c, false)
		case PhaseInterrupted:
			a.buildLiveScreen(c, true)
		case PhaseFailed:
			a.buildFailedScreen(c)
		}
	})

	a.buildLogsModal(c)
	a.buildSettingsModal(c)
	a.buildAndroidModal(c)
}

func (a *mirrpaneApp) buildBootingScreen(c *ui.Context) {
	root := heroRoot(c)
	root.Children(func() {
		a.idleChrome(c)
		ui.Column(c).Grow(1).FillWidth().Center().Padding(24).Children(func() {
			ui.Column(c).Gap(20).AlignItems(ui.Center).Children(func() {
				heroHeader(c, "鏡屏", "AIRPLAY 接收器")
				statusPill(c, "正在啟動接收器", colWarn)
				ui.Spinner(c)
				msg := a.snap.Message
				if msg == "" {
					msg = "正在初始化…"
				}
				ui.Text(c, msg).FontSize(13).TextColor(colMuted).TextAlign(ui.Center)
				ui.Row(c).Gap(10).Children(func() {
					softButton(c, "boot-logs", icons.logs, "查看日誌", false, a.openLogs)
					softButton(c, "boot-restart", icons.refresh, "重新啟動", false, func() {
						a.restartReceiver(nil, "")
					})
				})
			})
		})
	})
}

func (a *mirrpaneApp) buildWaitingScreen(c *ui.Context) {
	st := a.snap.State
	if st == nil {
		s := defaultReceiverState()
		st = &s
	}
	availW, availH := c.Size()
	padY := float32(28)
	if availH > 720 {
		padY = 44
	}

	root := heroRoot(c)
	root.Children(func() {
		a.idleChrome(c)
		// Scroll-safe stage: 640×420 and 150% DPI windows scroll instead of
		// clipping the mark or the action row.
		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().MinHeight(availH).Padding(padY, 24).Center().Children(func() {
				ui.Column(c).Gap(18).AlignItems(ui.Center).Children(func() {
					heroHeader(c, "鏡屏", "AIRPLAY 螢幕鏡像接收器")

					// A device waiting for manual approval is the one action that
					// matters: it replaces the connect card until it is answered.
					if p := st.PendingSender; p != nil {
						statusPill(c, "等待核准連線", colWarn)
						a.buildApprovalCard(c, p, availW)
						return
					}

					statusPill(c, "等待手機或平板連線", colOK)
					a.buildConnectCard(c, st, availW)
					ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
						softButton(c, "wait-android", icons.phone, "Android 裝置", false, a.openAndroid)
						softButton(c, "wait-rescan", icons.refresh, "重新廣播", false, func() {
							a.restartReceiver(nil, "接收器已重新啟動")
						})
						softButton(c, "wait-shots", icons.folder, "截圖資料夾", false, a.openScreenshots)
					})
				})
			})
		})
	})
}

// buildConnectCard is the waiting screen's core: the broadcast name to look
// for on the sender, the address to check, and the steps that connect.
func (a *mirrpaneApp) buildConnectCard(c *ui.Context, st *ReceiverState, availW float32) {
	name := nonEmpty(st.Name, "鏡屏")
	card(c, min(560, availW-40)).Children(func() {
		// The broadcast name is what the sender's list shows, so it leads;
		// resolution and decoder are reference data beside it.
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Children(func() {
			ui.Box(c).Width(3).Height(38).Radius(2).Background(colAccent)
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, "AIRPLAY 接收名稱").FontSize(10.5).TextColor(colFaint).LetterSpacing(1.4)
				ui.Text(c, name).FontSize(26).Bold().TextColor(colInk).SingleLine()
			})
			ui.Textf(c, "%d × %d · %d fps", st.TargetWidth, st.TargetHeight, st.MaxFPS).
				Font("Cascadia Mono").FontSize(12).TextColor(colMuted).FontFeatures("tnum")
		})

		addr := fmt.Sprintf("%s:%d", nonEmpty(st.IP, "127.0.0.1"), st.Port)
		ui.Row(c).FillWidth().Padding(10, 14).Radius(12).Background(colWell).
			Border(1, colHairline).AlignItems(ui.Center).Gap(12).Children(func() {
			ui.Column(c).Grow(1).Gap(1).Children(func() {
				ui.Text(c, "區域網路位址").FontSize(11).TextColor(colFaint)
				ui.Text(c, addr).Font("Cascadia Mono").FontSize(15).TextColor(colInk).FontFeatures("tnum")
			})
			if softButton(c, "copy-addr", icons.copy, "複製位址", false, func() {
				c.WriteClipboard(addr)
				c.Toast("已複製 " + addr)
			}) {
			}
		})

		ui.Text(c, "在 iPhone 控制中心 → 螢幕鏡像輸出 → 選擇「"+name+"」").
			FontSize(12).TextColor(colMuted)
	})
}

// buildApprovalCard answers a sender that waits for manual approval.
func (a *mirrpaneApp) buildApprovalCard(c *ui.Context, p *PendingSender, availW float32) {
	card(c, min(480, availW-40)).Border(1, colAccent.Alpha(0.5)).
		InsetShadow(0, 1, 0, 0, colAccent.Alpha(0.18)).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "新裝置配對請求").FontSize(11.5).Bold().TextColor(colAccent).LetterSpacing(0.6)
			ui.Spacer(c)
			ui.Textf(c, "%d 秒前", p.AgeSeconds).Font("Cascadia Mono").FontSize(11.5).
				TextColor(colFaint).FontFeatures("tnum")
		})
		ui.Column(c).Gap(3).Children(func() {
			ui.Text(c, nonEmpty(p.Label, nonEmpty(p.Remote, "未知裝置"))).FontSize(24).Bold().TextColor(colInk)
			ui.Textf(c, "來源 %s", nonEmpty(p.Remote, "-")).FontSize(12).TextColor(colMuted)
		})
		ui.Row(c).FillWidth().Padding(8, 12).Background(colGround).Border(1, colHairline).
			Radius(8).AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, "指紋").FontSize(11).TextColor(colFaint)
			ui.Text(c, shortFingerprint(p.Fingerprint)).Font("Cascadia Mono").FontSize(12).
				TextColor(colMuted).FontFeatures("tnum").Grow(1).SingleLine()
		})
		fp := p.Fingerprint
		ui.Row(c).FillWidth().Justify(ui.End).Gap(10).Children(func() {
			softButton(c, "reject-sender", icons.close, "拒絕", true, func() {
				go func() {
					if err := a.sidecar.RejectSender(fp); err != nil {
						a.toastAsync("拒絕失敗：" + err.Error())
					} else {
						a.toastAsync("已拒絕連線請求")
					}
				}()
			})
			primaryButton(c, "approve-sender", icons.check, "允許連線", func() {
				go func() {
					if err := a.sidecar.ApproveSender(fp); err != nil {
						a.toastAsync("核准失敗：" + err.Error())
					} else {
						a.toastAsync("已核准，請在裝置上再次選取接收器")
					}
				}()
			})
		})
	})
}

func (a *mirrpaneApp) buildConnectingScreen(c *ui.Context) {
	st := a.snap.State
	availW, _ := c.Size()
	root := heroRoot(c)
	root.Children(func() {
		a.idleChrome(c)
		ui.Column(c).Grow(1).FillWidth().Center().Padding(24).Children(func() {
			// The connecting device owns the stage; the receiver's own identity
			// stays out of the way until the session starts.
			card(c, min(440, availW-40)).AlignItems(ui.Center).Gap(14).Children(func() {
				ui.Box(c).Width(48).Height(48).Radius(14).Background(colSurfaceHi).
					Border(1, colBorder).Center().Children(func() {
					ui.Image(c, brandMark).Width(30).Height(30)
				})
				statusPill(c, "正在建立鏡像串流", colAccent)
				device := "AirPlay 裝置"
				if st != nil && st.Sender != "" {
					device = st.Sender
				}
				ui.Text(c, device).FontSize(24).Bold().TextColor(colInk).TextAlign(ui.Center)
				if st != nil && st.Reason != "" {
					ui.Text(c, st.Reason).FontSize(13).TextColor(colMuted).TextAlign(ui.Center)
				}
				ui.Progress(c, -1).Width(200).Height(3)
				softButton(c, "cancel-connect", icons.power, "中斷連線", true, a.disconnect)
			})
		})
	})
}

func (a *mirrpaneApp) buildLiveScreen(c *ui.Context, interrupted bool) {
	st := a.snap.State
	if st == nil {
		s := defaultReceiverState()
		st = &s
	}

	box := ui.Box(c).Fill().Background(ui.RGB(0, 0, 0))
	// Pointer activity reveals the toolbar, and a double click toggles fullscreen.
	// HandleInput receives only events left unhandled by the child controls.
	android := a.androidLive()
	box.HandleInput(func(ev ui.InputEvent) bool {
		switch ev.Kind {
		case ui.InputPointerMove, ui.InputPointerDown:
			a.barLastMove = time.Now()
			if !a.barVisible {
				a.barVisible = true
				a.barHovered = false
				if a.win != nil && !a.win.IsDestroyed() {
					a.win.Invalidate()
				}
			}
			// A double click on an Android picture would send two taps to the
			// phone, so fullscreen keeps to F11 there.
			if !android && ev.Kind == ui.InputPointerDown && ev.Clicks >= 2 && !a.barHovered {
				a.toggleFullScreen()
			}
		}
		if !android || a.showSettings || a.showLogs || a.showAndroid {
			return false
		}
		switch ev.Kind {
		case ui.InputPointerDown:
			if ev.Button == 0 {
				a.androidPointerEvent(ev, c)
			}
		case ui.InputPointerMove:
			if a.androidPointer {
				a.androidPointerEvent(ev, c)
			}
		case ui.InputPointerUp:
			if a.androidPointer {
				a.androidPointerEvent(ev, c)
			}
		case ui.InputScroll:
			a.androidScrollEvent(ev, c)
		}
		return false // observing only: the press goes on as it would
	})
	box.Children(func() {
		a.buildVideoSurface(c)

		// Overlay top bar. ZIndex keeps the bar above the GPU-composited video
		// picture, which this mygo version paints over later siblings otherwise.
		showBar := a.barShown(interrupted, st)
		if !showBar {
			a.barHovered, a.barFocused = false, false
		}
		if showBar {
			ui.Column(c).Absolute().Top(0).Left(0).Right(0).Padding(12, 14).ZIndex(60).Children(func() {
				a.buildLiveTopBar(c, st, interrupted)
			})
		}

		// Audio-only sessions have no picture; name the sender on the empty
		// surface so a live audio stream does not look like a missing video.
		if !interrupted && audioOnly(st) {
			ui.Column(c).Absolute().Top(0).Left(0).Right(0).Bottom(0).Center().PassThrough().
				Children(func() {
					ui.Column(c).Width(400).Padding(26).Gap(12).Radius(16).Background(colOverlayBg).
						Border(1, colBorder).Shadow(0, 22, 60, 0, ui.RGBA(0, 0, 0, 0.5)).
						AlignItems(ui.Center).Children(func() {
						ui.Box(c).Width(52).Height(52).Radius(14).Background(colAccent.Alpha(0.12)).
							Border(1, colAccent.Alpha(0.3)).Center().Children(func() {
							ui.Icon(c, icons.music).Width(24).Height(24).TextColor(colAccent)
						})
						pill(c, "僅音訊", colAccent)
						ui.Textf(c, "來源裝置：%s", nonEmpty(st.Sender, "AirPlay")).
							FontSize(16).Bold().TextColor(colInk)
						ui.Text(c, "發送端正在播放音訊，沒有視訊畫面。").
							FontSize(12.5).TextColor(colMuted).TextAlign(ui.Center)
					})
				})
		}

		// Telemetry HUD overlay.
		if a.prefs.ShowHud {
			a.buildHudOverlay(c, st, showBar)
		}

		// Interrupted overlay card over the frozen last frame.
		if interrupted {
			ui.Column(c).Absolute().Top(0).Left(0).Right(0).Bottom(0).Center().
				Background(ui.RGBA(0, 0, 0, 0.55)).ZIndex(70).Children(func() {
				ui.Column(c).Width(430).Padding(26).Gap(14).Radius(16).Background(colSurface).
					Border(1, colWarn.Alpha(0.55)).Shadow(0, 24, 64, 0, ui.RGBA(0, 0, 0, 0.55)).
					AlignItems(ui.Center).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						statusDot(c, colWarn, 8)
						ui.Text(c, "串流中斷").FontSize(11.5).Bold().TextColor(colWarn).LetterSpacing(1)
					})
					ui.Text(c, "畫面暫時中斷").FontSize(20).Bold().TextColor(colInk)
					ui.Text(c, nonEmpty(st.Reason, "等待傳送端恢復串流或重新連線…")).
						FontSize(12.5).TextColor(colMuted).TextAlign(ui.Center)
					ui.Row(c).Gap(10).Children(func() {
						if a.fullscreen {
							softButton(c, "int-fs-exit", icons.fullscreen, "離開全螢幕", false, func() {
								a.setFullScreen(false)
							})
						}
						softButton(c, "int-restart", icons.refresh, "重啟接收器", false, func() {
							a.restartReceiver(nil, "")
						})
						primaryButton(c, "int-disconnect", icons.power, "返回等待畫面", a.disconnect)
					})
				})
			})
		}
	})
}

func (a *mirrpaneApp) buildLiveTopBar(c *ui.Context, st *ReceiverState, interrupted bool) {
	displayFPS := st.FPS
	if displayFPS <= 0 {
		displayFPS = a.uiFPS
	}
	resText := fmt.Sprintf("%dx%d", st.Width, st.Height)
	if st.Width <= 0 || st.Height <= 0 {
		bw, bh := a.bitmap.Size()
		resText = fmt.Sprintf("%dx%d", bw, bh)
	}
	viewW, _ := c.Size()
	media := audioOnly(st)
	android := a.androidLive()
	compact := viewW < 720
	medium := viewW < 920

	bar := ui.Row(c).FillWidth().Padding(6, 10).Gap(2).AlignItems(ui.Center).Radius(999).
		Background(colGlass).Material(glass.Blur{Radius: 20}).
		Border(1, colBorder).InsetShadow(0, 1, 0, 0, colRim).
		Shadow(0, 12, 36, 0, ui.RGBA(0, 0, 0, 0.5))
	// Hovered and FocusWithin are read as the bar builds: mygo rebuilds when
	// the pointer comes over it, leaves, or the keyboard focus changes, and
	// while a control holds either the hide timer stays paused.
	hovered, focused := bar.Hovered(), bar.FocusWithin()
	a.barHovered, a.barFocused = hovered, focused
	if hovered || focused {
		a.barLastMove = time.Now()
	}
	var dispBtn, moreBtn ui.Element
	bar.Children(func() {
		// Sender and stream quality.
		lamp := colOK
		if interrupted {
			lamp = colWarn
		}
		ui.Box(c).MarginX(8).Children(func() { statusDot(c, lamp, 7) })
		senderMax := float32(240)
		if medium {
			senderMax = 170
		}
		if compact {
			senderMax = 130
		}
		ui.Text(c, nonEmpty(st.Sender, "AirPlay")).FontSize(13.5).Bold().TextColor(colInk).
			SingleLine().MaxWidth(senderMax)
		if !compact {
			meta := fmt.Sprintf("%s · %.0f fps", resText, displayFPS)
			textColor := colMuted
			if media {
				meta = "僅音訊 · AAC-ELD"
				textColor = colAccent
			}
			if android {
				meta += " · Android"
				textColor = colAccent
			}
			ui.Text(c, meta).Font("Cascadia Mono").FontSize(11).TextColor(textColor).
				FontFeatures("tnum").SingleLine().MaxWidth(viewW - 560).MarginStart(6)
		}
		ui.Spacer(c)

		// Android navigation runs before the sound controls so the device's
		// Back/Home/Recents are the first reachable actions.
		if android && !compact {
			if iconButton(c, "live-back", icons.backArrow, "返回鍵 (Esc)", false, false).Clicked() {
				a.forwardInput(AndroidInput{Kind: "back"})
			}
			if iconButton(c, "live-home", icons.homeIcon, "主畫面", false, false).Clicked() {
				a.forwardInput(AndroidInput{Kind: "home"})
			}
			if iconButton(c, "live-recents", icons.recents, "最近應用", false, false).Clicked() {
				a.forwardInput(AndroidInput{Kind: "appswitch"})
			}
			if iconButton(c, "live-android-rot", icons.rotate, "旋轉裝置", false, false).Clicked() {
				a.forwardInput(AndroidInput{Kind: "rotate"})
			}
			vrule(c)
		}

		// Sound: mute toggle and a slim volume slider.
		muteIcon := icons.volume
		muteTip := "靜音 (M)"
		if a.prefs.Muted {
			muteIcon, muteTip = icons.mute, "恢復聲音 (M)"
		}
		if iconButton(c, "live-mute", muteIcon, muteTip, a.prefs.Muted, false).Clicked() {
			a.toggleMute()
		}
		vol := a.prefs.Volume
		if a.prefs.Muted {
			vol = 0
		}
		if ui.Slider(c.Key("live-vol"), &vol, 0, 1).Width(72).MarginX(3).Changed() {
			next := a.prefs
			next.Volume = vol
			next.Muted = vol <= 0.001
			a.savePrefs(next)
		}
		vrule(c)

		// Picture: snapshot plus the display-adjustment menu. An audio-only
		// session has no picture, so both are disabled rather than hidden.
		if !compact {
			snap := iconButton(c, "live-snap", icons.camera, "截圖 (Ctrl+S)", false, false)
			snap.Disabled(media)
			if snap.Clicked() {
				a.takeSnapshot()
			}
		}
		transformed := a.quarterTurns%4 != 0 || a.prefs.FlipHorizontal ||
			a.prefs.FillViewport || a.prefs.ShowHud
		dispBtn = iconButton(c, "live-display", icons.display, "畫面調整", a.displayMenuOpen || transformed, false)
		dispBtn.Disabled(media)
		if dispBtn.Clicked() && !media {
			a.displayMenuOpen = !a.displayMenuOpen
			a.moreMenuOpen = false
		}
		vrule(c)

		// Window: pin and full screen.
		if !compact {
			if iconButton(c, "live-pin", icons.pin, pinLabel(a.alwaysOnTop), a.alwaysOnTop, false).Clicked() {
				a.toggleAlwaysOnTop()
				if a.alwaysOnTop {
					a.pendingToast = "視窗已置頂"
				} else {
					a.pendingToast = "已取消視窗置頂"
				}
			}
		}
		if iconButton(c, "live-fs", icons.fullscreen, fullscreenLabel(a.fullscreen), a.fullscreen, false).Clicked() {
			a.toggleFullScreen()
		}
		vrule(c)

		if !compact {
			if iconButton(c, "live-logs", icons.logs, "日誌 (Ctrl+L)", false, false).Clicked() {
				a.openLogs()
			}
		}
		if iconButton(c, "live-settings", icons.settings, "設定 (Ctrl+K)", false, false).Clicked() {
			a.openSettings()
		}
		if compact {
			moreBtn = iconButton(c, "live-more", icons.more, "更多", a.moreMenuOpen, false)
			if moreBtn.Clicked() {
				a.moreMenuOpen = !a.moreMenuOpen
				a.displayMenuOpen = false
			}
		}
		if iconButton(c, "live-disc", icons.power, "中斷連線", false, true).Clicked() {
			a.disconnect()
		}
	})

	// The display menu: what the old three-row bar spread over two rows.
	ui.PopoverBase(c, dispBtn, &a.displayMenuOpen, func(panel ui.Element) {
		menuPanel(panel)
		panel.Width(232).ZIndex(240)
		panel.Children(func() {
			menuItem(c, "menu-rot", icons.rotate, "旋轉 90°", "R", a.quarterTurns%4 != 0, false, func() {
				a.displayMenuOpen = false
				a.rotateRight()
			})
			menuItem(c, "menu-flip", icons.flip, "水平翻轉", "H", a.prefs.FlipHorizontal, false, func() {
				a.displayMenuOpen = false
				a.toggleFlip()
			})
			menuItem(c, "menu-fill", icons.fill, "填滿視窗", "Z", a.prefs.FillViewport, false, func() {
				a.displayMenuOpen = false
				a.toggleFill()
			})
			ui.Box(c).FillWidth().Height(1).Background(colBorder).MarginY(4)
			menuItem(c, "menu-hud", icons.hud, "效能 HUD", "Tab", a.prefs.ShowHud, false, func() {
				a.displayMenuOpen = false
				a.toggleHud()
			})
		})
	})

	// Below 720 DIPs the bar trades three direct buttons for one menu.
	ui.PopoverBase(c, moreBtn, &a.moreMenuOpen, func(panel ui.Element) {
		menuPanel(panel)
		panel.Width(216).ZIndex(240)
		panel.Children(func() {
			if android {
				menuItem(c, "more-back", icons.backArrow, "返回鍵", "Esc", false, false, func() {
					a.moreMenuOpen = false
					a.forwardInput(AndroidInput{Kind: "back"})
				})
				menuItem(c, "more-home", icons.homeIcon, "主畫面", "", false, false, func() {
					a.moreMenuOpen = false
					a.forwardInput(AndroidInput{Kind: "home"})
				})
				menuItem(c, "more-recents", icons.recents, "最近應用", "", false, false, func() {
					a.moreMenuOpen = false
					a.forwardInput(AndroidInput{Kind: "appswitch"})
				})
				menuItem(c, "more-rot", icons.rotate, "旋轉裝置", "", false, false, func() {
					a.moreMenuOpen = false
					a.forwardInput(AndroidInput{Kind: "rotate"})
				})
				ui.Box(c).FillWidth().Height(1).Background(colBorder).MarginY(4)
			}
			menuItem(c, "more-snap", icons.camera, "截圖", "Ctrl+S", false, media, func() {
				a.moreMenuOpen = false
				if !media {
					a.takeSnapshot()
				}
			})
			menuItem(c, "more-pin", icons.pin, pinLabel(a.alwaysOnTop), "", a.alwaysOnTop, false, func() {
				a.moreMenuOpen = false
				a.toggleAlwaysOnTop()
			})
			menuItem(c, "more-logs", icons.logs, "日誌", "Ctrl+L", false, false, func() {
				a.moreMenuOpen = false
				a.openLogs()
			})
		})
	})
}

// hudMetric is one telemetry line of the HUD: its label and reading over a
// meter that spans the card's full width.
func hudMetric(c *ui.Context, label, text string, value, hi float64, levels *ui.MeterLevels) {
	ui.Column(c).FillWidth().Gap(3).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, label).FontSize(10.5).TextColor(colMuted)
			ui.Spacer(c)
			ui.Text(c, text).Font("Cascadia Mono").FontSize(10.5).TextColor(colInk).FontFeatures("tnum")
		})
		ui.Meter(c, value, 0, hi, levels).FillWidth().Height(4.5).Background(colGround)
	})
}

func (a *mirrpaneApp) buildHudOverlay(c *ui.Context, st *ReceiverState, showBar bool) {
	bw, bh := a.bitmap.Size()
	w, h := st.Width, st.Height
	if w <= 0 || h <= 0 {
		w, h = bw, bh
	}
	fps := st.FPS
	if fps <= 0 {
		fps = a.uiFPS
	}
	media := audioOnly(st)

	// Position all four corners in the view's logical coordinates. The bar's
	// visibility decides the top offset, so an interrupted stream that hides
	// the bar cannot leave the HUD floating too low, and an open display menu
	// cannot cover a top-right HUD.
	const cardH = 172
	_, viewH := c.Size()
	topPad := float32(74) // clear the single-row toolbar
	if !showBar {
		topPad = 14
	}
	top := topPad
	if a.prefs.HudCorner != "topLeft" && a.prefs.HudCorner != "topRight" {
		top = viewH - cardH - 14
		if top < topPad {
			top = topPad
		}
	}
	if a.displayMenuOpen && a.prefs.HudCorner == "topRight" {
		top += 206
	}

	card := ui.Column(c).Absolute().Top(top).Width(288).PassThrough().
		Padding(12, 14).Gap(8).Radius(18).Background(colHudBg).
		Material(glass.Blur{Radius: 20}).Border(1, colBorder).
		InsetShadow(0, 1, 0, 0, colRim).ZIndex(50)
	if a.prefs.HudCorner == "topRight" || a.prefs.HudCorner == "bottomRight" {
		card.Right(14)
	} else {
		card.Left(14)
	}
	card.Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
			statusDot(c, colOK, 6)
			ui.Text(c, "效能 HUD").FontSize(10).Bold().TextColor(colMuted).LetterSpacing(1.4)
			ui.Spacer(c)
			detail := fmt.Sprintf("%d × %d", w, h)
			if media {
				detail = "AAC-ELD 48kHz"
			}
			ui.Text(c, detail).Font("Cascadia Mono").FontSize(10.5).TextColor(colFaint).FontFeatures("tnum")
		})
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(4).Children(func() {
			ui.Textf(c, "%.1f", fps).Font("Cascadia Mono").FontSize(26).Bold().TextColor(colInk).FontFeatures("tnum")
			ui.Text(c, "fps").FontSize(11).TextColor(colMuted)
			ui.Spacer(c)
			uiFPS := "UI —"
			if a.uiFPS >= 0.5 {
				uiFPS = fmt.Sprintf("UI %.0f", a.uiFPS)
			}
			ui.Text(c, uiFPS).Font("Cascadia Mono").FontSize(10.5).TextColor(colFaint).FontFeatures("tnum")
		})
		hudMetric(c, "解碼延遲",
			fmt.Sprintf("%.1f / p95 %.1f ms", st.DecodeAvgMs, st.DecodeP95Ms),
			st.DecodeP95Ms, 16.7, &ui.MeterLevels{Warning: 12, Critical: 16.7})
		hudMetric(c, "音訊緩衝",
			fmt.Sprintf("%.0f ms · 欠載 %d", st.AudioOccupancy, st.AudioUnderruns),
			st.AudioOccupancy, 200, &ui.MeterLevels{Warning: 140, Critical: 180})
		ui.Textf(c, "LAG %d · DROP %d", st.LagFrames, st.Dropped).
			Font("Cascadia Mono").FontSize(9.5).TextColor(colFaint).FontFeatures("tnum").SingleLine()
	})
}

func (a *mirrpaneApp) buildFailedScreen(c *ui.Context) {
	availW, availH := c.Size()
	cardW := min(660, availW-40)
	logH := float32(210)
	if len(a.logLines) == 0 {
		logH = 84
	}
	portHint := strings.Contains(a.snap.Error, "7000") || strings.Contains(a.snap.Error, "連接埠")

	root := heroRoot(c)
	root.Children(func() {
		a.idleChrome(c)
		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().MinHeight(availH).Padding(32, 24).Center().Children(func() {
				ui.Column(c).Gap(18).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Width(76).Height(76).Radius(20).Background(colDanger.Alpha(0.1)).
						Border(1, colDanger.Alpha(0.4)).InsetShadow(0, 1, 0, 0, colRim).
						Center().Children(func() {
						ui.Icon(c, icons.alert).Width(32).Height(32).TextColor(colDanger)
					})
					ui.Column(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "接收器無法啟動").FontSize(28).Bold().TextColor(colInk)
						ui.Text(c, nonEmpty(a.snap.Error, "未知錯誤")).FontSize(13).TextColor(colMuted).
							TextAlign(ui.Center).MaxWidth(600)
					})
					if portHint {
						ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(8, 12).Radius(9).
							Background(colWarn.Alpha(0.1)).Border(1, colWarn.Alpha(0.28)).
							MaxWidth(cardW).Children(func() {
							statusDot(c, colWarn, 6)
							ui.Text(c, "提示：請關閉其他 AirPlay 接收程式，或檢查防火牆的 7000 連接埠").
								FontSize(11.5).TextColor(colWarn)
						})
					}

					ui.Column(c).Width(cardW).Padding(18).Gap(12).Radius(14).Background(colSurface).
						Border(1, colBorder).InsetShadow(0, 1, 0, 0, colRim).
						Shadow(0, 22, 56, 0, ui.RGBA(0, 0, 0, 0.48)).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
							ui.Icon(c, icons.logs).Width(15).Height(15).TextColor(colMuted)
							ui.Text(c, "airplayd.log 近期內容").FontSize(12).Bold().TextColor(colInk)
							ui.Spacer(c)
							if softButton(c, "fail-refresh-log", icons.refresh, "重新整理", false, func() {
								a.logLines = a.sidecar.LogTail(24)
							}) {
							}
						})
						ui.Scroll(c).Height(logH).FillWidth().Padding(12).Radius(10).
							Background(colGround).Border(1, colHairline).Children(func() {
							if len(a.logLines) == 0 {
								ui.Text(c, "（尚無日誌內容）").Font("Cascadia Mono").FontSize(11.5).TextColor(colFaint)
							} else {
								for i, line := range a.logLines {
									ui.Text(c.Key(i), line).Font("Cascadia Mono").FontSize(11).
										TextColor(colMuted).SingleLine()
								}
							}
						})
					})

					ui.Row(c).Gap(10).Children(func() {
						softButton(c, "fail-open-dir", icons.folder, "開啟程式資料夾", false, a.openInstallDir)
						softButton(c, "fail-logs", icons.logs, "完整日誌", false, a.openLogs)
						primaryButton(c, "fail-retry", icons.refresh, "重試", func() {
							a.restartReceiver(nil, "")
						})
					})
				})
			})
		})
	})
}

// forwardAndroidKeys sends the editing and navigation keys an Android device
// expects. Letters are left to the app's shortcuts: a desktop keyboard layout
// does not map onto the phone's IME, so typing on it is a later phase.
func (a *mirrpaneApp) forwardAndroidKeys(c *ui.Context) {
	pairs := []struct {
		key  ui.Key
		code int
	}{
		{ui.KeyTab, 61}, {ui.KeyBackspace, 67}, {ui.KeyEnter, 66}, {ui.KeyDelete, 112},
		{ui.KeyLeft, 21}, {ui.KeyRight, 22}, {ui.KeyUp, 19}, {ui.KeyDown, 20},
	}
	for _, p := range pairs {
		if shortcut(c, 0, p.key) {
			a.forwardInput(AndroidInput{Kind: "key", Action: "down", Keycode: p.code})
			a.forwardInput(AndroidInput{Kind: "key", Action: "up", Keycode: p.code})
		}
	}
}

// androidPoint maps a pointer event in the video surface onto the device's
// pixel grid, undoing the view's rotation and mirror flip.
func (a *mirrpaneApp) androidPoint(x, y float32, c *ui.Context) (int, int, bool) {
	availW, availH := c.Size()
	rawW, rawH := a.bitmap.Size()
	return androidMapPoint(x, y, availW, availH, float32(rawW), float32(rawH),
		((a.quarterTurns%4)+4)%4, a.prefs.FlipHorizontal, a.prefs.FillViewport)
}

// androidMapPoint is the pure geometry behind androidPoint, shared with its
// tests: the video is letterboxed (or cover-cropped) into availW×availH, the
// picture may be rotated clockwise by turn quarter turns and mirrored.
func androidMapPoint(x, y, availW, availH, rawW, rawH float32, turn int, flip, fill bool) (int, int, bool) {
	if rawW <= 0 || rawH <= 0 || availW <= 0 || availH <= 0 {
		return 0, 0, false
	}
	turn = ((turn % 4) + 4) % 4
	effW, effH := rawW, rawH
	if turn%2 == 1 {
		effW, effH = effH, effW
	}
	var fit float32
	if fill {
		fit = max32(availW/effW, availH/effH)
	} else {
		fit = min32(availW/effW, availH/effH)
	}
	dispW, dispH := effW*fit, effH*fit
	left, top := (availW-dispW)/2, (availH-dispH)/2
	u := (x - left) / dispW
	v := (y - top) / dispH
	if u < 0 {
		u = 0
	}
	if u > 1 {
		u = 1
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	// Undo the clockwise view rotation the picture is drawn with.
	var iu, iv float32
	switch turn {
	case 1:
		iu, iv = v, 1-u
	case 2:
		iu, iv = 1-u, 1-v
	case 3:
		iu, iv = 1-v, u
	default:
		iu, iv = u, v
	}
	if flip {
		iu = 1 - iu
	}
	dx := int(iu * rawW)
	dy := int(iv * rawH)
	if dx >= int(rawW) {
		dx = int(rawW) - 1
	}
	if dy >= int(rawH) {
		dy = int(rawH) - 1
	}
	if dx < 0 {
		dx = 0
	}
	if dy < 0 {
		dy = 0
	}
	return dx, dy, true
}

// androidPointerEvent converts one pointer event into touch injection.
func (a *mirrpaneApp) androidPointerEvent(ev ui.InputEvent, c *ui.Context) {
	x, y, ok := a.androidPoint(ev.X, ev.Y, c)
	if !ok {
		return
	}
	w, h := a.bitmap.Size()
	action := "move"
	switch ev.Kind {
	case ui.InputPointerDown:
		action = "down"
		a.androidPointer = true
	case ui.InputPointerUp:
		action = "up"
		a.androidPointer = false
	}
	a.forwardInput(AndroidInput{
		Kind: "touch", Action: action, X: x, Y: y, W: w, H: h,
		Pressure: float64(ev.Pressure),
	})
}

// androidScrollEvent forwards a wheel motion as a scrcpy scroll message. The
// API's units run from -16 to 16; a wheel notch is about that much. MyGo's DY
// grows down the content, while Android's vScroll is positive upwards, so the
// sign flips on the way.
func (a *mirrpaneApp) androidScrollEvent(ev ui.InputEvent, c *ui.Context) {
	x, y, ok := a.androidPoint(ev.X, ev.Y, c)
	if !ok {
		return
	}
	w, h := a.bitmap.Size()
	units := func(v float32) float64 {
		v /= 8
		if v > 16 {
			v = 16
		}
		if v < -16 {
			v = -16
		}
		return -float64(v)
	}
	a.forwardInput(AndroidInput{
		Kind: "scroll", X: x, Y: y, W: w, H: h,
		DX: units(ev.DX), DY: units(ev.DY),
	})
}

// buildAndroidModal lists adb devices and starts a scrcpy session.
func (a *mirrpaneApp) buildAndroidModal(c *ui.Context) {
	a.panelOverlay(c.Key("android-panel"), &a.showAndroid, 640, 580, func() {
		st := a.androidState()
		active := a.androidActive()
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			ui.Icon(c, icons.phone).Width(17).Height(17).TextColor(colAccent)
			ui.Text(c, "Android 裝置").FontSize(17).Bold().TextColor(colInk)
			ui.Text(c, "ADB").FontSize(11).TextColor(colFaint)
			ui.Spacer(c)
			if !active {
				if iconButton(c, "android-refresh", icons.refresh, "重新整理裝置", a.androidLoading, false).Clicked() {
					a.refreshAndroidDevices()
				}
			}
			if iconButton(c, "android-close", icons.close, "關閉 (Esc)", false, false).Clicked() {
				a.showAndroid = false
			}
		})

		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(12).PaddingEnd(4).Children(func() {
				if active {
					ui.Column(c).FillWidth().Padding(14).Gap(10).Radius(12).
						Background(colWell).Border(1, colHairline).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(9).Children(func() {
							if st != nil && st.Phase == "live" {
								statusDot(c, colOK, 8)
								ui.Text(c, "已連線").FontSize(13).Bold().TextColor(colOK)
							} else {
								ui.Spinner(c)
								ui.Text(c, "正在啟動 scrcpy 伺服器…").FontSize(13).Bold().TextColor(colAccent)
							}
							ui.Spacer(c)
							if st != nil && st.Phase == "live" {
								ui.Textf(c, "%d × %d", st.Width, st.Height).Font("Cascadia Mono").
									FontSize(11).TextColor(colFaint).FontFeatures("tnum")
							}
						})
						device := ""
						if st != nil {
							device = nonEmpty(st.Device, nonEmpty(st.Model, st.Serial))
						}
						ui.Text(c, nonEmpty(device, "Android 裝置")).FontSize(16).Bold().TextColor(colInk)
						ui.Text(c, "畫面會顯示在 MirrPane 視窗中；滑鼠與鍵盤會直接操作裝置。").
							FontSize(11.5).TextColor(colMuted)
						ui.Row(c).FillWidth().Justify(ui.End).Children(func() {
							softButton(c, "android-stop", icons.power, "中斷連線", true, a.disconnectAndroid)
						})
					})
					return
				}

				if a.androidErr != "" {
					ui.Row(c).FillWidth().Padding(10, 12).Radius(10).Gap(9).
						Background(colWarn.Alpha(0.1)).Border(1, colWarn.Alpha(0.3)).Children(func() {
						ui.Icon(c, icons.alert).Width(16).Height(16).TextColor(colWarn)
						ui.Column(c).Grow(1).Gap(3).Children(func() {
							ui.Text(c, a.androidErr).FontSize(11.5).TextColor(colWarn)
							ui.Text(c, "需要 Android Platform Tools（adb）。可執行 winget install Google.PlatformTools 安裝，或設定 MIRRPANE_ADB 指向 adb.exe。").
								FontSize(11).TextColor(colMuted)
						})
					})
				}

				sectionLabel(c, "USB 裝置")
				ui.Scroll(c).Height(170).FillWidth().Children(func() {
					ui.Column(c).FillWidth().Gap(6).Children(func() {
						if len(a.androidDevices) == 0 {
							ui.Text(c, "尚未偵測到裝置。請以 USB 連接手機，並在開發人員選項開啟「USB 偵錯」。").
								FontSize(11.5).TextColor(colMuted)
						}
						for i, dev := range a.androidDevices {
							dev := dev
							label, color := androidDeviceLabel(dev.State)
							ui.Row(c).FillWidth().Padding(9, 12).Radius(10).Gap(10).
								Background(colWell).Border(1, colHairline).AlignItems(ui.Center).Children(func() {
								ui.Icon(c, icons.phone).Width(16).Height(16).TextColor(colMuted)
								ui.Column(c).Grow(1).Gap(1).Children(func() {
									model := nonEmpty(dev.Model, dev.Serial)
									ui.Text(c, model).FontSize(13).Bold().TextColor(colInk).SingleLine()
									ui.Text(c, dev.Serial).Font("Cascadia Mono").FontSize(10.5).
										TextColor(colFaint).SingleLine()
								})
								pill(c, label, color)
								btn := ui.ButtonBase(c.Key(fmt.Sprintf("android-connect-%d", i))).
									Padding(7, 13).Radius(9)
								btn.Background(colCardBg).Border(1, colBorder).TextColor(colInk)
								if btn.Hovered() && dev.State == "device" {
									btn.Background(ui.RGBA(255, 244, 230, 0.085))
								}
								btn.Disabled(dev.State != "device")
								btn.Children(func() {
									ui.Text(c, "連線").FontSize(12.5)
								})
								if btn.Clicked() && dev.State == "device" {
									a.connectAndroid(dev.Serial)
								}
							})
						}
					})
				})
				ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
					if softButton(c, "android-scan", icons.refresh, "重新整理", false, a.refreshAndroidDevices) {
					}
					ui.Text(c, "adb devices").Font("Cascadia Mono").FontSize(10.5).TextColor(colFaint)
				})

				ui.Box(c).FillWidth().Height(1).Background(colHairline).MarginY(2)
				sectionLabel(c, "無線偵錯")
				ui.Row(c).FillWidth().Gap(8).Children(func() {
					ui.TextInput(c.Key("android-addr"), &a.androidAddress).Placeholder("192.168.1.50:5555").Grow(1)
					ui.TextInput(c.Key("android-code"), &a.androidPairCode).Placeholder("配對碼").Width(110)
				})
				ui.Row(c).FillWidth().Gap(8).Children(func() {
					if softButton(c, "android-pair", icons.check, "配對", false, a.pairAndroidWireless) {
					}
					if softButton(c, "android-wireless", icons.phone, "無線連線", false, a.connectAndroidWireless) {
					}
					ui.Spacer(c)
					ui.Text(c, "Android 11+：無線偵錯 → 使用配對碼").
						FontSize(10.5).TextColor(colFaint)
				})
			})
		})
	})
}

// panelOverlay draws a centred modal panel above the app and runs fn inside
// it. Context.Size and element positions both use logical pixels.
func (a *mirrpaneApp) panelOverlay(c *ui.Context, open *bool, width, height float32, fn func()) {
	availW, availH := c.Size()
	ui.DialogBase(c, open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, 0.5))
		panel.Width(min(width, availW-32)).Height(min(height, availH-32)).
			Padding(18).Gap(12).Radius(22).Background(colPanel).
			Border(1, colBorder).InsetShadow(0, 1, 0, 0, colRim).
			Shadow(0, 30, 80, 0, ui.RGBA(0, 0, 0, 0.6))
		fn()
	})
}

func (a *mirrpaneApp) buildLogsModal(c *ui.Context) {
	a.panelOverlay(c.Key("logs-panel"), &a.showLogs, 860, 560, func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			ui.Icon(c, icons.logs).Width(17).Height(17).TextColor(colAccent)
			ui.Text(c, "接收器日誌").FontSize(17).Bold().TextColor(colInk)
			ui.Text(c, a.sidecar.LogPath()).Font("Cascadia Mono").FontSize(11).
				TextColor(colFaint).SingleLine().Grow(1)
			if softButton(c, "logs-copy", icons.copy, "複製全部", false, func() {
				if len(a.logLines) == 0 {
					a.logLines = a.sidecar.LogTail(40)
				}
				c.WriteClipboard(strings.Join(a.logLines, "\r\n"))
				c.Toast("已複製日誌內容")
			}) {
			}
			if iconButton(c, "logs-close", icons.close, "關閉 (Esc)", false, false).Clicked() {
				a.showLogs = false
			}
		})
		ui.Scroll(c).Grow(1).FillWidth().Padding(14).Radius(10).
			Background(colGround).Border(1, colHairline).Children(func() {
			ui.Column(c).Gap(2).Children(func() {
				if len(a.logLines) == 0 {
					ui.Text(c, "（目前沒有日誌記錄）").Font("Cascadia Mono").FontSize(11.5).TextColor(colFaint)
				} else {
					for i, line := range a.logLines {
						ui.Text(c.Key(i), line).Font("Cascadia Mono").FontSize(11).
							TextColor(colMuted).SingleLine()
					}
				}
			})
		})
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			if softButton(c, "logs-refresh", icons.refresh, "重新整理", false, func() {
				a.logLines = a.sidecar.LogTail(40)
			}) {
			}
			if softButton(c, "logs-open-dir", icons.folder, "開啟資料夾", false, func() {
				if err := openFolder(filepath.Dir(a.sidecar.LogPath())); err != nil {
					a.pendingToast = "無法開啟資料夾：" + err.Error()
				}
			}) {
			}
			ui.Spacer(c)
			ui.Textf(c, "%d 行", len(a.logLines)).Font("Cascadia Mono").FontSize(11).TextColor(colFaint)
			softButton(c, "logs-done", nil, "關閉", false, func() { a.showLogs = false })
		})
	})
}

// settingsTabNames are the settings dialog's tabs, in order.
var settingsTabNames = []string{"一般", "圖形與解碼", "畫面", "音訊"}

var settingsTabIcons = []*ui.SVG{icons.settings, icons.display, icons.image, icons.volume}

// settingsDirty reports whether the dialog holds restart-required edits.
func (a *mirrpaneApp) settingsDirty() bool {
	return a.configDraft != a.initialConfig
}

// settingsNavItem is one row of the settings sidebar.
func settingsNavItem(c *ui.Context, key any, ic *ui.SVG, label string, selected, dirty bool, onClick func()) {
	row := ui.ButtonBase(c.Key(key)).FillWidth().Padding(7, 11).Radius(999).Gap(8)
	if selected {
		row.Background(colAccent.Alpha(0.14)).TextColor(colAccent)
	} else if row.Hovered() {
		row.Background(colHover).TextColor(colInk)
	} else {
		row.TextColor(colMuted)
	}
	row.Children(func() {
		ui.Icon(c, ic).Width(15).Height(15)
		ui.Text(c, label).FontSize(12.5).Grow(1)
		if dirty {
			ui.Box(c).Width(6).Height(6).Radius(3).Background(colAccent)
		}
	})
	if row.Clicked() && onClick != nil {
		onClick()
	}
}

func (a *mirrpaneApp) buildSettingsModal(c *ui.Context) {
	a.panelOverlay(c.Key("settings-panel"), &a.showSettings, 760, 600, func() {
		availW, _ := c.Size()
		wide := availW >= 760
		dirty := a.settingsDirty()

		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			ui.Icon(c, icons.settings).Width(17).Height(17).TextColor(colAccent)
			ui.Text(c, "設定").FontSize(17).Bold().TextColor(colInk)
			ui.Text(c, "v"+appVersion).FontSize(11).TextColor(colFaint)
			ui.Spacer(c)
			if iconButton(c, "set-close", icons.close, "關閉 (Esc)", false, false).Clicked() {
				a.showSettings = false
			}
		})

		if a.settingsTab < 0 || a.settingsTab >= len(settingsTabNames) {
			a.settingsTab = 0
		}

		builder := func() {
			ui.Column(c).FillWidth().Gap(14).Padding(2, 0).Children(func() {
				switch a.settingsTab {
				case 0:
					a.buildSettingsGeneral(c)
				case 1:
					a.buildSettingsGraphics(c)
				case 2:
					a.buildSettingsRender(c)
				case 3:
					a.buildSettingsAudio(c)
				}
			})
		}

		if wide {
			ui.Row(c).Grow(1).FillWidth().Gap(16).AlignItems(ui.Stretch).Children(func() {
				// Left rail: a studio panel switcher with a dirty marker on
				// the tabs that hold restart-required settings.
				ui.Column(c).Width(158).Padding(6).Gap(3).Background(colWell).
					Border(1, colHairline).Radius(12).Children(func() {
					for i, name := range settingsTabNames {
						idx := i
						tabDirty := dirty && idx != 2
						settingsNavItem(c, "set-nav-"+strconv.Itoa(i), settingsTabIcons[i], name,
							a.settingsTab == i, tabDirty, func() { a.settingsTab = idx })
					}
				})
				ui.Scroll(c).Grow(1).FillWidth().PaddingEnd(4).Children(builder)
			})
		} else {
			ui.Tabs(c.Key("set-tabs"), &a.settingsTab, settingsTabNames...)
			ui.Scroll(c).Grow(1).FillWidth().PaddingEnd(4).Children(builder)
		}

		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			if dirty {
				ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
					statusDot(c, colWarn, 6)
					ui.Text(c, "已修改需重啟的設定").FontSize(11.5).TextColor(colWarn)
				})
			} else {
				ui.Text(c, "即時偏好已自動儲存").FontSize(11.5).TextColor(colFaint)
			}
			ui.Spacer(c)
			if dirty {
				softButton(c, "set-cancel", nil, "放棄變更", false, func() { a.showSettings = false })
				primaryButton(c, "set-save", icons.check, "套用並重啟", a.saveConfigAndRestart)
			} else {
				primaryButton(c, "set-done", icons.check, "完成", func() { a.showSettings = false })
			}
		})
	})
}

// snapStateVersion is the receiver's AirPlay version for the settings rail.
func (a *mirrpaneApp) snapStateVersion() string {
	if st := a.snap.State; st != nil {
		return st.Version
	}
	return ""
}

// settingRow lays out a labelled setting with its control on the right.
func settingRow(c *ui.Context, label, desc string, control func()) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(16).Children(func() {
		ui.Column(c).Grow(1).Gap(2).Children(func() {
			ui.Text(c, label).FontSize(13).TextColor(colInk)
			if desc != "" {
				ui.Text(c, desc).FontSize(11).TextColor(colMuted)
			}
		})
		control()
	})
}

// switchRow is a settingRow with a switch.
func switchRow(c *ui.Context, key any, label, desc string, v *bool) bool {
	changed := false
	settingRow(c, label, desc, func() {
		if ui.Switch(c.Key(key), v).Changed() {
			changed = true
		}
	})
	return changed
}

// selectField is a settingRow with a dropdown.
func selectField(c *ui.Context, key any, label, desc string, value *string, options []string) bool {
	changed := false
	settingRow(c, label, desc, func() {
		if ui.Select(c.Key(key), value, options).Width(236).Changed() {
			changed = true
		}
	})
	return changed
}

// segmentedField is a settingRow with a segmented control.
func segmentedField(c *ui.Context, key any, label, desc string, idx *int, labels ...string) bool {
	changed := false
	settingRow(c, label, desc, func() {
		if ui.Segmented(c.Key(key), idx, labels...).Changed() {
			changed = true
		}
	})
	return changed
}

// indexOf returns the index of v in list, or 0.
func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return 0
}

func (a *mirrpaneApp) buildSettingsGeneral(c *ui.Context) {
	sectionLabel(c, "接收器")
	settingRow(c, "接收名稱", "", func() {
		ui.TextInput(c.Key("set-name-in"), &a.configDraft.name).Width(236)
	})
	if switchRow(c, "set-approval", "首次連線需人工核准",
		"關閉時，已配對的裝置第一次連線就自動加入信任名單", &a.configDraft.requireApproval) {
	}

	ui.Box(c).FillWidth().Height(1).Background(colHairline).MarginY(2)
	sectionLabel(c, "連線資訊")
	if st := a.snap.State; st != nil {
		infoRow(c, "區域網路位址", fmt.Sprintf("%s:%d", nonEmpty(st.IP, "-"), st.Port))
		infoRow(c, "當前解碼引擎", nonEmpty(st.Decoder, "待機"))

		infoRow(c, "協定版本", "AirPlay "+nonEmpty(st.Version, "-"))
	}

	ui.Box(c).FillWidth().Height(1).Background(colHairline).MarginY(2)
	sectionLabel(c, "維護")
	ui.Row(c).FillWidth().Gap(8).Wrap().Children(func() {
		if softButton(c, "set-open-shots", icons.image, "截圖資料夾", false, a.openScreenshots) {
		}
		if softButton(c, "set-open-dir", icons.folder, "程式資料夾", false, a.openInstallDir) {
		}
		if softButton(c, "set-reset-trust", icons.close, "清除信任裝置", false, func() {
			go func() {
				if err := a.sidecar.ResetTrustedSenders(); err != nil {
					a.toastAsync("清除失敗：" + err.Error())
				} else {
					a.toastAsync("已清除所有信任裝置")
				}
			}()
		}) {
		}
		if softButton(c, "set-reset-id", icons.power, "重設配對身分", true, func() {
			if a.restartReceiver(func() error { return ResetIdentity(a.installDir) }, "接收器已重設配對身分") {
				a.showSettings = false
			} else {
				a.pendingToast = "接收器正在重新啟動，請稍候再操作"
			}
		}) {
		}
	})
}

// configOptionLists map the config.json values onto the labels the settings
// dialog offers.
var (
	decoderModes   = []string{"auto", "software"}
	decoderLabels  = []string{"自動（硬體）", "軟體解碼"}
	colorRanges    = []string{"auto", "full", "limited"}
	colorLabels    = []string{"自動", "完整", "有限"}
	scaleFilters   = []string{"fast", "bilinear", "bicubic"}
	scaleLabels    = []string{"快速", "雙線性", "雙三次"}
	audioBuffers   = []string{"low", "balanced", "stable"}
	audioBufLabels = []string{"低延遲", "平衡", "穩定"}
)

func (a *mirrpaneApp) buildSettingsGraphics(c *ui.Context) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(7).Children(func() {
		statusDot(c, colWarn, 6)
		ui.Text(c, "這些設定會在接收器重新啟動後生效").FontSize(11.5).TextColor(colWarn)
	})

	sectionLabel(c, "虛擬顯示器")
	selectField(c, "set-res", "解析度", "",
		&a.configDraft.resKey, resolutionOrder)
	fps := strconv.Itoa(a.configDraft.maxFps)
	fpsIdx := 0
	if fps == "30" {
		fpsIdx = 1
	}
	if segmentedField(c, "set-fps", "宣告幀率上限", "",
		&fpsIdx, "60 fps", "30 fps") {
		if fpsIdx == 1 {
			a.configDraft.maxFps = 30
		} else {
			a.configDraft.maxFps = 60
		}
	}

	sectionLabel(c, "解碼與色彩")
	decIdx := indexOf(decoderModes, a.configDraft.decoderMode)
	if segmentedField(c, "set-dec", "H.264 解碼", "",
		&decIdx, decoderLabels...) {
		a.configDraft.decoderMode = decoderModes[decIdx]
	}
	rangeIdx := indexOf(colorRanges, a.configDraft.colorRange)
	if segmentedField(c, "set-range", "輸入色彩範圍", "",
		&rangeIdx, colorLabels...) {
		a.configDraft.colorRange = colorRanges[rangeIdx]
	}
	filterIdx := indexOf(scaleFilters, a.configDraft.scaleFilter)
	if segmentedField(c, "set-filter", "YUV → RGBA 轉換", "",
		&filterIdx, scaleLabels...) {
		a.configDraft.scaleFilter = scaleFilters[filterIdx]
	}
}

func (a *mirrpaneApp) buildSettingsRender(c *ui.Context) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(7).Children(func() {
		statusDot(c, colOK, 6)
		ui.Text(c, "這些設定即時生效").FontSize(11.5).TextColor(colOK)
	})

	sectionLabel(c, "構圖")
	fill := a.prefs.FillViewport
	if switchRow(c, "set-fill", "填滿視窗（Z）", "", &fill) {
		next := a.prefs
		next.FillViewport = fill
		a.savePrefs(next)
	}
	flip := a.prefs.FlipHorizontal
	if switchRow(c, "set-flip", "水平翻轉（H）", "", &flip) {
		next := a.prefs
		next.FlipHorizontal = flip
		a.savePrefs(next)
	}
	quality := samplingChoice(a.prefs.FilterQuality)
	qualityIdx := indexOf(samplingChoices, quality)
	if segmentedField(c, "set-qual", "縮放取樣", "",
		&qualityIdx, samplingChoices...) {
		next := a.prefs
		next.FilterQuality = filterQualityForChoice(samplingChoices[qualityIdx])
		a.savePrefs(next)
	}

	sectionLabel(c, "工具列與 HUD")
	hud := a.prefs.ShowHud
	if switchRow(c, "set-hud", "顯示效能 HUD（Tab）", "", &hud) {
		next := a.prefs
		next.ShowHud = hud
		a.savePrefs(next)
	}
	cornerIdx := indexOf(allowedHudCorners, a.prefs.HudCorner)
	if segmentedField(c, "set-corner", "HUD 位置", "", &cornerIdx, "左上", "右上", "左下", "右下") {
		next := a.prefs
		next.HudCorner = allowedHudCorners[cornerIdx]
		a.savePrefs(next)
	}
	hideIdx := indexOf([]string{"0", "2", "4"}, strconv.Itoa(a.prefs.BarHideSeconds))
	if segmentedField(c, "set-hide", "工具列自動隱藏", "",
		&hideIdx, "不隱藏", "2 秒", "4 秒") {
		if v, err := strconv.Atoi([]string{"0", "2", "4"}[hideIdx]); err == nil {
			next := a.prefs
			next.BarHideSeconds = v
			a.savePrefs(next)
		}
	}

	sectionLabel(c, "色彩")
	brightness := a.prefs.Brightness
	settingRow(c, "亮度", fmt.Sprintf("%+.2f", brightness), func() {
		if ui.Slider(c.Key("set-bri"), &brightness, -0.3, 0.3).Width(236).Changed() {
			next := a.prefs
			next.Brightness = brightness
			a.savePrefs(next)
		}
	})
	contrast := a.prefs.Contrast
	settingRow(c, "對比", fmt.Sprintf("%.2f", contrast), func() {
		if ui.Slider(c.Key("set-con"), &contrast, 0.7, 1.4).Width(236).Changed() {
			next := a.prefs
			next.Contrast = contrast
			a.savePrefs(next)
		}
	})
	saturation := a.prefs.Saturation
	settingRow(c, "飽和度", fmt.Sprintf("%.2f", saturation), func() {
		if ui.Slider(c.Key("set-sat"), &saturation, 0.5, 1.5).Width(236).Changed() {
			next := a.prefs
			next.Saturation = saturation
			a.savePrefs(next)
		}
	})

	ui.Row(c).FillWidth().Children(func() {
		softButton(c, "set-reset-prefs", icons.refresh, "重設畫面偏好", false, func() {
			next := DefaultUIPrefs()
			next.Muted, next.Volume = a.prefs.Muted, a.prefs.Volume
			a.savePrefs(next)
			c.Toast("已重設畫面偏好")
		})
	})
}

func (a *mirrpaneApp) buildSettingsAudio(c *ui.Context) {
	sectionLabel(c, "播放")
	vol := a.prefs.Volume
	settingRow(c, "音量", fmt.Sprintf("%.0f%%", vol*100), func() {
		if ui.Slider(c.Key("set-vol"), &vol, 0, 1).Width(236).Changed() {
			next := a.prefs
			next.Volume = vol
			next.Muted = vol <= 0.001
			a.savePrefs(next)
		}
	})
	muted := a.prefs.Muted
	if switchRow(c, "set-mute", "靜音輸出", "保留音量設定，暫時關閉播放（M）", &muted) {
		next := a.prefs
		next.Muted = muted
		a.savePrefs(next)
	}

	sectionLabel(c, "延遲與緩衝")
	bufIdx := indexOf(audioBuffers, a.configDraft.audioBuffer)
	if segmentedField(c, "set-buf", "抗抖動緩衝", "重新啟動後生效",
		&bufIdx, audioBufLabels...) {
		a.configDraft.audioBuffer = audioBuffers[bufIdx]
	}
}

// infoRow lays out one read-only telemetry line.
func infoRow(c *ui.Context, label, value string) {
	ui.Row(c).FillWidth().Children(func() {
		ui.Text(c, label).FontSize(12.5).TextColor(colMuted)
		ui.Spacer(c)
		ui.Text(c, value).Font("Cascadia Mono").FontSize(12.5).TextColor(colInk)
	})
}

// dispatchUI runs fn on the window's UI thread, or here when the app runs
// without a window (tests, or the moment after teardown). Inside the window it
// goes through Update so UI state is never mutated from a worker goroutine.
func (a *mirrpaneApp) dispatchUI(fn func()) {
	win := a.win
	if win == nil || win.IsDestroyed() {
		fn()
		return
	}
	win.Update(fn)
}

func (a *mirrpaneApp) toastAsync(msg string) {
	if a.closed.Load() {
		return
	}
	a.dispatchUI(func() { a.pendingToast = msg })
}

func pinLabel(pinned bool) string {
	if pinned {
		return "取消置頂"
	}
	return "視窗置頂"
}

func fullscreenLabel(fs bool) string {
	if fs {
		return "退出全螢幕"
	}
	return "全螢幕"
}

func flipLabel(on bool) string {
	if on {
		return "翻轉：開"
	}
	return "翻轉：關"
}

func fillLabel(on bool) string {
	if on {
		return "填滿：開"
	}
	return "填滿：關"
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func shortFingerprint(fp string) string {
	if len(fp) <= 16 {
		return fp
	}
	return fp[:8] + "…" + fp[len(fp)-8:]
}

func openFolder(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
