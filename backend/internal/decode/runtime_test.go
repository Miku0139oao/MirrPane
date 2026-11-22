package decode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/ffmpeg"
)

func TestMain(m *testing.M) {
	// Tests run from backend/internal/decode where production ffmpeg.Find() no
	// longer searches CWD. Point MIRRPANE_FFMPEG_DIR at the repo's fetched
	// test runtime when the caller has not overridden it.
	if os.Getenv("MIRRPANE_FFMPEG_DIR") == "" && os.Getenv("MIRRPANE_TEST_MISSING_RUNTIME") != "1" {
		if dir := findRepoTestFFmpeg(); dir != "" {
			_ = os.Setenv("MIRRPANE_FFMPEG_DIR", dir)
		}
	}
	os.Exit(m.Run())
}

func findRepoTestFFmpeg() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for range 6 {
		root := filepath.Join(cwd, "third_party", "ffmpeg")
		candidates := []string{filepath.Join(root, "bin")}
		if data, err := os.ReadFile(filepath.Join(root, "current")); err == nil {
			name := strings.TrimSpace(string(data))
			if name != "" && name != "." && filepath.Base(name) == name {
				candidates = append(candidates, filepath.Join(root, name))
			}
		}
		for _, dir := range candidates {
			ok := true
			for _, dll := range ffmpeg.DLLs {
				info, err := os.Stat(filepath.Join(dir, dll))
				if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
					ok = false
					break
				}
			}
			if ok {
				return dir
			}
		}
		cwd = filepath.Dir(cwd)
	}
	return ""
}

func TestFFmpegRejectsIncompatibleABI(t *testing.T) {
	for _, minimum := range []uint32{minCodecVersion, minUtilVersion, minScaleVersion, minResampleVersion} {
		for _, actual := range []uint32{minimum - 1, minimum + 1<<16} {
			if err := checkVersion("test library", actual, minimum); err == nil {
				t.Fatalf("accepted incompatible version %x for %x", actual, minimum)
			}
		}
		for _, actual := range []uint32{minimum, minimum + 1, minimum + 1<<8} {
			if err := checkVersion("test library", actual, minimum); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The executable must start and report a normal Go error without FFmpeg. The
// old cgo-linked binary failed in the Windows loader before Go could run setup.
func TestMissingRuntimeIsRecoverable(t *testing.T) {
	if os.Getenv("MIRRPANE_TEST_MISSING_RUNTIME") == "1" {
		if _, err := NewDecoder(); err == nil || !strings.Contains(err.Error(), "MIRRPANE_FFMPEG_DIR") {
			t.Fatalf("missing runtime did not produce a recoverable error: %v", err)
		}
		return
	}
	t.Setenv("MIRRPANE_FFMPEG_DIR", t.TempDir())
	cmd := exec.Command(os.Args[0], "-test.run=^TestMissingRuntimeIsRecoverable$")
	cmd.Env = append(os.Environ(), "MIRRPANE_TEST_MISSING_RUNTIME=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process could not run without FFmpeg: %v\n%s", err, output)
	}
}

func TestClosedDecodersRejectInput(t *testing.T) {
	d, err := NewDecoderWithOptions(Options{})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Close()
	if _, err := d.DecodeAccessUnit([]byte{0}); err == nil {
		t.Fatal("closed video decoder accepted input")
	}
	if d.LastImage() != nil || d.Hardware() {
		t.Fatal("closed decoder retained frame state")
	}
	a, err := NewAudioDecoder(nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	a.Close()
	if _, err := a.Decode([]byte{0}); err == nil {
		t.Fatal("closed audio decoder accepted input")
	}
}
