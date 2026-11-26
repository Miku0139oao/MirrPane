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
	"github.com/egoist/mygo/ui"
)

// MirrPane's desktop colour palette.
var (
	colGround    = ui.Hex("#191817")
	colSurface   = ui.Hex("#211F1D")
	colSurfaceHi = ui.Hex("#2C2926")
	colBorder    = ui.RGBA(255, 255, 255, 0.12)
	colInk       = ui.Hex("#F3EFE9")
	colMuted     = ui.Hex("#A6A099")
	colAccent    = ui.Hex("#D08A3E")
	colAccentHov = ui.Hex("#DDA05C")
	colAccentPrs = ui.Hex("#B8752E")
	colOK        = ui.Hex("#6BBF7B")
	colDanger    = ui.Hex("#D4685C")
	colWarn      = ui.Hex("#E3A857")
	colOverlayBg = ui.RGBA(25, 24, 23, 0.84)
	colHudBg     = ui.RGBA(18, 17, 16, 0.78)
)

func mirrpaneTheme() *ui.Theme {
	t := ui.DarkTheme()
	t.Background = colGround
	t.Surface = colSurface
	t.SurfaceHover = colSurfaceHi
	t.SurfacePressed = ui.Hex("#36322E")
	t.Border = colBorder
	t.Text = colInk
	t.TextMuted = colMuted
	t.Accent = colAccent
	t.AccentHover = colAccentHov
	t.AccentPressed = colAccentPrs
	t.AccentText = colGround
	t.Danger = colDanger
	t.Warning = colWarn
	t.Success = colOK
	t.Focus = ui.RGBA(208, 138, 62, 0.65)
	t.Radius = 8
	return t
}

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
	name        string
	resKey      string
	maxFps      int
	decoderMode string
	colorRange  string
	scaleFilter string
	audioBuffer string
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
	a.workers.Add(1)
	go a.writePreferences()
	return a
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
	a.snap = snap
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
	if a.prefs.BarHideSeconds > 0 && !a.barHovered && !a.barFocused &&
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
	return interrupted || a.barVisible || a.prefs.BarHideSeconds == 0 || audioOnly(st)
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
		case a.showSettings, a.showLogs:
			a.showSettings, a.showLogs = false, false
		case a.fullscreen:
			a.setFullScreen(false)
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
	if !a.showSettings && !a.showLogs && !c.FocusTakesText() && a.streaming() {
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
		if shortcut(c, 0, ui.KeyTab) {
			a.toggleHud()
		}
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
}

func (a *mirrpaneApp) buildHeaderBar(c *ui.Context, title string, badge string, badgeColor ui.Color) {
	ui.Row(c).FillWidth().Padding(12, 16).Gap(10).AlignItems(ui.Center).
		Background(colSurface).BorderWidth(0, 0, 1, 0).BorderColor(colBorder).Children(func() {
		ui.Text(c, title).FontSize(15).Bold().TextColor(colInk)
		if badge != "" {
			pill(c, badge, badgeColor)
		}
		ui.Spacer(c)
		if ui.Button(c.Key("hdr-pin"), pinLabel(a.alwaysOnTop)).Clicked() {
			a.toggleAlwaysOnTop()
		}
		if ui.Button(c.Key("hdr-fs"), fullscreenLabel(a.fullscreen)).Clicked() {
			a.toggleFullScreen()
		}
		if ui.Button(c.Key("hdr-logs"), "日誌").Clicked() {
			a.openLogs()
		}
		if ui.Button(c.Key("hdr-settings"), "設定").Clicked() {
			a.openSettings()
		}
	})
}

