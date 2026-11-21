// Package frames publishes decoded pictures to the Flutter process through a
// named shared-memory section.
//
// The layout and the publish protocol follow a verified design: a 4 KiB control
// page, then three frame slots, each 64 KiB aligned. A slot is claimed with a
// compare-and-swap before it is written and released only after the pixels are
// in place, so a reader can never observe a half-written frame. Publishing is
// latest-frame-wins: if the reader is busy the producer drops a frame rather
// than blocking.
//
// The control block is a C-compatible struct with fixed offsets. Both sides
// assert the offsets; see the test in this package and the static_asserts in
// frontend/windows/runner/frame_reader.cc.
package frames

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Magic identifies the mapping, so a consumer can reject a foreign section.
const Magic uint64 = 0x314D4853524D4641 // "AFRMSHM1" little endian

// Version is bumped when the layout changes incompatibly.
const Version = 1

// Layout constants.
const (
	HeaderBytes    = 0x1000
	SlotCount      = 3
	SlotDataOffset = 0x10000
	SlotAlign      = 0x10000
)

// Pixel formats. The Flutter pixel-buffer path uploads RGBA, so that is what
// the producer writes.
const (
	FormatRGBA uint32 = 1
	FormatBGRA uint32 = 2
)

// Claim states for a slot.
const (
	claimFree   uint32 = 0
	claimWriter uint32 = 1
	claimReader uint32 = 2
)

// slotEntry is the per-slot control block, exactly 64 bytes.
type slotEntry struct {
	Seq       uint64 // 0x00
	Claim     uint32 // 0x08
	Flags     uint32 // 0x0C
	FrameSeq  uint64 // 0x10
	Width     uint32 // 0x18
	Height    uint32 // 0x1C
	Stride    uint32 // 0x20
	Format    uint32 // 0x24
	WriteQPC  uint64 // 0x28
	WriterPID uint32 // 0x30
	Reserved0 uint32 // 0x34
	Reserved1 uint64 // 0x38
}

// control is the 4 KiB control page.
type control struct {
	Magic          uint64 // 0x000
	Version        uint32 // 0x008
	HeaderBytes    uint32 // 0x00C
	SlotCount      uint32 // 0x010
	SlotStride     uint32 // 0x014
	SlotDataOffset uint64 // 0x018
	Width          uint32 // 0x020
	Height         uint32 // 0x024
	Stride         uint32 // 0x028
	PixelFormat    uint32 // 0x02C
	Generation     uint64 // 0x030
	ProducerPID    uint32 // 0x038
	Flags          uint32 // 0x03C
	Publish        uint64 // 0x040
	FrameSeq       uint64 // 0x048
	LastPublishQPC uint64 // 0x050
	HeartbeatQPC   uint64 // 0x058
	FramePTS       uint64 // 0x060
	Dropped        uint64 // 0x068
	Reserved0      uint64 // 0x070
	Reserved1      uint64 // 0x078
	ConsumerPID    uint32 // 0x080
	ConsumerFlags  uint32 // 0x084
	ConsumerQPC    uint64 // 0x088
	ConsumerSlot   uint32 // 0x090
	Reserved2      uint32 // 0x094
	// ConsumerAdopted is the number of pictures the reader has adopted for
	// display. The C++ side owns it (frontend/.../frame_reader.cpp writes it as
	// the first Reserved word at 0x098) and the producer only reads it.
	ConsumerAdopted uint64                    // 0x098
	Reserved3       [12]uint64                // 0x0A0
	Slots           [SlotCount]slotEntry      // 0x100
	Tail            [HeaderBytes - 0x1C0]byte // 0x1C0
}

// QueryPerformanceCounter bindings. Every timestamp in the control block is a
// QPC tick, not wall-clock time: the producer and the UI process share one
// monotonic, high-resolution clock, so the two sides can compare stamps
// directly. time.Now is deliberately not used — its epoch differs from what
// the C++ reader can produce.
var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")

	qpcFreqOnce sync.Once
	qpcFreq     int64
)

