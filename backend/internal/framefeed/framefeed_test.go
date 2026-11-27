//go:build windows

package framefeed

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Miku0139oao/MirrPane/backend/internal/frames"
	"golang.org/x/sys/windows"
)

func testSectionName(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf(`Local\YingpingFramefeedTest_%d_%s_%d`, windows.GetCurrentProcessId(), suffix, time.Now().UnixNano())
}

func makeSolidRGBA(w, h int, v byte) []byte {
	buf := make([]byte, w*h*4)
	for i := range buf {
		buf[i] = v
	}
	return buf
}

func countReaderSlots(ctrl *control) int {
	n := 0
	for i := 0; i < slotCount; i++ {
		if atomic.LoadUint32(&ctrl.Slots[i].Claim) == claimReader {
			n++
		}
	}
	return n
}

func TestLayoutContract(t *testing.T) {
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
		{"Reserved1", unsafe.Offsetof(c.Reserved1), 0x078},
		{"ConsumerPID", unsafe.Offsetof(c.ConsumerPID), 0x080},
		{"ConsumerFlags", unsafe.Offsetof(c.ConsumerFlags), 0x084},
		{"ConsumerQPC", unsafe.Offsetof(c.ConsumerQPC), 0x088},
		{"ConsumerSlot", unsafe.Offsetof(c.ConsumerSlot), 0x090},
		{"Reserved2", unsafe.Offsetof(c.Reserved2), 0x094},
		{"ConsumerAdopted", unsafe.Offsetof(c.ConsumerAdopted), 0x098},
		{"Reserved3", unsafe.Offsetof(c.Reserved3), 0x0A0},
		{"Slots", unsafe.Offsetof(c.Slots), 0x100},
		{"Tail", unsafe.Offsetof(c.Tail), 0x1C0},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("control.%s offset = 0x%03X, want 0x%03X", f.name, f.got, f.want)
		}
	}

	var s slotEntry
	slotFields := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Seq", unsafe.Offsetof(s.Seq), 0x00},
		{"Claim", unsafe.Offsetof(s.Claim), 0x08},
		{"Flags", unsafe.Offsetof(s.Flags), 0x0C},
		{"FrameSeq", unsafe.Offsetof(s.FrameSeq), 0x10},
		{"Width", unsafe.Offsetof(s.Width), 0x18},
		{"Height", unsafe.Offsetof(s.Height), 0x1C},
		{"Stride", unsafe.Offsetof(s.Stride), 0x20},
		{"Format", unsafe.Offsetof(s.Format), 0x24},
		{"WriteQPC", unsafe.Offsetof(s.WriteQPC), 0x28},
		{"WriterPID", unsafe.Offsetof(s.WriterPID), 0x30},
		{"Reserved0", unsafe.Offsetof(s.Reserved0), 0x34},
		{"SharedHandle", unsafe.Offsetof(s.SharedHandle), 0x38},
	}
	for _, f := range slotFields {
		if f.got != f.want {
			t.Errorf("slotEntry.%s offset = 0x%02X, want 0x%02X", f.name, f.got, f.want)
		}
	}

	if got := unsafe.Sizeof(control{}); got != headerBytes {
		t.Errorf("sizeof(control) = %#x, want %#x", got, headerBytes)
	}
	if got := unsafe.Sizeof(slotEntry{}); got != 64 {
		t.Errorf("sizeof(slotEntry) = %d, want 64", got)
	}
}

func TestOpenValidation(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want error")
	}
	if _, err := Open(testSectionName(t, "missing")); err == nil {
		t.Fatal("Open on missing section succeeded, want error")
	}

	// Create a foreign mapping with invalid header magic and confirm Open rejects it.
	badName := testSectionName(t, "badmagic")
	namePtr, err := windows.UTF16PtrFromString(badName)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, headerBytes, namePtr)
	if err != nil {
		t.Fatalf("CreateFileMapping: %v", err)
	}
	defer windows.CloseHandle(h)

	if _, err := Open(badName); err == nil {
		t.Fatal("Open on zeroed/invalid header succeeded, want error")
	}
}