func (a *mirrpaneApp) buildBootingScreen(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		a.buildHeaderBar(c, "鏡屏 MirrPane", "啟動中", colWarn)
		body := ui.Column(c).Grow(1).FillWidth().Center().Gap(16).Padding(24)
		body.Children(func() {
			ui.Column(c).Width(460).Padding(28).Gap(16).Radius(12).Background(colSurface).
				Border(1, colBorder).AlignItems(ui.Center).Children(func() {
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c)
					ui.Text(c, "正在啟動 MirrPane 接收器").FontSize(18).Bold()
				})
				msg := a.snap.Message
				if msg == "" {
					msg = "正在初始化…"
				}
				ui.Text(c, msg).TextColor(colMuted).TextAlign(ui.Center)
				ui.Row(c).Gap(10).Children(func() {
					if ui.Button(c.Key("boot-logs"), "查看日誌").Clicked() {
						a.openLogs()
					}
					if ui.Button(c.Key("boot-restart"), "重新啟動").Clicked() {
						a.restartReceiver(nil, "")
					}
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

	ui.Column(c).Fill().Children(func() {
		a.buildHeaderBar(c, "鏡屏 MirrPane", "待機中", colOK)
		body := ui.Column(c).Grow(1).FillWidth().Center().Gap(20).Padding(24)
		body.Children(func() {
			// Pending sender approval card, shown when an unapproved device verifies.
			if p := st.PendingSender; p != nil {
				ui.Column(c).Width(560).Padding(20).Gap(12).Radius(12).Background(colSurfaceHi).
					Border(1, colAccent).Children(func() {
					ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
						pill(c, "待核准裝置", colAccent)
						ui.Text(c, nonEmpty(p.Label, nonEmpty(p.Remote, "未知裝置"))).FontSize(16).Bold()
						ui.Spacer(c)
						ui.Textf(c, "%d 秒前", p.AgeSeconds).FontSize(12).TextColor(colMuted)
					})
					ui.Textf(c, "來源：%s   指紋：%s", nonEmpty(p.Remote, "-"), shortFingerprint(p.Fingerprint)).
						Font("Consolas").FontSize(12).TextColor(colMuted)
					fp := p.Fingerprint
					ui.Row(c).Gap(10).Justify(ui.End).FillWidth().Children(func() {
						if ui.Button(c.Key("reject-sender"), "拒絕").Clicked() {
							go func() {
								if err := a.sidecar.RejectSender(fp); err != nil {
									a.toastAsync("拒絕失敗：" + err.Error())
								} else {
									a.toastAsync("已拒絕連線請求")
								}
							}()
						}
						if ui.PrimaryButton(c.Key("approve-sender"), "允許連線").Clicked() {
							go func() {
								if err := a.sidecar.ApproveSender(fp); err != nil {
									a.toastAsync("核准失敗：" + err.Error())
								} else {
									a.toastAsync("已核准，請在裝置上再次選取接收器")
								}
							}()
						}
					})
				})
				return
			}

			// Main waiting receiver card.
			ui.Column(c).Width(560).Padding(28).Gap(18).Radius(14).Background(colSurface).
				Border(1, colBorder).Shadow(0, 12, 32, 0, ui.RGBA(0, 0, 0, 0.35)).Children(func() {
				ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Children(func() {
					ui.Column(c).Grow(1).Gap(4).Children(func() {
						ui.Text(c, "AirPlay 接收名稱").FontSize(12).TextColor(colMuted)
						name := st.Name
						if name == "" {
							name = "鏡屏"
						}
						ui.Text(c, name).FontSize(28).Bold().TextColor(colInk)
					})
					pill(c, "等待手機或平板連線", colOK)
				})

				addr := fmt.Sprintf("%s:%d", nonEmpty(st.IP, "127.0.0.1"), st.Port)
				ui.Row(c).FillWidth().Padding(12, 14).Radius(10).Background(colSurfaceHi).
					Border(1, colBorder).AlignItems(ui.Center).Gap(10).Children(func() {
					ui.Column(c).Grow(1).Gap(2).Children(func() {
						ui.Text(c, "本機位址").FontSize(11).TextColor(colMuted)
						ui.Text(c, addr).Font("Consolas").FontSize(16).Bold().TextColor(colInk)
					})
					if ui.Button(c.Key("copy-addr"), "複製位址").Clicked() {
						c.WriteClipboard(addr)
						c.Toast("已複製 " + addr)
					}
				})

				ui.Row(c).FillWidth().Gap(8).Wrap().Children(func() {
					if ui.Button(c.Key("wait-rescan"), "重新廣播").Clicked() {
						a.restartReceiver(nil, "接收器已重新啟動")
					}
					if ui.Button(c.Key("wait-shots"), "開啟截圖資料夾").Clicked() {
						a.openScreenshots()
					}
				})

				// Target resolution, decoder, and AirPlay version badges.
				ui.Row(c).Gap(8).Wrap().Children(func() {
					resText := fmt.Sprintf("目標 %dx%d @ %d fps", st.TargetWidth, st.TargetHeight, st.MaxFPS)
					ui.Badge(c, resText)
					dec := nonEmpty(st.Decoder, st.DecoderMode)
					ui.Badge(c, "解碼 "+nonEmpty(dec, "auto"))
					ver := nonEmpty(st.Version, "AirPlay")
					ui.Badge(c, "版本 "+ver)
				})

				ui.Text(c, "在 iPhone / iPad 的「控制中心」點選「螢幕鏡像輸出」，選擇上方接收名稱即可開始投影。").
					FontSize(13).TextColor(colMuted)
			})
		})
	})
}

func (a *mirrpaneApp) buildConnectingScreen(c *ui.Context) {
	st := a.snap.State
	ui.Column(c).Fill().Children(func() {
		a.buildHeaderBar(c, "鏡屏 MirrPane", "連線中", colAccent)
		body := ui.Column(c).Grow(1).FillWidth().Center().Padding(24)
		body.Children(func() {
			ui.Column(c).Width(440).Padding(28).Gap(16).Radius(12).Background(colSurface).
				Border(1, colBorder).AlignItems(ui.Center).Children(func() {
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c)
					ui.Text(c, "正在建立鏡像串流").FontSize(18).Bold()
				})
				if st != nil && st.Sender != "" {
					ui.Textf(c, "來源裝置：%s", st.Sender).TextColor(colInk)
				}
				if st != nil && st.Reason != "" {
					ui.Text(c, st.Reason).FontSize(13).TextColor(colMuted)
				}
				if ui.Button(c.Key("cancel-connect"), "中斷連線").Clicked() {
					a.disconnect()
				}
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
			if ev.Kind == ui.InputPointerDown && ev.Clicks >= 2 && !a.barHovered {
				a.toggleFullScreen()
			}
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
			ui.Column(c).Absolute().Top(0).Left(0).Right(0).Padding(10, 14).ZIndex(60).Children(func() {
				a.buildLiveTopBar(c, st)
			})
		}

		// Audio-only sessions have no picture; name the sender on the empty
		// surface so a live audio stream does not look like a missing video.
		if !interrupted && audioOnly(st) {
			ui.Column(c).Absolute().Top(0).Left(0).Right(0).Bottom(0).Center().PassThrough().
				Children(func() {
					ui.Column(c).Width(440).Padding(24).Gap(10).Radius(14).Background(colOverlayBg).
						Border(1, colBorder).AlignItems(ui.Center).Children(func() {
						pill(c, "僅音訊", colAccent)
						ui.Textf(c, "來源裝置：%s", nonEmpty(st.Sender, "AirPlay")).
							FontSize(16).Bold().TextColor(colInk)
						ui.Text(c, "發送端正在播放音訊，沒有視訊畫面。").
							FontSize(13).TextColor(colMuted).TextAlign(ui.Center)
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
				Background(ui.RGBA(0, 0, 0, 0.55)).Children(func() {
				ui.Column(c).Width(440).Padding(26).Gap(14).Radius(14).Background(colSurface).
					Border(1, colWarn).Shadow(0, 16, 40, 0, ui.RGBA(0, 0, 0, 0.5)).
					AlignItems(ui.Center).Children(func() {
					pill(c, "串流中斷", colWarn)
					ui.Text(c, "畫面暫時中斷").FontSize(20).Bold()
					reason := nonEmpty(st.Reason, "等待傳送端恢復串流或重新連線…")
					ui.Text(c, reason).FontSize(13).TextColor(colMuted).TextAlign(ui.Center)
					ui.Row(c).Gap(10).Children(func() {
						if a.fullscreen {
							if ui.Button(c.Key("int-fs-exit"), "離開全螢幕").Clicked() {
								a.setFullScreen(false)
							}
						}
						if ui.Button(c.Key("int-restart"), "重啟接收器").Clicked() {
							a.restartReceiver(nil, "")
						}
						if ui.PrimaryButton(c.Key("int-disconnect"), "返回等待畫面").Clicked() {
							a.disconnect()
						}
					})
				})
			})
		}
	})
}

func (a *mirrpaneApp) buildLiveTopBar(c *ui.Context, st *ReceiverState) {
	displayFPS := st.FPS
	if displayFPS <= 0 {
		displayFPS = a.uiFPS
	}
	resText := fmt.Sprintf("%dx%d", st.Width, st.Height)
	if st.Width <= 0 || st.Height <= 0 {
		bw, bh := a.bitmap.Size()
		resText = fmt.Sprintf("%dx%d", bw, bh)
	}
	decText := nonEmpty(st.Decoder, nonEmpty(st.DecoderMode, "auto"))

	bar := ui.Column(c).FillWidth().Padding(8, 12).Gap(6).Radius(10).Background(colOverlayBg).
		Border(1, colBorder)
	// Hovered and FocusWithin are read as the bar builds: mygo rebuilds when
	// the pointer comes over it, leaves, or the keyboard focus changes, and
	// while a control holds either the hide timer stays paused.
	hovered, focused := bar.Hovered(), bar.FocusWithin()
	a.barHovered, a.barFocused = hovered, focused
	if hovered || focused {
		a.barLastMove = time.Now()
	}
	bar.Children(func() {
		// Status line: sender and stream quality.
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, nonEmpty(st.Sender, "AirPlay")).Bold().FontSize(14).SingleLine()
			if audioOnly(st) {
				ui.Badge(c, "僅音訊")
			} else {
				ui.Badge(c, resText)
				ui.Badge(c, fmt.Sprintf("%.0f fps", displayFPS))
				ui.Badge(c, decText)
			}
			ui.Spacer(c)
		})

		// Control line: audio and picture actions.
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			muteText := "靜音"
			if a.prefs.Muted {
				muteText = "已靜音"
			}
			if ui.Button(c.Key("live-mute"), muteText).Clicked() {
				a.toggleMute()
			}

			vol := a.prefs.Volume
			if ui.Slider(c.Key("live-vol"), &vol, 0, 1).Width(96).Changed() {
				next := a.prefs
				next.Volume = vol
				next.Muted = vol <= 0.001
				a.savePrefs(next)
			}

			if ui.Button(c.Key("live-snap"), "截圖").Clicked() {
				a.takeSnapshot()
			}
			if ui.Button(c.Key("live-rot"), "旋轉").Clicked() {
				a.rotateRight()
			}
			if ui.Button(c.Key("live-flip"), flipLabel(a.prefs.FlipHorizontal)).Clicked() {
				a.toggleFlip()
			}
			if ui.Button(c.Key("live-fill"), fillLabel(a.prefs.FillViewport)).Clicked() {
				a.toggleFill()
			}
		})

		// Third line: window and receiver controls.
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			hudLabel := "HUD 關"
			if a.prefs.ShowHud {
				hudLabel = "HUD 開"
			}
			if ui.Button(c.Key("live-hud"), hudLabel).Clicked() {
				a.toggleHud()
			}
			if ui.Button(c.Key("live-pin"), pinLabel(a.alwaysOnTop)).Clicked() {
				a.toggleAlwaysOnTop()
			}
			if ui.Button(c.Key("live-fs"), fullscreenLabel(a.fullscreen)).Clicked() {
				a.toggleFullScreen()
			}
			if ui.Button(c.Key("live-logs"), "日誌").Clicked() {
				a.openLogs()
			}
			if ui.Button(c.Key("live-settings"), "設定").Clicked() {
				a.openSettings()
			}
			if ui.Button(c.Key("live-disc"), "中斷").TextColor(colDanger).Clicked() {
				a.disconnect()
			}
		})
	})
}