// nowQPC reads the performance counter, or returns 0 when the call fails.
func nowQPC() int64 {
	var counter int64
	if r, _, _ := procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&counter))); r == 0 {
		return 0
	}
	return counter
}

// qpcFrequency reports ticks per second, cached after the first call.
func qpcFrequency() int64 {
	qpcFreqOnce.Do(func() {
		var freq int64
		if r, _, _ := procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&freq))); r == 0 || freq <= 0 {
			freq = 1
		}
		qpcFreq = freq
	})
	return qpcFreq
}

// qpcNanos converts a counter delta to nanoseconds. A non-positive delta is no
// sample, so it reports zero.
func qpcNanos(ticks int64) int64 {
	if ticks <= 0 {
		return 0
	}
	return int64(float64(ticks) * 1e9 / float64(qpcFrequency()))
}

// Publisher owns the mapping and writes frames into it.
type Publisher struct {
	name   string
	mapped windows.Handle
	base   uintptr
	ctrl   *control
	slots  uintptr // base + SlotDataOffset
	stride uint32  // bytes per slot

	event windows.Handle

	seq  uint64
	next int
	// current is the slot index most recently published, never reused for the
	// next write, so the reader's slot stays intact while it renders.
	current int

	// lastPublishNanos is the duration of the most recent Publish, in
	// nanoseconds, measured with QPC.
	lastPublishNanos atomic.Int64
}

// SlotStrideFor rounds one frame's bytes up to the slot alignment.
func SlotStrideFor(width, height int) uint32 {
	need := uint64(width) * uint64(height) * 4
	stride := (need + SlotAlign - 1) / SlotAlign * SlotAlign
	if stride < SlotAlign {
		stride = SlotAlign
	}
	return uint32(stride)
}

// MappingSize reports how many bytes the section needs for a display size.
func MappingSize(width, height int) uint64 {
	return SlotDataOffset + uint64(SlotCount)*uint64(SlotStrideFor(width, height))
}

// EventName derives the wake-up event's name from the mapping name.
func EventName(mapping string) string {
	return mapping + ".frame"
}

// SlotCapacity is the largest picture a slot accepts, independent of the
// display size we advertise. A sender picks its own mirroring resolution — an
// iPhone sends 498x1080 upright and 1920x884 sideways, an iPad sends more — so
// the slots are sized for the largest display we intend to support rather than
// for whatever we asked for. A pagefile-backed section commits pages only when
// they are written, so the unused capacity costs address space, not memory.
const (
	SlotCapacityWidth  = 4096
	SlotCapacityHeight = 2304
)