func TestLatestAdoptsNewestAndDoubleBufferHold(t *testing.T) {
	name := testSectionName(t, "adopt")
	p, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()

	r, err := Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if r.event == 0 {
		t.Fatal("expected wake event to be opened")
	}
	if p.ConsumerAllowsGPUShare() {
		t.Fatal("ConsumerAllowsGPUShare = true, want false for CPU RGBA reader")
	}
	if got := atomic.LoadUint32(&r.ctrl.ConsumerFlags); got != consumerFlagAttached|consumerFlagNoGPUShare {
		t.Fatalf("ConsumerFlags = %#x, want %#x", got, consumerFlagAttached|consumerFlagNoGPUShare)
	}
	if got := atomic.LoadUint32(&r.ctrl.ConsumerPID); got != uint32(windows.GetCurrentProcessId()) {
		t.Fatalf("ConsumerPID = %d, want %d", got, windows.GetCurrentProcessId())
	}

	if _, ok := r.Latest(); ok {
		t.Fatal("Latest() returned frame before any publish")
	}

	// Publish frame 1 and adopt it.
	if err := p.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0x11)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	f1, ok := r.Latest()
	if !ok {
		t.Fatal("Latest() returned false after first publish")
	}
	if f1.Width != 16 || f1.Height != 8 || f1.Stride != 16*4 || f1.Seq != 1 {
		t.Fatalf("f1 metadata = %dx%d stride=%d seq=%d", f1.Width, f1.Height, f1.Stride, f1.Seq)
	}
	if len(f1.Pixels) != 16*8*4 || f1.Pixels[0] != 0x11 || f1.Pixels[len(f1.Pixels)-1] != 0x11 {
		t.Fatalf("f1 pixels unexpected: len=%d first=%#x last=%#x", len(f1.Pixels), f1.Pixels[0], f1.Pixels[len(f1.Pixels)-1])
	}

	slot1 := r.heldSlot
	if slot1 < 0 || slot1 >= slotCount {
		t.Fatalf("invalid heldSlot %d", slot1)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[slot1].Claim); got != claimReader {
		t.Fatalf("slot1 Claim = %d, want claimReader", got)
	}
	if held := countReaderSlots(r.ctrl); held == 0 || held > 2 {
		t.Fatalf("reader-held slots = %d, want 1..2", held)
	}

	stats := p.ConsumerStats()
	if stats.Adopted != 1 || stats.ConsumerSlot != uint32(slot1) || stats.ConsumerPID != uint32(windows.GetCurrentProcessId()) {
		t.Fatalf("ConsumerStats after frame 1 = %+v (want Adopted=1 ConsumerSlot=%d)", stats, slot1)
	}
	if atomic.LoadUint64(&r.ctrl.ConsumerQPC) == 0 {
		t.Fatal("ConsumerQPC was not stamped on adoption")
	}

	// Polling Latest() with no new publish keeps the held frame without re-adopting.
	again, ok := r.Latest()
	if !ok || again.Seq != f1.Seq {
		t.Fatalf("idle Latest() = (seq=%d, ok=%v), want seq=%d", again.Seq, ok, f1.Seq)
	}
	if got := p.ConsumerStats().Adopted; got != 1 {
		t.Fatalf("Adopted incremented on idle Latest(): got %d, want 1", got)
	}

	// Publish two newer frames while frame 1 is still held; the producer must
	// never overwrite slot1 and must not drop frames.
	if err := p.Publish(24, 12, 24*4, makeSolidRGBA(24, 12, 0x22)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if err := p.Publish(32, 16, 32*4, makeSolidRGBA(32, 16, 0x33)); err != nil {
		t.Fatalf("publish 3: %v", err)
	}
	if dropped := p.Dropped(); dropped != 0 {
		t.Fatalf("Dropped = %d while reader held 1 slot, want 0", dropped)
	}
	for i, b := range f1.Pixels {
		if b != 0x11 {
			t.Fatalf("held f1 pixel[%d] was overwritten: got %#x, want 0x11", i, b)
		}
	}

	// Adopting frame 3 adopts the newest publish and releases slot1.
	f3, ok := r.Latest()
	if !ok {
		t.Fatal("Latest() returned false after frame 3")
	}
	if f3.Width != 32 || f3.Height != 16 || f3.Stride != 32*4 || f3.Seq != 3 {
		t.Fatalf("f3 metadata = %dx%d stride=%d seq=%d, want 32x16 seq=3", f3.Width, f3.Height, f3.Stride, f3.Seq)
	}
	if f3.Pixels[0] != 0x33 || f3.Pixels[len(f3.Pixels)-1] != 0x33 {
		t.Fatalf("f3 pixels = (%#x..%#x), want 0x33", f3.Pixels[0], f3.Pixels[len(f3.Pixels)-1])
	}
	slot3 := r.heldSlot
	if slot3 == slot1 {
		t.Fatalf("slot3 == slot1 (%d); producer reused held slot", slot3)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[slot1].Claim); got != claimFree {
		t.Fatalf("previous slot %d Claim = %d after next adopt, want claimFree", slot1, got)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[slot3].Claim); got != claimReader {
		t.Fatalf("new slot %d Claim = %d, want claimReader", slot3, got)
	}
	if held := countReaderSlots(r.ctrl); held == 0 || held > 2 {
		t.Fatalf("reader-held slots = %d, want 1..2", held)
	}
	stats = p.ConsumerStats()
	if stats.Adopted != 2 || stats.ConsumerSlot != uint32(slot3) {
		t.Fatalf("ConsumerStats after frame 3 = %+v (want Adopted=2 ConsumerSlot=%d)", stats, slot3)
	}

	// Close must release slot3 so another reader (or producer) sees all slots free.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := r.Latest(); ok {
		t.Fatal("Latest() succeeded after Close()")
	}
	r2, err := Open(name)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	defer r2.Close()
	if got := atomic.LoadUint32(&r2.ctrl.Slots[slot3].Claim); got != claimFree {
		t.Fatalf("slot3 Claim after Close = %d, want claimFree", got)
	}
}