func (a *mirrpaneApp) buildHudOverlay(c *ui.Context, st *ReceiverState, showBar bool) {
	bw, bh := a.bitmap.Size()
	w, h := st.Width, st.Height
	if w <= 0 || h <= 0 {
		w, h = bw, bh
	}

	// Position all four corners in the view's logical coordinates. The bar's
	// visibility decides the top offset, so an interrupted stream that forces
	// the bar on cannot leave the HUD underneath it.
	const cardH = float32(78)
	_, viewH := c.Size()
	topPad := float32(142) // clear all three toolbar rows
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

	card := ui.Column(c).Absolute().Top(top).Width(480).PassThrough().
		Padding(10, 12).Gap(3).Radius(8).Background(colHudBg).Border(1, colBorder).ZIndex(50)
	if a.prefs.HudCorner == "topRight" || a.prefs.HudCorner == "bottomRight" {
		card.Right(14)
	} else {
		card.Left(14)
	}
	card.Children(func() {
		ui.Textf(c, "RES %dx%d  UI %.1f fps  DEC %.1f fps (%s)",
			w, h, a.uiFPS, st.FPS, nonEmpty(st.Decoder, st.DecoderMode)).
			Font("Consolas").FontSize(12).TextColor(colInk)
		ui.Textf(c, "DECODE avg %.1f ms (p95 %.1f)  LAG %d  DROP %d",
			st.DecodeAvgMs, st.DecodeP95Ms, st.LagFrames, st.Dropped).
			Font("Consolas").FontSize(11).TextColor(colMuted)
		ui.Textf(c, "AUDIO buf %.1f ms  underruns %d  vol %.0f%%",
			st.AudioOccupancy, st.AudioUnderruns, a.prefs.Volume*100).
			Font("Consolas").FontSize(11).TextColor(colMuted)
	})
}