// NewPublisher creates the section and initialises the control block. width and
// height are the virtual display size we advertised; they are reported in the
// control block, while the slots are sized by SlotCapacity.
func NewPublisher(name string, width, height int) (*Publisher, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("frames: invalid display size %dx%d", width, height)
	}
	slotStride := SlotStrideFor(SlotCapacityWidth, SlotCapacityHeight)
	size := SlotDataOffset + uint64(SlotCount)*uint64(slotStride)
	if size > 1<<29 {
		return nil, fmt.Errorf("frames: slots need %d bytes, too large", size)
	}

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("frames: name: %w", err)
	}
	// An unnamed pagefile-backed section: the handle is not a file. When the UI
	// is still running across a sidecar restart, it keeps the existing mapping
	// open and CreateFileMapping returns ERROR_ALREADY_EXISTS with a valid
	// handle to that same section.
	mapped, err := windows.CreateFileMapping(
		windows.InvalidHandle, nil, windows.PAGE_READWRITE,
		uint32(size>>32), uint32(size), namePtr)
	reused := errors.Is(err, windows.ERROR_ALREADY_EXISTS)
	if err != nil && !reused {
		return nil, fmt.Errorf("frames: create section %q: %w", name, err)
	}
	base, err := windows.MapViewOfFile(mapped, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		_ = windows.CloseHandle(mapped)
		return nil, fmt.Errorf("frames: map section: %w", err)
	}

	p := &Publisher{
		name:    name,
		mapped:  mapped,
		base:    base,
		stride:  slotStride,
		next:    1,
		current: 0,
	}
	p.ctrl = (*control)(unsafe.Pointer(base))
	p.slots = base + SlotDataOffset

	if reused &&
		p.ctrl.Magic == Magic &&
		p.ctrl.Version == Version &&
		p.ctrl.SlotCount == SlotCount &&
		p.ctrl.SlotStride == slotStride &&
		p.ctrl.SlotDataOffset == SlotDataOffset {
		prevPublish := atomic.LoadUint64(&p.ctrl.Publish)
		p.seq = prevPublish >> 2
		if cur := int(prevPublish & 0x3); cur < SlotCount {
			p.current = cur
		}
		for i := 0; i < SlotCount; i++ {
			atomic.CompareAndSwapUint32(&p.ctrl.Slots[i].Claim, claimWriter, claimFree)
		}
		atomic.AddUint64(&p.ctrl.Generation, 1)
		atomic.StoreUint64(&p.ctrl.Dropped, 0)
		p.ctrl.Width = uint32(width)
		p.ctrl.Height = uint32(height)
		p.ctrl.Stride = uint32(width * 4)
		p.ctrl.PixelFormat = FormatRGBA
		p.ctrl.ProducerPID = uint32(windows.GetCurrentProcessId())
	} else {
		p.ctrl.Magic = Magic
		p.ctrl.Version = Version
		p.ctrl.HeaderBytes = HeaderBytes
		p.ctrl.SlotCount = SlotCount
		p.ctrl.SlotStride = p.stride
		p.ctrl.SlotDataOffset = SlotDataOffset
		p.ctrl.Width = uint32(width)
		p.ctrl.Height = uint32(height)
		p.ctrl.Stride = uint32(width * 4)
		p.ctrl.PixelFormat = FormatRGBA
		p.ctrl.Generation = 1
		p.ctrl.ProducerPID = uint32(windows.GetCurrentProcessId())
	}

	// Wake-up hint for the Flutter side. Failure is not fatal: the consumer also
	// polls Publish, so a missing event costs latency, not correctness.
	evName, _ := windows.UTF16PtrFromString(EventName(name))
	if ev, err := windows.CreateEvent(nil, 0, 0, evName); err == nil || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		p.event = ev
	}
	return p, nil
}

// Name reports the mapping name.
func (p *Publisher) Name() string { return p.name }

// SlotStride reports the size of one slot.
func (p *Publisher) SlotStride() uint32 { return p.stride }

// Dropped reports how many pictures were skipped because every slot was busy.
// A non-zero count means the consumer is not keeping up, which is the difference
// between "the sender stopped" and "we could not display it".
//
// With three slots and a reader that holds one, this counter can only fire if
// the writer itself failed, so it is structurally near-always zero. Use
// ConsumerStats.LagFrames for the honest producer→consumer drop count.
func (p *Publisher) Dropped() uint64 {
	if p.ctrl == nil {
		return 0
	}
	return atomic.LoadUint64(&p.ctrl.Dropped)
}

// Heartbeat stamps the producer's liveness in the control block. The frame sink
// calls it for every access unit, including invalid ones, so a stalled decoder
// is distinguishable from a stalled sender.
func (p *Publisher) Heartbeat() {
	if p.ctrl == nil {
		return
	}
	atomic.StoreUint64(&p.ctrl.HeartbeatQPC, uint64(nowQPC()))
}

// LastPublishNanos reports how long the most recent Publish took, in
// nanoseconds: claiming a slot, copying the pixels and publishing the word.
// Zero before the first picture.
func (p *Publisher) LastPublishNanos() int64 { return p.lastPublishNanos.Load() }