func TestSkipsDXGISlot(t *testing.T) {
	name := testSectionName(t, "dxgi")
	p, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()

	r, err := Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	// DXGI frame before any RGBA frame is skipped and returns ok=false.
	c1, err := p.Acquire(16, 16)
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	dxgiSlot1 := c1.Index()
	c1.CommitGPU(0x12345678, 0x00000001_0000abcd)

	if _, ok := r.Latest(); ok {
		t.Fatal("Latest() returned DXGI slot as RGBA")
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[dxgiSlot1].Claim); got != claimFree {
		t.Fatalf("skipped DXGI slot Claim = %d, want claimFree", got)
	}
	if got := p.ConsumerStats().Adopted; got != 0 {
		t.Fatalf("Adopted after skipped DXGI = %d, want 0", got)
	}

	// Publish an RGBA frame, adopt it, then publish a DXGI frame.
	if err := p.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0x55)); err != nil {
		t.Fatalf("Publish RGBA: %v", err)
	}
	rgbaFrame, ok := r.Latest()
	if !ok || rgbaFrame.Pixels[0] != 0x55 {
		t.Fatalf("Latest() RGBA ok=%v", ok)
	}
	seqRGBA := rgbaFrame.Seq
	if got := p.ConsumerStats().Adopted; got != 1 {
		t.Fatalf("Adopted after RGBA = %d, want 1", got)
	}

	c2, err := p.Acquire(32, 32)
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	dxgiSlot2 := c2.Index()
	c2.CommitGPU(0xdeadbeef, 0x00000001_0000abcd)

	// DXGI slot is skipped; Reader keeps returning the held RGBA frame.
	stillRGBA, ok := r.Latest()
	if !ok {
		t.Fatal("Latest() returned false when an RGBA frame was already held")
	}
	if stillRGBA.Seq != seqRGBA || stillRGBA.Pixels[0] != 0x55 {
		t.Fatalf("Latest() returned seq=%d byte=%#x instead of held RGBA seq=%d", stillRGBA.Seq, stillRGBA.Pixels[0], seqRGBA)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[dxgiSlot2].Claim); got != claimFree {
		t.Fatalf("skipped DXGI slot2 Claim = %d, want claimFree", got)
	}
	if got := p.ConsumerStats().Adopted; got != 1 {
		t.Fatalf("Adopted changed on skipped DXGI slot2: got %d, want 1", got)
	}

	// Subsequent RGBA frame is adopted normally.
	if err := p.Publish(20, 10, 20*4, makeSolidRGBA(20, 10, 0x66)); err != nil {
		t.Fatalf("Publish RGBA 2: %v", err)
	}
	nextRGBA, ok := r.Latest()
	if !ok || nextRGBA.Seq <= seqRGBA || nextRGBA.Pixels[0] != 0x66 {
		t.Fatalf("Latest() after fallback ok=%v seq=%d (prev %d)", ok, nextRGBA.Seq, seqRGBA)
	}
	if got := p.ConsumerStats().Adopted; got != 2 {
		t.Fatalf("Adopted after second RGBA = %d, want 2", got)
	}
}

