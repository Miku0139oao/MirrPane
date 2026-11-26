package decode

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestHWFailureTrackerFrameThreshold(t *testing.T) {
	tr := newHWFailureTracker()
	now := time.Unix(0, 0)
	if tr.state != hwHealthy {
		t.Fatalf("initial state = %v, want healthy", tr.state)
	}

	// Failures below the threshold only degrade.
	for i := 1; i < hwFailureFrames; i++ {
		if action, run := tr.failure(now); action != hwActionNone || run != i {
			t.Fatalf("failure %d = (%v, %d), want (none, %d)", i, action, run, i)
		}
		if tr.state != hwDegraded {
			t.Fatalf("after %d failures state = %v, want degraded", i, tr.state)
		}
	}
	// Crossing it asks for one device recreation and restarts the count.
	if action, run := tr.failure(now); action != hwActionRecreateDevice || run != hwFailureFrames {
		t.Fatalf("threshold failure = (%v, %d), want (recreate, %d)", action, run, hwFailureFrames)
	}
	if tr.state != hwHealthy || tr.consecutive != 0 {
		t.Fatalf("after recreate: state=%v consecutive=%d, want healthy/0", tr.state, tr.consecutive)
	}

	// A second run past the threshold falls back to software for good.
	for i := 1; i < hwFailureFrames; i++ {
		if action, _ := tr.failure(now); action != hwActionNone {
			t.Fatalf("second run failure %d = %v, want none", i, action)
		}
	}
	if action, _ := tr.failure(now); action != hwActionUseSoftware {
		t.Fatalf("second threshold = %v, want software", action)
	}
	if tr.state != hwSoftware {
		t.Fatalf("state = %v, want software", tr.state)
	}

	// Software is terminal: neither failures nor successes leave it.
	for i := 0; i < 2*hwFailureFrames; i++ {
		if action, _ := tr.failure(now); action != hwActionNone {
			t.Fatalf("failure in software mode = %v, want none", action)
		}
	}
	tr.success()
	if tr.state != hwSoftware {
		t.Fatalf("success left software mode: %v", tr.state)
	}
}

func TestHWFailureTrackerSuccessResets(t *testing.T) {
	tr := newHWFailureTracker()
	now := time.Unix(0, 0)
	// Intermittent errors (corrupt packets) separated by good pictures must
	// never accumulate into a fallback.
	for round := 0; round < 10; round++ {
		for i := 0; i < hwFailureFrames-1; i++ {
			if action, _ := tr.failure(now); action != hwActionNone {
				t.Fatalf("round %d failure %d = %v, want none", round, i, action)
			}
		}
		tr.success()
		if tr.state != hwHealthy || tr.consecutive != 0 {
			t.Fatalf("round %d: success gave state=%v consecutive=%d", round, tr.state, tr.consecutive)
		}
	}
	if tr.retries != hwDeviceRetries {
		t.Fatalf("retries consumed without crossing the threshold: %d", tr.retries)
	}
}

func TestHWFailureTrackerTimeWindow(t *testing.T) {
	tr := newHWFailureTracker()
	start := time.Unix(100, 0)
	step := 300 * time.Millisecond

	// A slow failure stream (static screen) trips on elapsed time once the
	// minimum count is reached, long before the frame threshold.
	var action hwAction
	var run int
	n := 0
	for action == hwActionNone {
		action, run = tr.failure(start.Add(time.Duration(n) * step))
		n++
		if n > hwFailureFrames {
			t.Fatal("time window never triggered")
		}
	}
	if action != hwActionRecreateDevice {
		t.Fatalf("action = %v, want recreate", action)
	}
	if run < hwFailureMinFrames || time.Duration(run-1)*step < hwFailureWindow {
		t.Fatalf("tripped after %d failures spanning %v", run, time.Duration(run-1)*step)
	}

	// Elapsed time alone is not enough: fewer than the minimum count waits.
	tr = newHWFailureTracker()
	for i := 0; i < hwFailureMinFrames-1; i++ {
		if action, _ := tr.failure(start.Add(time.Duration(i) * time.Hour)); action != hwActionNone {
			t.Fatalf("failure %d tripped below the minimum count", i)
		}
	}
}

func TestHWFailureTrackerForceSoftware(t *testing.T) {
	tr := newHWFailureTracker()
	tr.failure(time.Unix(0, 0))
	tr.forceSoftware()
	if tr.state != hwSoftware || tr.consecutive != 0 {
		t.Fatalf("forceSoftware: state=%v consecutive=%d", tr.state, tr.consecutive)
	}
	if action, _ := tr.failure(time.Unix(0, 0)); action != hwActionNone {
		t.Fatalf("failure after forceSoftware = %v", action)
	}
}

