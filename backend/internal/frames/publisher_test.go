package frames

import (
	"errors"
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
		{"AdapterLUID", unsafe.Offsetof(c.AdapterLUID), 0x070},
		{"ConsumerPID", unsafe.Offsetof(c.ConsumerPID), 0x080},
		{"ConsumerFlags", unsafe.Offsetof(c.ConsumerFlags), 0x084},
		{"ConsumerQPC", unsafe.Offsetof(c.ConsumerQPC), 0x088},
		{"ConsumerSlot", unsafe.Offsetof(c.ConsumerSlot), 0x090},
		{"ConsumerAdopted", unsafe.Offsetof(c.ConsumerAdopted), 0x098},
		{"Slots", unsafe.Offsetof(c.Slots), 0x100},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s at 0x%03X, want 0x%03X", f.name, f.got, f.want)
		}
	}
	var s slotEntry
	if got := unsafe.Offsetof(s.SharedHandle); got != 0x38 {
		t.Errorf("slotEntry.SharedHandle at 0x%02X, want 0x38", got)
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
		base := unsafe.Slice((*byte)(unsafe.Add(p.slots, uintptr(slot)*uintptr(p.stride))), w*h*4)
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

// Acquire/Commit is the direct-to-slot path the decoder uses: the producer
// renders into mapped memory and only the commit publishes it.
func TestAcquireCommitRoundTrip(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestAcquire`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	const w, h = 40, 24
	c, err := p.Acquire(w, h)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if c.Stride() != w*4 {
		t.Fatalf("claim stride = %d, want %d", c.Stride(), w*4)
	}
	if len(c.Pixels()) != w*h*4 {
		t.Fatalf("claim buffer = %d bytes, want %d", len(c.Pixels()), w*h*4)
	}
	for i := range c.Pixels() {
		c.Pixels()[i] = byte(i)
	}

	before := atomic.LoadUint64(&p.ctrl.Publish)
	c.Commit()
	after := atomic.LoadUint64(&p.ctrl.Publish)
	if after == before {
		t.Fatal("commit did not advance the publish word")
	}
	slot := int(after & 0x3)
	if slot != c.index {
		t.Fatalf("publish word names slot %d, claim was slot %d", slot, c.index)
	}
	s := &p.ctrl.Slots[slot]
	if s.Width != w || s.Height != h || s.Stride != w*4 || s.Format != FormatRGBA {
		t.Fatalf("slot metadata = %dx%d stride %d format %d", s.Width, s.Height, s.Stride, s.Format)
	}
	if s.Claim != claimFree {
		t.Fatalf("committed slot still claimed: %d", s.Claim)
	}
	if got := atomic.LoadUint64(&p.ctrl.FrameSeq); got != 1 {
		t.Fatalf("frame sequence = %d, want 1", got)
	}

	// The pixels committed must be exactly what the caller rendered.
	base := unsafe.Slice((*byte)(unsafe.Add(p.slots, uintptr(slot)*uintptr(p.stride))), w*h*4)
	for i := 0; i < w*h*4; i++ {
		if base[i] != byte(i) {
			t.Fatalf("slot byte %d = %#x, want %#x", i, base[i], byte(i))
		}
	}

	// Committing again must not publish the same picture twice.
	c.Commit()
	if got := atomic.LoadUint64(&p.ctrl.Publish); got != after {
		t.Fatalf("double commit advanced the publish word to %#x", got)
	}
}

// A claimed-but-uncommitted slot must not be observable: the publish word still
// names the previous picture and Latest returns it unchanged. Cancelling must
// give the slot back without publishing.
func TestAcquireUncommittedSlotNotPublished(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestUncommitted`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	const w, h = 32, 16
	img := make([]byte, w*h*4)
	for i := range img {
		img[i] = 0x5a
	}
	if err := p.Publish(w, h, w*4, img); err != nil {
		t.Fatalf("publish: %v", err)
	}
	prevPublish := atomic.LoadUint64(&p.ctrl.Publish)
	prevPic, ok := p.Latest()
	if !ok {
		t.Fatal("no picture after a publish")
	}

	c, err := p.Acquire(w, h)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate a half-written slot: the pixels are already scribbled over but
	// the commit has not happened.
	for i := range c.Pixels() {
		c.Pixels()[i] = 0xAB
	}
	if got := atomic.LoadUint64(&p.ctrl.Publish); got != prevPublish {
		t.Fatalf("publish word changed before commit: %#x", got)
	}
	if got := p.ctrl.Slots[c.index].Claim; got != claimWriter {
		t.Fatalf("acquired slot claim = %d, want claimWriter", got)
	}
	pic, ok := p.Latest()
	if !ok || pic.Seq != prevPic.Seq {
		t.Fatalf("Latest saw the uncommitted picture: ok=%v seq=%d want %d", ok, pic.Seq, prevPic.Seq)
	}
	for i := range pic.Pixels {
		if pic.Pixels[i] != 0x5a {
			t.Fatalf("Latest returned half-written pixel %d = %#x", i, pic.Pixels[i])
		}
	}

	c.Cancel()
	if got := p.ctrl.Slots[c.index].Claim; got != claimFree {
		t.Fatalf("cancelled slot claim = %d, want claimFree", got)
	}
	if got := atomic.LoadUint64(&p.ctrl.Publish); got != prevPublish {
		t.Fatalf("cancel published a picture: %#x", got)
	}
	if pic, ok := p.Latest(); !ok || pic.Seq != prevPic.Seq {
		t.Fatalf("Latest changed after a cancel: ok=%v seq=%d", ok, pic.Seq)
	}
}

// With every slot claimed there is one more slot than a real consumer can hold
// busy at once, so the fourth acquire is refused rather than overwriting a
// slot in use.
func TestAcquireRefusesWhenAllSlotsBusy(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestBusy`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	claims := make([]*Claim, 0, SlotCount)
	for i := 0; i < SlotCount; i++ {
		c, err := p.Acquire(8, 8)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		claims = append(claims, c)
	}
	if _, err := p.Acquire(8, 8); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("acquire with every slot busy = %v, want ErrNoSlot", err)
	}
	for _, c := range claims {
		c.Cancel()
	}
	if _, err := p.Acquire(8, 8); err != nil {
		t.Fatalf("acquire after cancelling: %v", err)
	}
}

// Latest is the snapshot path: a detached copy of the newest published frame.
func TestLatestCopiesNewestPicture(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestLatest`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	if _, ok := p.Latest(); ok {
		t.Fatal("Latest found a picture before any publish")
	}

	first := make([]byte, 16*8*4)
	for i := range first {
		first[i] = 0x11
	}
	if err := p.Publish(16, 8, 16*4, first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	second := make([]byte, 24*12*4)
	for i := range second {
		second[i] = 0x22
	}
	if err := p.Publish(24, 12, 24*4, second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	pic, ok := p.Latest()
	if !ok {
		t.Fatal("Latest found no picture after publishing")
	}
	if pic.Width != 24 || pic.Height != 12 || pic.Stride != 24*4 {
		t.Fatalf("latest = %dx%d stride %d", pic.Width, pic.Height, pic.Stride)
	}
	if len(pic.Pixels) != pic.Stride*pic.Height {
		t.Fatalf("latest buffer = %d bytes, want %d", len(pic.Pixels), pic.Stride*pic.Height)
	}
	for i, b := range pic.Pixels {
		if b != 0x22 {
			t.Fatalf("latest byte %d = %#x, want the second picture", i, b)
		}
	}
	// The result is a copy: changing it must not reach shared memory.
	pic.Pixels[0] = 0x33
	again, _ := p.Latest()
	if again.Pixels[0] != 0x22 {
		t.Fatal("Latest aliased the mapped slot")
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

func TestCommitGPURoundTripAndConsumerFallbackFlag(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestGPU`, 1920, 1080)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	if p.ConsumerAllowsGPUShare() {
		t.Fatal("GPU sharing enabled before consumer attached")
	}
	atomic.StoreUint32(&p.ctrl.ConsumerFlags, ConsumerFlagAttached)
	if p.ConsumerAllowsGPUShare() {
		t.Fatal("GPU sharing enabled for legacy consumer")
	}
	atomic.StoreUint32(&p.ctrl.ConsumerFlags, ConsumerFlagAttached|ConsumerFlagGPUShare)
	if !p.ConsumerAllowsGPUShare() {
		t.Fatal("GPU sharing disabled for capable consumer")
	}

	c, err := p.Acquire(1280, 720)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	idx := c.Index()
	const fakeHandle uintptr = 0x12345678
	const fakeLUID uint64 = 0x00000001_0000abcd
	c.CommitGPU(fakeHandle, fakeLUID)

	slotIdx, format, ok := p.LatestSlot()
	if !ok || slotIdx != idx || format != FormatDXGIBGRA {
		t.Fatalf("LatestSlot = (%d, %d, %v), want (%d, %d, true)", slotIdx, format, ok, idx, FormatDXGIBGRA)
	}
	if _, ok := p.Latest(); ok {
		t.Fatal("Latest() returned CPU pixels for a GPU-only slot")
	}
	if got := p.ctrl.Slots[idx].SharedHandle; got != uint64(fakeHandle) {
		t.Fatalf("SharedHandle = %#x, want %#x", got, fakeHandle)
	}
	if got := atomic.LoadUint64(&p.ctrl.AdapterLUID); got != fakeLUID {
		t.Fatalf("AdapterLUID = %#x, want %#x", got, fakeLUID)
	}

	// Simulate UI signaling cross-adapter mismatch via ConsumerFlagNoGPUShare.
	atomic.StoreUint32(&p.ctrl.ConsumerFlags, ConsumerFlagAttached|ConsumerFlagGPUShare|ConsumerFlagNoGPUShare)
	if p.ConsumerAllowsGPUShare() {
		t.Fatal("ConsumerAllowsGPUShare = true after ConsumerFlagNoGPUShare set")
	}

	// Subsequent CPU frame clears SharedHandle on the newly committed slot and
	// restores Latest() CPU readback.
	rgba := make([]byte, 16*8*4)
	rgba[0] = 0x77
	if err := p.Publish(16, 8, 16*4, rgba); err != nil {
		t.Fatalf("Publish CPU fallback: %v", err)
	}
	slotIdx, format, ok = p.LatestSlot()
	if !ok || format != FormatRGBA {
		t.Fatalf("LatestSlot after CPU publish = (%d, %d, %v)", slotIdx, format, ok)
	}
	if got := p.ctrl.Slots[slotIdx].SharedHandle; got != 0 {
		t.Fatalf("CPU slot retained SharedHandle = %#x", got)
	}
	if got := atomic.LoadUint64(&p.ctrl.AdapterLUID); got != 0 {
		t.Fatalf("CPU publish retained AdapterLUID = %#x", got)
	}
	pic, ok := p.Latest()
	if !ok || pic.Pixels[0] != 0x77 {
		t.Fatalf("Latest() after CPU fallback ok=%v", ok)
	}
}

func TestAcquirePreservesPublishedSlot(t *testing.T) {
	p, err := NewPublisher(`Local\YingpingFramesTestPreservesPublished`, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()
	frame, err := p.Acquire(16, 16)
	if err != nil {
		t.Fatal(err)
	}
	frame.CommitGPU(1, 1)
	for i := range SlotCount {
		if i != frame.Index() {
			atomic.StoreUint32(&p.ctrl.Slots[i].Claim, claimReader)
		}
	}
	if c, err := p.Acquire(16, 16); !errors.Is(err, ErrNoSlot) {
		c.Cancel()
		t.Fatalf("Acquire while other slots are held = %v, want ErrNoSlot", err)
	}
	if i, format, ok := p.LatestSlot(); !ok || i != frame.Index() || format != FormatDXGIBGRA {
		t.Fatalf("latest publication changed: index=%d format=%d ok=%v", i, format, ok)
	}
}

func TestRestartRetiresGPUPublication(t *testing.T) {
	const name = `Local\YingpingFramesTestGPURestart`
	first, err := NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	frame, err := first.Acquire(16, 16)
	if err != nil {
		t.Fatal(err)
	}
	frame.CommitGPU(1, 1)
	prevSeq := first.ctrl.Slots[frame.Index()].FrameSeq

	// The mapping stays open across producer restarts when the UI is attached.
	restarted, err := NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, _, ok := restarted.LatestSlot(); ok {
		t.Fatal("restarted producer still advertises a retired GPU handle")
	}
	if err := restarted.Publish(16, 16, 16*4, make([]byte, 16*16*4)); err != nil {
		t.Fatal(err)
	}
	if pic, ok := restarted.Latest(); !ok || pic.Seq <= prevSeq {
		t.Fatalf("first publication after restart returned seq=%d ok=%v, previous seq=%d", pic.Seq, ok, prevSeq)
	}
}