func TestProducerRestartGenerationBump(t *testing.T) {
	name := testSectionName(t, "restart")
	p1, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher 1: %v", err)
	}

	r, err := Open(name)
	if err != nil {
		p1.Close()
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if err := p1.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0xAA)); err != nil {
		p1.Close()
		t.Fatalf("p1 Publish: %v", err)
	}
	f1, ok := r.Latest()
	if !ok || f1.Pixels[0] != 0xAA {
		p1.Close()
		t.Fatalf("p1 Latest ok=%v", ok)
	}
	gen1 := atomic.LoadUint64(&r.ctrl.Generation)
	adopted1 := p1.ConsumerStats().Adopted

	// Close p1 while r keeps the section and wake event open, then reopen p2.
	p1.Close()
	p2, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher 2: %v", err)
	}
	defer p2.Close()

	gen2 := atomic.LoadUint64(&r.ctrl.Generation)
	if gen2 <= gen1 {
		t.Fatalf("Generation did not bump on restart: gen1=%d gen2=%d", gen1, gen2)
	}

	// The restart must neither release nor re-adopt the picture on screen: the
	// slot stays claimed, the pixels stay, and nothing is uploaded again for
	// the same sequence.
	held := r.heldSlot
	kept, ok := r.Latest()
	if !ok || kept.Seq != f1.Seq || kept.Pixels[0] != 0xAA {
		t.Fatalf("Latest immediately after restart: ok=%v seq=%d", ok, kept.Seq)
	}
	if r.heldSlot != held {
		t.Fatalf("restart moved the held slot %d -> %d", held, r.heldSlot)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[held].Claim); got != claimReader {
		t.Fatalf("held slot %d Claim after restart = %d, want claimReader", held, got)
	}
	if got := p2.ConsumerStats().Adopted; got != adopted1 {
		t.Fatalf("Adopted after restart = %d, want %d (no re-adoption)", got, adopted1)
	}

	// New frames published by p2 are adopted cleanly.
	prevSlot := r.heldSlot
	if err := p2.Publish(24, 12, 24*4, makeSolidRGBA(24, 12, 0xBB)); err != nil {
		t.Fatalf("p2 Publish: %v", err)
	}
	f2, ok := r.Latest()
	if !ok || f2.Seq <= f1.Seq || f2.Pixels[0] != 0xBB {
		t.Fatalf("p2 Latest ok=%v seq=%d (prev %d)", ok, f2.Seq, f1.Seq)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[prevSlot].Claim); got != claimFree {
		t.Fatalf("prevSlot %d Claim after p2 adopt = %d, want claimFree", prevSlot, got)
	}
	if got := p2.ConsumerStats().Adopted; got != adopted1+1 {
		t.Fatalf("Adopted after p2 publish = %d, want %d", got, adopted1+1)
	}
}

// TestGenerationBumpKeepsPictureWhileProducerPublishes covers a restart while
// the reader holds a frame that is not the published one (the case a DXGI-only
// publish leaves behind): the restart must not free that slot for the new
// producer to overwrite under a picture the UI still shows.
func TestGenerationBumpKeepsHeldSlotAcrossPublishes(t *testing.T) {
	name := testSectionName(t, "holdgen")
	p1, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher 1: %v", err)
	}
	r, err := Open(name)
	if err != nil {
		p1.Close()
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if err := p1.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0xAA)); err != nil {
		p1.Close()
		t.Fatalf("p1 Publish: %v", err)
	}
	f1, ok := r.Latest()
	if !ok || f1.Pixels[0] != 0xAA {
		p1.Close()
		t.Fatalf("p1 Latest ok=%v", ok)
	}
	held := r.heldSlot
	adopted1 := p1.ConsumerStats().Adopted

	p1.Close()
	p2, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher 2: %v", err)
	}
	defer p2.Close()

	// Restart without a new picture: the same frame, no new adoption.
	if kept, ok := r.Latest(); !ok || kept.Seq != f1.Seq {
		t.Fatalf("Latest after restart ok=%v seq=%d", ok, kept.Seq)
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[held].Claim); got != claimReader {
		t.Fatalf("held slot %d Claim = %d, want claimReader", held, got)
	}
	if got := p2.ConsumerStats().Adopted; got != adopted1 {
		t.Fatalf("Adopted = %d after restart, want %d", got, adopted1)
	}

	// Two newer pictures fill the other slots; the one on screen must not move.
	if err := p2.Publish(24, 12, 24*4, makeSolidRGBA(24, 12, 0xBB)); err != nil {
		t.Fatalf("p2 Publish 1: %v", err)
	}
	if err := p2.Publish(32, 16, 32*4, makeSolidRGBA(32, 16, 0xCC)); err != nil {
		t.Fatalf("p2 Publish 2: %v", err)
	}
	for i, b := range f1.Pixels {
		if b != 0xAA {
			t.Fatalf("picture on screen changed at byte %d: %#x, want 0xAA", i, b)
		}
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[held].Claim); got != claimReader {
		t.Fatalf("held slot %d Claim = %d while presenting, want claimReader", held, got)
	}

	// The newest picture is then adopted, and releases the old slot.
	f3, ok := r.Latest()
	if !ok || f3.Seq <= f1.Seq || f3.Pixels[0] != 0xCC {
		t.Fatalf("Latest after p2 publishes ok=%v seq=%d byte=%#x", ok, f3.Seq, f3.Pixels[0])
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[held].Claim); got != claimFree {
		t.Fatalf("held slot %d Claim after adopt = %d, want claimFree", held, got)
	}
	if got := p2.ConsumerStats().Adopted; got != adopted1+1 {
		t.Fatalf("Adopted after adopt = %d, want %d", got, adopted1+1)
	}
}