// ConsumerStats is one snapshot of the producer→consumer progress words. The UI
// process writes the consumer fields from its raster thread with interlocked
// operations; the producer only reads them here.
type ConsumerStats struct {
	Published        uint64 // pictures the producer announced (control FrameSeq)
	Adopted          uint64 // pictures the reader took for display
	LagFrames        uint64 // Published - Adopted while a reader is attached
	ConsumerPID      uint32 // the UI process, or 0 before it mapped the section
	ConsumerSlot     uint32 // the slot index the reader last adopted
	ConsumerAgeNanos int64  // age of the reader's last adoption, or 0
	RawDropped       uint64 // the legacy claim-contention counter
}

// ConsumerStats reads the progress words. This is the real lag metric: because
// publishing is latest-frame-wins, every announced picture the reader did not
// adopt is a picture that never reached the screen. Lag is only defined once a
// reader has adopted at least one picture, so a UI that has not attached yet
// does not look like it is dropping everything.
func (p *Publisher) ConsumerStats() ConsumerStats {
	if p.ctrl == nil {
		return ConsumerStats{}
	}
	published := atomic.LoadUint64(&p.ctrl.FrameSeq)
	adopted := atomic.LoadUint64(&p.ctrl.ConsumerAdopted)
	var lag uint64
	if adopted > 0 && published > adopted {
		lag = published - adopted
	}
	var age int64
	if consumerQPC := atomic.LoadUint64(&p.ctrl.ConsumerQPC); consumerQPC != 0 {
		if now := nowQPC(); now > 0 && uint64(now) >= consumerQPC {
			age = qpcNanos(int64(uint64(now) - consumerQPC))
		}
	}
	return ConsumerStats{
		Published:        published,
		Adopted:          adopted,
		LagFrames:        lag,
		ConsumerPID:      atomic.LoadUint32(&p.ctrl.ConsumerPID),
		ConsumerSlot:     atomic.LoadUint32(&p.ctrl.ConsumerSlot),
		ConsumerAgeNanos: age,
		RawDropped:       atomic.LoadUint64(&p.ctrl.Dropped),
	}
}

// StatsWindow is how many recent samples the rolling aggregates keep. At 60 fps
// that is about two seconds of history.
const StatsWindow = 120

// RollingStats keeps the last StatsWindow samples and reports their mean and
// nearest-rank p95. It is not safe for concurrent use; the caller serialises
// access. The zero value is ready to use.
type RollingStats struct {
	samples [StatsWindow]float64
	count   int
	next    int
}

// Add records one sample, evicting the oldest once the window is full.
func (r *RollingStats) Add(v float64) {
	r.samples[r.next] = v
	r.next = (r.next + 1) % StatsWindow
	if r.count < StatsWindow {
		r.count++
	}
}

// Reset forgets every sample.
func (r *RollingStats) Reset() {
	r.count = 0
	r.next = 0
}

// Len reports how many samples the window currently holds.
func (r *RollingStats) Len() int { return r.count }

// Mean reports the arithmetic mean of the samples, or zero when empty.
func (r *RollingStats) Mean() float64 {
	if r.count == 0 {
		return 0
	}
	var sum float64
	for _, v := range r.window() {
		sum += v
	}
	return sum / float64(r.count)
}