func (a *mirrpaneApp) buildFailedScreen(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		a.buildHeaderBar(c, "鏡屏 MirrPane", "錯誤", colDanger)
		body := ui.Column(c).Grow(1).FillWidth().Center().Padding(24)
		body.Children(func() {
			ui.Column(c).Width(680).Padding(24).Gap(16).Radius(14).Background(colSurface).
				Border(1, colDanger).Children(func() {
				ui.Row(c).AlignItems(ui.Center).Gap(10).Children(func() {
					pill(c, "接收器無法啟動", colDanger)
					ui.Spacer(c)
					if ui.Button(c.Key("fail-open-dir"), "開啟資料夾").Clicked() {
						a.openInstallDir()
					}
				})
				errMsg := nonEmpty(a.snap.Error, "未知錯誤")
				ui.Text(c, errMsg).FontSize(16).Bold().TextColor(colInk)

				ui.Text(c, "airplayd.log 近期內容").FontSize(12).TextColor(colMuted)
				ui.Scroll(c).Height(220).FillWidth().Padding(12).Radius(8).
					Background(colGround).Border(1, colBorder).Children(func() {
					if len(a.logLines) == 0 {
						ui.Text(c, "（尚無日誌內容）").Font("Consolas").FontSize(12).TextColor(colMuted)
					} else {
						for i, line := range a.logLines {
							ui.Text(c.Key(i), line).Font("Consolas").FontSize(11).TextColor(colMuted)
						}
					}
				})

				ui.Row(c).FillWidth().Gap(10).Justify(ui.End).Children(func() {
					if ui.Button(c.Key("fail-refresh-log"), "重新整理日誌").Clicked() {
						a.logLines = a.sidecar.LogTail(24)
					}
					if ui.PrimaryButton(c.Key("fail-retry"), "重試").Clicked() {
						a.restartReceiver(nil, "")
					}
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
		back.Background(ui.RGBA(0, 0, 0, 0.45))
		panel.Width(min(width, availW-32)).Height(min(height, availH-32)).
			Padding(16).Gap(10).Radius(12).Background(colSurface).
			Border(1, colBorder).Shadow(0, 16, 40, 0, ui.RGBA(0, 0, 0, 0.5))
		fn()
	})
}

func (a *mirrpaneApp) buildLogsModal(c *ui.Context) {
	a.panelOverlay(c.Key("logs-panel"), &a.showLogs, 760, 480, func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "接收器日誌（airplayd.log）").FontSize(16).Bold()
			ui.Spacer(c)
			if ui.Button(c.Key("logs-refresh"), "重新整理").Clicked() {
				a.logLines = a.sidecar.LogTail(40)
			}
			if ui.Button(c.Key("logs-close"), "關閉").Clicked() {
				a.showLogs = false
			}
		})
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, a.sidecar.LogPath()).Grow(1).Font("Consolas").FontSize(11).
				TextColor(colMuted).SingleLine()
			if ui.Button(c.Key("logs-copy-path"), "複製路徑").Clicked() {
				c.WriteClipboard(a.sidecar.LogPath())
				c.Toast("已複製日誌路徑")
			}
			if ui.Button(c.Key("logs-copy"), "複製日誌").Clicked() {
				if len(a.logLines) == 0 {
					a.logLines = a.sidecar.LogTail(40)
				}
				c.WriteClipboard(strings.Join(a.logLines, "\r\n"))
				c.Toast("已複製日誌內容")
			}
		})
		ui.Scroll(c).Grow(1).FillWidth().Padding(12).Radius(8).
			Background(colGround).Border(1, colBorder).Children(func() {
			if len(a.logLines) == 0 {
				ui.Text(c, "（目前沒有日誌記錄）").Font("Consolas").FontSize(12).TextColor(colMuted)
			} else {
				for i, line := range a.logLines {
					ui.Text(c.Key(i), line).Font("Consolas").FontSize(11).TextColor(colInk)
				}
			}
		})
	})
}