// TestLayoutChangeStopsPictures simulates a producer that replaced the section
// with an incompatible slot layout: the reader must stop handing out pictures
// (rather than slice the new memory with the old geometry) and report Stale.
func TestLayoutChangeStopsPictures(t *testing.T) {
	name := testSectionName(t, "layout")
	p, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()
	r, err := Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if err := p.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0x11)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := r.Latest(); !ok {
		t.Fatal("Latest before the layout change returned false")
	}
	held := r.heldSlot

	// A restarted producer with another slot stride bumps the generation after
	// rewriting the header.
	atomic.StoreUint32(&r.ctrl.SlotStride, r.slotStride+0x10000)
	atomic.AddUint64(&r.ctrl.Generation, 1)

	if _, ok := r.Latest(); ok {
		t.Fatal("Latest handed out a picture after an incompatible layout change")
	}
	if !r.Stale() {
		t.Fatal("Stale = false after an incompatible layout change")
	}
	if got := atomic.LoadUint32(&r.ctrl.Slots[held].Claim); got != claimFree {
		t.Fatalf("held slot %d Claim = %d, want claimFree", held, got)
	}
	if _, ok := r.Latest(); ok {
		t.Fatal("Latest kept handing out pictures after going stale")
	}
}

func TestCloseSafeWhileProducerPublishes(t *testing.T) {
	name := testSectionName(t, "concurrent")
	p, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()

	r, err := Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		img := makeSolidRGBA(32, 32, 0x77)
		for {
			select {
			case <-stop:
				return
			default:
				if err := p.Publish(32, 32, 32*4, img); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
			}
		}
	}()

	for i := 0; i < 50; i++ {
		_, _ = r.Latest()
		time.Sleep(100 * time.Microsecond)
	}
	if err := r.Close(); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("Close: %v", err)
	}
	// Idempotent second Close.
	if err := r.Close(); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("second Close: %v", err)
	}

	close(stop)
	wg.Wait()

	// Reopen to confirm no slots remain stuck in claimReader.
	r2, err := Open(name)
	if err != nil {
		t.Fatalf("Open after concurrent Close: %v", err)
	}
	defer r2.Close()
	if held := countReaderSlots(r2.ctrl); held != 0 {
		t.Fatalf("reader-held slots after Close = %d, want 0", held)
	}
}

func TestWaitWakeEventAndCloseUnblock(t *testing.T) {
	name := testSectionName(t, "wait")
	p, err := frames.NewPublisher(name, 64, 64)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer p.Close()

	r, err := Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	woke := make(chan bool, 1)
	go func() {
		woke <- r.Wait(2 * time.Second)
	}()
	time.Sleep(15 * time.Millisecond)
	if err := p.Publish(16, 8, 16*4, makeSolidRGBA(16, 8, 0x99)); err != nil {
		r.Close()
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ok := <-woke:
		if !ok {
			r.Close()
			t.Fatal("Wait returned false on Publish wakeup")
		}
	case <-time.After(time.Second):
		r.Close()
		t.Fatal("Wait did not wake on Publish")
	}

	f, ok := r.Latest()
	if !ok || f.Pixels[0] != 0x99 {
		r.Close()
		t.Fatalf("Latest after Wait ok=%v", ok)
	}

	// Close must unblock a pending Wait immediately.
	go func() {
		woke <- r.Wait(5 * time.Second)
	}()
	time.Sleep(15 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case ok := <-woke:
		if ok {
			t.Fatal("Wait returned true on Close, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Wait")
	}
}
