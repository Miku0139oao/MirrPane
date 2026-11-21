package decode

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
