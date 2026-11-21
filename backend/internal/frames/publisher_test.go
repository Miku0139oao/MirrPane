package frames

import (
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The control block is a contract with the C++ reader. Every offset here is
// mirrored by a static_assert in frontend/windows/runner/frame_reader.cc; a
// drift on either side must fail a build, not produce a garbled picture.
func TestControlLayout(t *testing.T) {
	var c control
	fields := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Magic", unsafe.Offsetof(c.Magic), 0x000},
		{"Version", unsafe.Offsetof(c.Version), 0x008},
		{"HeaderBytes", unsafe.Offsetof(c.HeaderBytes), 0x00C},
		{"SlotCount", unsafe.Offsetof(c.SlotCount), 0x010},
		{"SlotStride", unsafe.Offsetof(c.SlotStride), 0x014},
		{"SlotDataOffset", unsafe.Offsetof(c.SlotDataOffset), 0x018},
		{"Width", unsafe.Offsetof(c.Width), 0x020},
		{"Height", unsafe.Offsetof(c.Height), 0x024},
		{"Stride", unsafe.Offsetof(c.Stride), 0x028},
		{"PixelFormat", unsafe.Offsetof(c.PixelFormat), 0x02C},
		{"Generation", unsafe.Offsetof(c.Generation), 0x030},
		{"ProducerPID", unsafe.Offsetof(c.ProducerPID), 0x038},
		{"Flags", unsafe.Offsetof(c.Flags), 0x03C},
		{"Publish", unsafe.Offsetof(c.Publish), 0x040},
		{"FrameSeq", unsafe.Offsetof(c.FrameSeq), 0x048},
		{"LastPublishQPC", unsafe.Offsetof(c.LastPublishQPC), 0x050},
		{"HeartbeatQPC", unsafe.Offsetof(c.HeartbeatQPC), 0x058},
		{"FramePTS", unsafe.Offsetof(c.FramePTS), 0x060},
		{"Dropped", unsafe.Offsetof(c.Dropped), 0x068},
		{"ConsumerPID", unsafe.Offsetof(c.ConsumerPID), 0x080},
		{"ConsumerQPC", unsafe.Offsetof(c.ConsumerQPC), 0x088},
		{"ConsumerSlot", unsafe.Offsetof(c.ConsumerSlot), 0x090},
		{"Slots", unsafe.Offsetof(c.Slots), 0x100},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s at 0x%03X, want 0x%03X", f.name, f.got, f.want)
		}
	}
	slotBase := unsafe.Offsetof(c.Slots)
	slotSize := unsafe.Sizeof(c.Slots[0])
	if got := slotBase + slotSize; got != 0x140 {
		t.Errorf("slot 1 at 0x%03X, want 0x140", got)
	}
	if got := slotBase + 2*slotSize; got != 0x180 {
		t.Errorf("slot 2 at 0x%03X, want 0x180", got)
	}
	if got := unsafe.Sizeof(control{}); got != HeaderBytes {
		t.Errorf("control is %d bytes, want %d", got, HeaderBytes)
	}
	if got := unsafe.Sizeof(slotEntry{}); got != 64 {
		t.Errorf("slotEntry is %d bytes, want 64", got)
	}
	if Magic != 0x314D4853524D4641 {
		t.Errorf("magic = %#x", Magic)
	}
}

func TestSlotStride(t *testing.T) {
	// 1920x1080 of RGBA is 8,294,400 bytes; the slot rounds up to 64 KiB.
	if got := SlotStrideFor(1920, 1080); got != 0x7F0000 {
		t.Fatalf("1920x1080 stride = %#x, want 0x7F0000", got)
	}
	if got := MappingSize(1920, 1080); got != 0x10000+3*0x7F0000 {
		t.Fatalf("mapping size = %#x", got)
	}
	if SlotStrideFor(1, 1) < SlotAlign {
		t.Fatal("tiny frames must still get a whole alignment unit")
	}
}