func TestAnnexBParameterSets(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1e}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	sei := []byte{0x06, 0x05, 0x01}
	idr := []byte{0x65, 0x88, 0x84}
	slice := []byte{0x41, 0x9a, 0x02}
	sc4 := []byte{0, 0, 0, 1}
	sc3 := []byte{0, 0, 1}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	want := cat(sc4, sps, sc4, pps)

	for _, tc := range []struct {
		name string
		au   []byte
		want []byte
	}{
		{"4-byte codes", cat(sc4, sps, sc4, pps, sc4, idr), want},
		{"3-byte codes", cat(sc3, sps, sc3, pps, sc3, idr), want},
		{"mixed with SEI", cat(sc4, sei, sc3, sps, sc4, pps, sc3, idr), want},
		{"parameter sets only", cat(sc4, sps, sc4, pps), want},
		{"slice only", cat(sc4, slice), nil},
		{"after first slice ignored", cat(sc4, slice, sc4, sps, sc4, pps), nil},
		{"no start code", []byte{1, 2, 3, 4}, nil},
		{"empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := annexBParameterSets(tc.au); !bytes.Equal(got, tc.want) {
				t.Fatalf("got % x, want % x", got, tc.want)
			}
		})
	}

	// The result is a copy, not a view into the caller's buffer.
	au := cat(sc4, sps, sc4, pps, sc4, idr)
	got := annexBParameterSets(au)
	au[4] = 0xff
	if got[4] != sps[0] {
		t.Fatal("result aliases the input buffer")
	}
}

func TestMergeParameterSets(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x42}
	pps := []byte{0, 0, 0, 1, 0x68, 0xce}
	newSPS := []byte{0, 0, 0, 1, 0x67, 0x64}
	cache := mergeParameterSets(nil, sps)
	cache = mergeParameterSets(cache, pps)
	if want := bytes.Join([][]byte{sps, pps}, nil); !bytes.Equal(cache, want) {
		t.Fatalf("separate SPS/PPS cache = % x, want % x", cache, want)
	}
	cache = mergeParameterSets(cache, newSPS)
	if want := bytes.Join([][]byte{newSPS, pps}, nil); !bytes.Equal(cache, want) {
		t.Fatalf("SPS update = % x, want % x", cache, want)
	}
}

func TestHardwareReportsCurrentMode(t *testing.T) {
	d := &Decoder{ctx: &codecContext{}, frame: &avFrame{}, hardwareFormat: 42, ready: true}
	// A retained snapshot must not make a freshly recreated hardware codec
	// look like software while it is still waiting for its first keyframe.
	if !d.Hardware() || d.Status() != "D3D11VA" {
		t.Fatalf("configured hardware: Hardware=%v Status=%q", d.Hardware(), d.Status())
	}
	d.decoded = true
	if d.Hardware() || !strings.Contains(d.Status(), "declined") {
		t.Fatalf("software picture: Hardware=%v Status=%q", d.Hardware(), d.Status())
	}
	d.frame.Format = 42
	if !d.Hardware() {
		t.Fatal("hardware picture did not report hardware mode")
	}
	d.hardwareFormat = pixelNone
	if d.Hardware() {
		t.Fatal("software fallback retained hardware mode")
	}
	d.ctx = nil
	if d.Hardware() || d.Status() != "closed" {
		t.Fatal("closed decoder retained its previous mode")
	}
}