// P95 reports the nearest-rank 95th percentile, or zero when empty.
func (r *RollingStats) P95() float64 {
	if r.count == 0 {
		return 0
	}
	sorted := append([]float64(nil), r.window()...)
	sort.Float64s(sorted)
	// nearest rank: ceil(0.95*n), clamped to the last index.
	idx := (95*r.count+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// window returns the live samples. For an unfilled window it aliases the
// internal array's prefix, so callers must treat the result as read-only.
func (r *RollingStats) window() []float64 {
	if r.count < StatsWindow {
		return r.samples[:r.count]
	}
	return r.samples[:]
}

// Publish writes one picture. pixels must be tightly packed rows of width*4
// bytes; imgStride is the row stride inside pixels.
//
// It never blocks: when no slot is free the frame is dropped and recorded in the
// control block.
func (p *Publisher) Publish(width, height, imgStride int, pixels []byte) error {
	if width <= 0 || height <= 0 {
		return errors.New("frames: empty image")
	}
	rowBytes := width * 4
	if imgStride < rowBytes {
		return fmt.Errorf("frames: stride %d shorter than a row (%d)", imgStride, rowBytes)
	}
	if need := uint64(imgStride) * uint64(height); need > uint64(p.stride) {
		return fmt.Errorf("frames: frame of %dx%d needs %d bytes, slot holds %d", width, height, need, p.stride)
	}

	start := nowQPC()
	i := p.claimSlot()
	if i < 0 {
		atomic.AddUint64(&p.ctrl.Dropped, 1)
		return nil // dropped, not an error: this is the designed backpressure
	}

	slot := &p.ctrl.Slots[i]
	p.seq++
	slot.Seq++
	slot.FrameSeq = p.seq
	slot.Width = uint32(width)
	slot.Height = uint32(height)
	slot.Stride = uint32(rowBytes)
	slot.Format = FormatRGBA
	slot.WriterPID = p.ctrl.ProducerPID
	slot.WriteQPC = uint64(nowQPC())

	dst := unsafe.Slice((*byte)(unsafe.Pointer(p.slots+uintptr(i)*uintptr(p.stride))), int(p.stride))
	if imgStride == rowBytes {
		copy(dst[:width*height*4], pixels)
	} else {
		for y := 0; y < height; y++ {
			copy(dst[y*rowBytes:(y+1)*rowBytes], pixels[y*imgStride:y*imgStride+rowBytes])
		}
	}

	// The publish word carries the slot index so a reader never has to guess
	// which slot the new metadata belongs to.
	atomic.StoreUint64(&p.ctrl.Publish, p.seq<<2|uint64(i))
	atomic.AddUint64(&p.ctrl.FrameSeq, 1)
	published := nowQPC()
	atomic.StoreUint64(&p.ctrl.LastPublishQPC, uint64(published))
	atomic.StoreUint64(&p.ctrl.HeartbeatQPC, uint64(published))
	if start > 0 && published > start {
		p.lastPublishNanos.Store(qpcNanos(published - start))
	}
	atomic.StoreUint32(&slot.Claim, claimFree)
	p.current = i

	if p.event != 0 {
		_ = windows.SetEvent(p.event)
	}
	return nil
}

// claimSlot CASes one of the two slots that are not currently published into the
// writer state. With three slots and one reader that holds at most one, a free
// slot always exists; the loop is belt-and-braces.
func (p *Publisher) claimSlot() int {
	for attempt := 0; attempt < SlotCount; attempt++ {
		i := (p.current + 1 + attempt) % SlotCount
		var expected uint32 = claimFree
		if atomic.CompareAndSwapUint32(&p.ctrl.Slots[i].Claim, expected, claimWriter) {
			return i
		}
	}
	return -1
}

// Close unmaps the section. The mapping name disappears with the last handle.
func (p *Publisher) Close() {
	if p.event != 0 {
		_ = windows.CloseHandle(p.event)
		p.event = 0
	}
	if p.base != 0 {
		_ = windows.UnmapViewOfFile(p.base)
		p.base = 0
		p.ctrl = nil
	}
	if p.mapped != 0 {
		_ = windows.CloseHandle(p.mapped)
		p.mapped = 0
	}
}

// ensure the control block is exactly the documented size.
var _ = [1]struct{}{}[unsafe.Sizeof(control{})-HeaderBytes]
var _ = [1]struct{}{}[unsafe.Sizeof(slotEntry{})-64]

// MagicBytes returns the magic in the order it appears in memory, for debugging.
func MagicBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], Magic)
	return b
}