func (a *mirrpaneApp) buildSettingsModal(c *ui.Context) {
	a.panelOverlay(c.Key("settings-panel"), &a.showSettings, 580, 560, func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "鏡屏 MirrPane 設定").FontSize(16).Bold()
			ui.Spacer(c)
			ui.Text(c, "v"+appVersion).FontSize(11).TextColor(colMuted)
			if ui.Button(c.Key("set-close"), "關閉").Clicked() {
				a.showSettings = false
			}
		})

		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(10).Children(func() {
				a.buildSettingsGeneral(c)
				ui.Divider(c)
				a.buildSettingsGraphics(c)
				ui.Divider(c)
				a.buildSettingsRender(c)
				ui.Divider(c)
				a.buildSettingsAudio(c)
			})
		})

		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			if a.configDraft != a.initialConfig {
				ui.Text(c, "部分變更需「儲存並重啟」生效").FontSize(12).TextColor(colAccent)
			}
			ui.Spacer(c)
			if ui.Button(c.Key("set-save"), "儲存並重啟").Clicked() {
				a.saveConfigAndRestart()
			}
		})
	})
}

func (a *mirrpaneApp) buildSettingsGeneral(c *ui.Context) {
	ui.Text(c, "一般").FontSize(13).Bold()
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "接收名稱").Width(96).TextColor(colMuted)
		ui.TextInput(c.Key("set-name-in"), &a.configDraft.name).Grow(1)
	})
	if st := a.snap.State; st != nil {
		infoRow(c, "區域網路 IP", fmt.Sprintf("%s:%d", nonEmpty(st.IP, "-"), st.Port))
		infoRow(c, "當前解碼引擎", nonEmpty(st.Decoder, "待機"))
		infoRow(c, "累計畫面", fmt.Sprintf("%d 幀（略過 %d 幀）", st.Pictures, st.Dropped))
		infoRow(c, "音訊封包", fmt.Sprintf("%d 個", st.AudioFrames))
		infoRow(c, "協定版本", "AirPlay "+nonEmpty(st.Version, "-"))
	}
	ui.Row(c).FillWidth().Gap(8).Wrap().Children(func() {
		if ui.Button(c.Key("set-open-shots"), "開啟截圖資料夾").Clicked() {
			a.openScreenshots()
		}
		if ui.Button(c.Key("set-open-dir"), "開啟程式資料夾").Clicked() {
			a.openInstallDir()
		}
		if ui.Button(c.Key("set-reset-trust"), "清除信任裝置").Clicked() {
			go func() {
				if err := a.sidecar.ResetTrustedSenders(); err != nil {
					a.toastAsync("清除失敗：" + err.Error())
				} else {
					a.toastAsync("已清除所有信任裝置")
				}
			}()
		}
		if ui.Button(c.Key("set-reset-id"), "重設配對身分").TextColor(colDanger).Clicked() {
			if a.restartReceiver(func() error { return ResetIdentity(a.installDir) }, "接收器已重設配對身分") {
				a.showSettings = false
			} else {
				a.pendingToast = "接收器正在重新啟動，請稍候再操作"
			}
		}
	})
}