// Exercise the actual access-unit recovery path without a GPU or FFmpeg DLLs.
// The injected API also checks native ownership and parameter replay.
func TestDecodeAccessUnitHardwareRecovery(t *testing.T) {
	for _, failure := range []string{"send", "receive", "transfer"} {
		t.Run(failure, func(t *testing.T) {
			const hardwareFormat = 42
			var events []string
			var storage, sent []byte
			f := &ffmpegAPI{
				findDecoder:  func(int32) uintptr { return 1 },
				allocContext: func(uintptr) *codecContext { return &codecContext{} },
				openCodec:    func(*codecContext, uintptr, uintptr) int32 { return 0 },
				freeContext: func(ctx **codecContext) {
					events = append(events, "codec")
					*ctx = nil
				},
				bufferUnref: func(device *uintptr) {
					if len(events) == 0 || events[len(events)-1] != "codec" {
						t.Fatal("hardware device freed before codec context")
					}
					events = append(events, "device")
					*device = 0
				},
				frameAlloc:  func() *avFrame { return &avFrame{} },
				frameFree:   func(frame **avFrame) { *frame = nil },
				frameUnref:  func(*avFrame) {},
				packetAlloc: func() *avPacket { return &avPacket{} },
				packetFree:  func(packet **avPacket) { *packet = nil },
				packetUnref: func(*avPacket) {},
				newPacket: func(packet *avPacket, size int32) int32 {
					storage = make([]byte, size)
					packet.Data, packet.Size = &storage[0], size
					return 0
				},
				parserInit:      func(int32) uintptr { return 1 },
				parserClose:     func(uintptr) {},
				strerror:        func(_ int32, b *byte, _ uintptr) int32 { *b = 0; return 0 },
				hwFrameTransfer: func(*avFrame, *avFrame, int32) int32 { return -1 },
			}
			d := &Decoder{
				ff: f, ctx: &codecContext{}, frame: &avFrame{Format: hardwareFormat},
				packet: &avPacket{}, device: 1, hardwareFormat: hardwareFormat,
				health: newHWFailureTracker(), ready: true,
				img: Image{Width: 1, Height: 1, Stride: 4, Pixels: []byte{1, 2, 3, 255}},
			}
			defer d.Close()
			// Skip the optional recreation so each injected failure exercises
			// the permanent software transition deterministically.
			d.health.retries = 0
			f.sendPacket = func(ctx *codecContext, packet *avPacket) int32 {
				sent = append(sent[:0], unsafe.Slice(packet.Data, int(packet.Size))...)
				if d.hardwareFormat != pixelNone && failure == "send" {
					return -1
				}
				return 0
			}
			f.receiveFrame = func(_ *codecContext, frame *avFrame) int32 {
				if d.hardwareFormat == pixelNone {
					return errorAgain
				}
				if failure == "receive" {
					return -1
				}
				frame.Format = hardwareFormat
				return 0
			}
			sps := []byte{0, 0, 0, 1, 0x67, 0x42}
			pps := []byte{0, 0, 0, 1, 0x68, 0xce}
			for i := 0; i < hwFailureFrames; i++ {
				data := pps
				if i == 0 {
					data = sps
				}
				if _, err := d.DecodeAccessUnit(data); err == nil || errors.Is(err, ErrNoFrame) {
					t.Fatalf("failure %d = %v, want injected decode error", i+1, err)
				}
				if i < hwFailureFrames-1 && len(events) != 0 {
					t.Fatalf("native state released before threshold: %v", events)
				}
			}
			if d.ctx == nil || d.Hardware() || d.health.state != hwSoftware {
				t.Fatalf("fallback failed: ctx=%v Hardware=%v state=%v", d.ctx != nil, d.Hardware(), d.health.state)
			}
			if got := strings.Join(events, ","); got != "codec,device" {
				t.Fatalf("release order = %s", got)
			}
			if d.LastImage() == nil || d.LastImage().Pixels[0] != 1 {
				t.Fatal("rebuild lost the last snapshot")
			}
			slice := []byte{0, 0, 0, 1, 0x65, 0x88}
			if _, err := d.DecodeAccessUnit(slice); !errors.Is(err, ErrNoFrame) {
				t.Fatalf("software packet = %v, want ErrNoFrame", err)
			}
			if want := bytes.Join([][]byte{sps, pps, slice}, nil); !bytes.Equal(sent, want) {
				t.Fatalf("replayed packet = % x, want % x", sent, want)
			}
			if d.replayParams || d.health.state != hwSoftware {
				t.Fatal("parameter replay repeated or hardware mode re-enabled")
			}
		})
	}
}

// nalUnits splits an Annex-B capture at both 3- and 4-byte start codes. Each
// unit keeps its start code.
func nalUnits(stream []byte) [][]byte {
	var out [][]byte
	start, payload := findStartCode(stream, 0)
	for start >= 0 {
		next, nextPayload := findStartCode(stream, payload)
		end := next
		if end < 0 {
			end = len(stream)
		}
		out = append(out, stream[start:end])
		start, payload = next, nextPayload
	}
	return out
}

// nalType reads the type of a unit produced by nalUnits.
func nalType(u []byte) byte {
	_, p := findStartCode(u, 0)
	return u[p] & 0x1f
}

// accessUnits groups an Annex-B capture so every slice carries the non-VCL
// units in front of it.
func accessUnits(stream []byte) [][]byte {
	var out [][]byte
	var cur []byte
	for _, u := range nalUnits(stream) {
		cur = append(cur, u...)
		if t := nalType(u); t >= 1 && t <= 5 {
			out = append(out, cur)
			cur = nil
		}
	}
	return out
}

// firstSlice strips the leading non-VCL units from an access unit.
func firstSlice(au []byte) []byte {
	for _, u := range nalUnits(au) {
		if t := nalType(u); t >= 1 && t <= 5 {
			return u
		}
	}
	return nil
}