// Publish must produce a control block a reader can validate, with the slot
// index and sequence packed into the publish word.
func TestPublishRoundTrip(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTest`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	w, h := 32, 16
	img := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		img[i*4] = 0x11
		img[i*4+1] = 0x22
		img[i*4+2] = 0x33
		img[i*4+3] = 0xff
	}

	for frame := 1; frame <= 6; frame++ {
		if err := p.Publish(w, h, w*4, img); err != nil {
			t.Fatalf("publish %d: %v", frame, err)
		}
		slot := int(p.ctrl.Publish & 3)
		if slot >= SlotCount {
			t.Fatalf("publish word points at slot %d", slot)
		}
		s := &p.ctrl.Slots[slot]
		if s.Width != uint32(w) || s.Height != uint32(h) {
			t.Fatalf("slot size = %dx%d", s.Width, s.Height)
		}
		if s.Stride != uint32(w*4) {
			t.Fatalf("slot stride = %d", s.Stride)
		}
		if s.Claim != claimFree {
			t.Fatalf("published slot still claimed: %d", s.Claim)
		}
		// The pixels must be readable at the slot's base.
		base := unsafe.Slice((*byte)(unsafe.Pointer(p.slots+uintptr(slot)*uintptr(p.stride))), w*h*4)
		for i := 0; i < 8; i++ {
			if base[i] != img[i] {
				t.Fatalf("frame %d byte %d = %#x, want %#x", frame, i, base[i], img[i])
			}
		}
		time.Sleep(time.Millisecond)
	}
	if p.ctrl.FrameSeq != 6 {
		t.Fatalf("frame sequence = %d, want 6", p.ctrl.FrameSeq)
	}
	if p.ctrl.Magic != Magic || p.ctrl.Version != Version {
		t.Fatalf("control block header is wrong: %#x v%d", p.ctrl.Magic, p.ctrl.Version)
	}
	if p.ctrl.PixelFormat != FormatRGBA {
		t.Fatalf("pixel format = %d, want RGBA", p.ctrl.PixelFormat)
	}
}

// A frame larger than a slot is refused rather than silently truncated.
func TestPublishRejectsOversize(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestSmall`, 32, 32)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	w, h := SlotCapacityWidth+16, SlotCapacityHeight+16
	big := make([]byte, w*h*4)
	if err := p.Publish(w, h, w*4, big); err == nil {
		t.Fatal("an oversized frame was accepted")
	}
}

// The slots are sized for the largest supported display, not for the one we
// advertised: a sender that mirrors at its own native resolution must still fit.
func TestPublishAcceptsLargerThanAdvertised(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestAdvert`, 1920, 1080)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	// An iPad Pro mirrors at this size; our advertised display is 1920x1080.
	const w, h = 2732, 2048
	img := make([]byte, w*h*4)
	if err := p.Publish(w, h, w*4, img); err != nil {
		t.Fatalf("a frame larger than the advertised display was refused: %v", err)
	}
	// The publish word names the slot the picture landed in.
	slot := &p.ctrl.Slots[p.ctrl.Publish&0x3]
	if slot.Width != w || slot.Height != h {
		t.Fatalf("slot holds %dx%d, want %dx%d", slot.Width, slot.Height, w, h)
	}
}

// Re-creating the publisher while the UI still holds the mapping and event
// handles open (for example during a rename/restart) must reattach cleanly.
func TestPublishReattachesOnRestart(t *testing.T) {
	const name = `Local\YingpingFramesTestRestart`
	p1, err := NewPublisher(name, 1920, 1080)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}

	w, h := 32, 16
	img := make([]byte, w*h*4)
	for i := 0; i < 3; i++ {
		if err := p1.Publish(w, h, w*4, img); err != nil {
			p1.Close()
			t.Fatalf("p1 publish %d: %v", i, err)
		}
	}

	prevPublish := atomic.LoadUint64(&p1.ctrl.Publish)
	prevSeq := prevPublish >> 2
	readerSlot := int(prevPublish & 0x3)
	staleWriterSlot := (readerSlot + 1) % SlotCount

	// Simulate the UI holding the mapping and event handles open, plus one
	// slot held by the reader and another left in claimWriter by a killed writer.
	namePtr, _ := windows.UTF16PtrFromString(name)
	readerMap, err := windows.CreateFileMapping(
		windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, 4096, namePtr)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		p1.Close()
		t.Fatalf("simulate reader CreateFileMapping: %v", err)
	}
	defer windows.CloseHandle(readerMap)

	evPtr, _ := windows.UTF16PtrFromString(EventName(name))
	readerEvent, err := windows.CreateEvent(nil, 0, 0, evPtr)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		p1.Close()
		t.Fatalf("simulate reader CreateEvent: %v", err)
	}
	defer windows.CloseHandle(readerEvent)

	atomic.StoreUint32(&p1.ctrl.Slots[readerSlot].Claim, claimReader)
	atomic.StoreUint32(&p1.ctrl.Slots[staleWriterSlot].Claim, claimWriter)

	p1.Close()

	p2, err := NewPublisher(name, 1920, 1080)
	if err != nil {
		t.Fatalf("NewPublisher on restart: %v", err)
	}
	defer p2.Close()

	if p2.event == 0 {
		t.Fatal("p2.event == 0 after reattach, expected valid event handle")
	}
	if got := atomic.LoadUint32(&p2.ctrl.Slots[readerSlot].Claim); got != claimReader {
		t.Fatalf("reader slot claim = %d, want %d (claimReader)", got, claimReader)
	}
	if got := atomic.LoadUint32(&p2.ctrl.Slots[staleWriterSlot].Claim); got != claimFree {
		t.Fatalf("stale writer slot claim = %d, want %d (claimFree)", got, claimFree)
	}

	if err := p2.Publish(w, h, w*4, img); err != nil {
		t.Fatalf("p2 publish: %v", err)
	}
	newPublish := atomic.LoadUint64(&p2.ctrl.Publish)
	newSeq := newPublish >> 2
	newSlot := int(newPublish & 0x3)
	if newSeq <= prevSeq {
		t.Fatalf("new publish seq = %d, want > %d", newSeq, prevSeq)
	}
	if newSlot == readerSlot {
		t.Fatalf("p2 overwrote reader-held slot %d", readerSlot)
	}
}