func (a *mirrpaneApp) buildSettingsGraphics(c *ui.Context) {
	ui.Text(c, "圖形與解碼（需儲存並重啟）").FontSize(13).Bold()
	selectRow(c, "set-res", "虛擬顯示解析度", &a.configDraft.resKey, resolutionOrder)
	maxFps := strconv.Itoa(a.configDraft.maxFps)
	if selectRow(c, "set-fps", "宣告幀率上限", &maxFps, []string{"60", "30"}) {
		if v, err := strconv.Atoi(maxFps); err == nil {
			a.configDraft.maxFps = v
		}
	}
	selectRow(c, "set-dec", "H.264 解碼模式", &a.configDraft.decoderMode, []string{"auto", "software"})
	selectRow(c, "set-range", "輸入色彩範圍", &a.configDraft.colorRange, []string{"auto", "full", "limited"})
	selectRow(c, "set-filter", "YUV → RGBA 轉換", &a.configDraft.scaleFilter, []string{"fast", "bilinear", "bicubic"})
}

func (a *mirrpaneApp) buildSettingsRender(c *ui.Context) {
	ui.Text(c, "畫面與色彩（即時生效）").FontSize(13).Bold()
	fill := a.prefs.FillViewport
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "填滿視窗（裁切）").Grow(1)
		if ui.Switch(c.Key("set-fill"), &fill).Changed() {
			next := a.prefs
			next.FillViewport = fill
			a.savePrefs(next)
		}
	})
	flip := a.prefs.FlipHorizontal
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "水平翻轉").Grow(1)
		if ui.Switch(c.Key("set-flip"), &flip).Changed() {
			next := a.prefs
			next.FlipHorizontal = flip
			a.savePrefs(next)
		}
	})
	hud := a.prefs.ShowHud
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "顯示效能 HUD").Grow(1)
		if ui.Switch(c.Key("set-hud"), &hud).Changed() {
			next := a.prefs
			next.ShowHud = hud
			a.savePrefs(next)
		}
	})
	corner := a.prefs.HudCorner
	if selectRow(c, "set-corner", "HUD 位置", &corner, allowedHudCorners) {
		next := a.prefs
		next.HudCorner = corner
		a.savePrefs(next)
	}
	// The renderer has two real sampling modes; offering four options that all
	// looked the same made the setting meaningless. Legacy values still load.
	quality := samplingChoice(a.prefs.FilterQuality)
	if selectRow(c, "set-qual", "縮放取樣", &quality, samplingChoices) {
		next := a.prefs
		next.FilterQuality = filterQualityForChoice(quality)
		a.savePrefs(next)
	}
	hideStr := strconv.Itoa(a.prefs.BarHideSeconds)
	if selectRow(c, "set-hide", "工具列自動隱藏（秒）", &hideStr, []string{"0", "2", "4"}) {
		if v, err := strconv.Atoi(hideStr); err == nil {
			next := a.prefs
			next.BarHideSeconds = v
			a.savePrefs(next)
		}
	}
	brightness := a.prefs.Brightness
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Textf(c, "亮度 %.2f", brightness).Width(110)
		if ui.Slider(c.Key("set-bri"), &brightness, -0.3, 0.3).Grow(1).Changed() {
			next := a.prefs
			next.Brightness = brightness
			a.savePrefs(next)
		}
	})
	contrast := a.prefs.Contrast
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Textf(c, "對比 %.2f", contrast).Width(110)
		if ui.Slider(c.Key("set-con"), &contrast, 0.7, 1.4).Grow(1).Changed() {
			next := a.prefs
			next.Contrast = contrast
			a.savePrefs(next)
		}
	})
	saturation := a.prefs.Saturation
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Textf(c, "飽和 %.2f", saturation).Width(110)
		if ui.Slider(c.Key("set-sat"), &saturation, 0.5, 1.5).Grow(1).Changed() {
			next := a.prefs
			next.Saturation = saturation
			a.savePrefs(next)
		}
	})
	if ui.Button(c.Key("set-reset-prefs"), "重設畫面偏好").Clicked() {
		next := DefaultUIPrefs()
		next.Muted, next.Volume = a.prefs.Muted, a.prefs.Volume
		a.savePrefs(next)
		c.Toast("已重設畫面偏好")
	}
}