// TestRebuildReplaysParameterSets drives the real recovery path: a rebuilt
// decoder must decode a keyframe that arrives without SPS/PPS, because the
// AirPlay sender only sends them once per stream.
func TestRebuildReplaysParameterSets(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := accessUnits(stream)
	if len(units) == 0 {
		t.Fatal("no access units in fixture")
	}
	idr := firstSlice(units[0])
	if idr == nil || nalType(idr) != 5 {
		t.Fatal("fixture does not start with an IDR access unit")
	}

	for _, tc := range []struct {
		name     string
		hardware bool
	}{{"software", false}, {"hardware-preferred", true}} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDecoderWithOptions(Options{PreferHardware: tc.hardware, ColorRange: "auto", ScaleFilter: "fast"})
			if err != nil {
				t.Skipf("FFmpeg runtime unavailable: %v", err)
			}
			defer d.Close()
			if _, err := d.DecodeAccessUnit(units[0]); err != nil && !errors.Is(err, ErrNoFrame) {
				t.Fatalf("first access unit: %v", err)
			}
			if len(d.params) == 0 {
				t.Fatal("parameter sets were not cached")
			}

			d.rebuild(tc.hardware)
			if d.ctx == nil {
				t.Fatal("rebuild left the decoder closed")
			}
			if !d.replayParams {
				t.Fatal("rebuild did not arm parameter-set replay")
			}
			stripped := append([][]byte{idr}, units[1:]...)
			img, err := firstPicture(d, stripped)
			if err != nil {
				t.Fatalf("after rebuild: %v", err)
			}
			if img.Width != 320 || img.Height != 240 {
				t.Fatalf("decoded %dx%d, want 320x240", img.Width, img.Height)
			}
			if d.replayParams {
				t.Fatal("replay stayed armed after a successful send")
			}
			t.Logf("after rebuild: %s", d.Status())

			// Control: without the replay the same input yields nothing,
			// which is the dark receiver this guards against.
			d.rebuild(tc.hardware)
			d.replayParams = false
			if img, err := firstPicture(d, stripped); err == nil {
				t.Fatalf("decoded %dx%d without parameter sets; control is meaningless", img.Width, img.Height)
			}
		})
	}
}

// firstPicture feeds access units until one yields a picture. The fixture has
// a two-frame reorder delay, so the keyframe's picture appears later.
func firstPicture(d *Decoder, units [][]byte) (*Image, error) {
	var last error = ErrNoFrame
	for _, u := range units {
		img, err := d.DecodeAccessUnit(u)
		if err == nil {
			return img, nil
		}
		last = err
	}
	return nil, last
}

// TestInjectedHardwareFailuresFallBackToSoftware feeds synthetic failures
// through the decoder's recovery hook and checks it ends up decoding in
// software, without needing a real GPU fault.
func TestInjectedHardwareFailuresFallBackToSoftware(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := accessUnits(stream)
	d, err := NewDecoder()
	if err != nil {
		t.Skipf("FFmpeg runtime unavailable: %v", err)
	}
	defer d.Close()
	t.Logf("initial: %s", d.Status())
	hadHardware := d.Hardware()
	if _, err := d.DecodeAccessUnit(units[0]); err != nil && !errors.Is(err, ErrNoFrame) {
		t.Fatalf("first access unit: %v", err)
	}

	injected := errors.New("download hardware frame: injected")
	for i := 0; i < 2*hwFailureFrames && d.health.state != hwSoftware; i++ {
		d.recordHardwareFailure(injected)
	}
	if d.health.state != hwSoftware {
		t.Fatalf("tracker state = %v after %d injected failures", d.health.state, 2*hwFailureFrames)
	}
	if d.ctx == nil || d.hardwareFormat != pixelNone {
		t.Fatalf("decoder not in software mode: ctx=%v hardwareFormat=%d", d.ctx != nil, d.hardwareFormat)
	}
	if s := d.Status(); !strings.HasPrefix(s, "software") {
		t.Fatalf("Status() = %q, want software", s)
	}

	img, err := firstPicture(d, append([][]byte{firstSlice(units[0])}, units[1:]...))
	if err != nil {
		t.Fatalf("after fallback: %v", err)
	}
	if img.Width != 320 || img.Height != 240 {
		t.Fatalf("decoded %dx%d, want 320x240", img.Width, img.Height)
	}
	if d.Hardware() {
		t.Fatal("Hardware() = true after software fallback")
	}
	t.Logf("final: %s", d.Status())
	if err := d.ResetStream(); err != nil {
		t.Fatal(err)
	}
	if d.LastImage() != nil || len(d.params) != 0 || d.replayParams || d.frameReady {
		t.Fatal("new sender inherited a picture or H.264 parameter sets")
	}
	if hadHardware && !d.Hardware() {
		t.Fatalf("new sender did not recover the initially available hardware decoder: %s", d.Status())
	}
	if d.Hardware() && d.health != newHWFailureTracker() {
		t.Fatalf("new hardware session inherited a depleted retry budget: %+v", d.health)
	}
	if _, err := firstPicture(d, units); err != nil {
		t.Fatalf("second sender: %v", err)
	}
}
