package main

import (
	"bytes"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestOfflineSettingsOnlyWriteEditedFields(t *testing.T) {
	a := &mirrpaneApp{}
	a.openSettings()
	a.configDraft.name = "Living room"
	patch := a.configDraft.changes(a.initialConfig)
	if len(patch) != 1 || patch["name"] != "Living room" {
		t.Fatalf("renaming an offline receiver also changed its settings: %v", patch)
	}
	a.configDraft.resKey = "2560x1440"
	patch = a.configDraft.changes(a.initialConfig)
	if len(patch) != 3 || patch["width"] != 2560 || patch["height"] != 1440 {
		t.Fatalf("resolution edit was not applied: %v", patch)
	}
}

func TestConfigPatchPreservesIdentityAndDamagedFile(t *testing.T) {
	dir := t.TempDir()
	path := ConfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, original := range []string{`{"keySeed":"unterminated`, "null"} {
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SaveReceiverName(dir, "receiver"); err == nil {
			t.Fatal("an unreadable identity was silently replaced")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != original {
			t.Fatal("failed edit modified the file", err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"keySeed":"dpapi:opaque","future":9223372036854775807}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveReceiverName(dir, "receiver"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(got, []byte("9223372036854775807")) || !bytes.Contains(got, []byte("dpapi:opaque")) {
		t.Fatal("settings edit lost previously stored data", err)
	}
}

func TestPreferenceEditsFlushInOrderOnClose(t *testing.T) {
	dir := t.TempDir()
	a := newMirrPaneApp(dir, nil, true)
	for i := range 100 {
		next := a.prefs
		next.Volume = float64(i) / 100
		a.savePrefs(next)
	}
	a.shutdown()
	a.shutdown()
	if got := LoadUIPrefs(dir); got != a.prefs || got.Volume != 0.99 {
		t.Fatalf("last persisted volume = %v", got.Volume)
	}
}

type stillPicture struct{ img *image.RGBA }

func (s *stillPicture) Latest() (*image.RGBA, bool) {
	frame := s.img
	s.img = nil
	return frame, frame != nil
}
func (*stillPicture) Close() error { return nil }

func TestLiveAndInterruptedViewKeepsPictureUntilDisconnect(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1178))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, 40, 80, 255
	}
	a := newMirrPaneApp(t.TempDir(), &stillPicture{img}, true)
	defer a.shutdown()
	a.applySnapshot(Snapshot{Phase: PhaseLive})
	view := ui.NewTester(a.view, 900, 540)
	if w, h := a.bitmap.Size(); w != 2560 || h != 1178 {
		t.Fatalf("presented %dx%d, want 2560x1178", w, h)
	}
	if got := view.Image().RGBAAt(450, 340); got != (color.RGBA{200, 40, 80, 255}) {
		t.Fatalf("2K live picture has wrong center color: %v", got)
	}
	a.fullscreen = true
	a.applySnapshot(Snapshot{Phase: PhaseInterrupted, State: &ReceiverState{Reason: "訊號中斷"}})
	view.Frame()
	if !view.HasText("訊號中斷") || !view.HasText("離開全螢幕") {
		t.Fatalf("interruption controls absent: %v", view.Texts())
	}
	if w, h := a.bitmap.Size(); w != 2560 || h != 1178 {
		t.Fatalf("last picture lost during interruption: %dx%d", w, h)
	}
	a.applySnapshot(Snapshot{Phase: PhaseWaiting})
	view.Frame()
	if w, h := a.bitmap.Size(); w != 1 || h != 1 || !view.HasText("待機中") {
		t.Fatalf("disconnect did not clear the picture and return to waiting: %dx%d", w, h)
	}
}

func TestFirstTrustCanBeApprovedWhileConnecting(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), nil, true)
	defer a.shutdown()
	st := defaultReceiverState()
	st.PendingSender = &PendingSender{Fingerprint: "abc", Label: "Test iPhone"}
	a.applySnapshot(Snapshot{Phase: PhaseConnecting, State: &st})
	view := ui.NewTester(a.view, 640, 420)
	if !view.HasText("允許連線") || !view.HasText("Test iPhone") || !view.HasText("拒絕") {
		t.Fatalf("connecting sender has no trust controls: %v", view.Texts())
	}
}

func TestSettingsFitsMinimumWindowAtHighDPI(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), nil, true)
	defer a.shutdown()
	a.applySnapshot(Snapshot{Phase: PhaseWaiting})
	a.openSettings()
	view := ui.NewTester(a.view, 640, 420)
	view.SetScale(1.5)
	view.Frame()
	save, ok := view.Find("儲存並重啟")
	if !ok || save.Y < 0 || save.Y+save.H > 420 {
		t.Fatalf("save button is outside the settings viewport: %+v", save)
	}
	dlg, ok := view.AccessByRole(ui.RoleDialog, "")
	if !ok || dlg.Bounds.X+dlg.Bounds.W/2 != 320 {
		t.Fatalf("settings dialog is not centered: %+v", dlg.Bounds)
	}
}