func (a *mirrpaneApp) buildSettingsAudio(c *ui.Context) {
	ui.Text(c, "音訊").FontSize(13).Bold()
	vol := a.prefs.Volume
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Textf(c, "音量 %.0f%%", vol*100).Width(110)
		if ui.Slider(c.Key("set-vol"), &vol, 0, 1).Grow(1).Changed() {
			muteNow := vol <= 0.001
			next := a.prefs
			next.Volume = vol
			next.Muted = muteNow
			a.savePrefs(next)
		}
	})
	muted := a.prefs.Muted
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "靜音輸出 (M)").Grow(1)
		if ui.Switch(c.Key("set-mute"), &muted).Changed() {
			next := a.prefs
			next.Muted = muted
			a.savePrefs(next)
		}
	})
	selectRow(c, "set-buf", "音訊抗抖動緩衝（需重啟）", &a.configDraft.audioBuffer, []string{"low", "balanced", "stable"})
}

// selectRow lays out a labelled dropdown and reports whether it changed.
func selectRow(c *ui.Context, key, label string, value *string, options []string) bool {
	changed := false
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).Grow(1).FontSize(13)
		if ui.Select(c.Key(key), value, options).Width(200).Changed() {
			changed = true
		}
	})
	return changed
}

// infoRow lays out one read-only telemetry line.
func infoRow(c *ui.Context, label, value string) {
	ui.Row(c).FillWidth().Children(func() {
		ui.Text(c, label).FontSize(12.5).TextColor(colMuted)
		ui.Spacer(c)
		ui.Text(c, value).Font("Consolas").FontSize(12.5).TextColor(colInk)
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

func pill(c *ui.Context, text string, accent ui.Color) {
	ui.Text(c, text).FontSize(12).Bold().TextColor(accent).
		Padding(4, 10).Radius(999).Background(accent.Alpha(0.16)).
		Border(1, accent.Alpha(0.35)).SingleLine()
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
