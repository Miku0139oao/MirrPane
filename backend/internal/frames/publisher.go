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
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrNoSlot means every slot is currently held, so the picture was dropped.
// Publishing is latest-frame-wins: this is backpressure, not a failure.
var ErrNoSlot = errors.New("frames: no free slot")

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

// Pixel formats. The Flutter pixel-buffer path uploads RGBA, while the GPU
// surface path shares a DXGI B8G8R8A8_UNORM texture handle.
const (
	FormatRGBA     uint32 = 1
	FormatBGRA     uint32 = 2
	FormatDXGIBGRA uint32 = 3
)

// Consumer capability flags stamped into control.ConsumerFlags by the UI.
// Readers opt in to GPU slots and can request CPU RGBA if importing fails.
const (
	ConsumerFlagAttached   uint32 = 1 << 0
	ConsumerFlagNoGPUShare uint32 = 1 << 1
	ConsumerFlagGPUShare   uint32 = 1 << 2
)

// Claim states for a slot.
const (
	claimFree   uint32 = 0
	claimWriter uint32 = 1
	claimReader uint32 = 2
)

// slotEntry is the per-slot control block, exactly 64 bytes.
type slotEntry struct {
	Seq          uint64 // 0x00
	Claim        uint32 // 0x08
	Flags        uint32 // 0x0C
	FrameSeq     uint64 // 0x10
	Width        uint32 // 0x18
	Height       uint32 // 0x1C
	Stride       uint32 // 0x20
	Format       uint32 // 0x24
	WriteQPC     uint64 // 0x28
	WriterPID    uint32 // 0x30
	Reserved0    uint32 // 0x34
	SharedHandle uint64 // 0x38 (DXGI shared HANDLE when Format == FormatDXGIBGRA)
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
	AdapterLUID    uint64 // 0x070 (producer DXGI adapter LUID when GPU sharing is active)
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
	slots  unsafe.Pointer // base + SlotDataOffset
	stride uint32         // bytes per slot

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
	// MapViewOfFile returns a uintptr. unsafe.Add on a nil base converts it
	// without an explicit uintptr→unsafe.Pointer conversion, which go vet's
	// unsafeptr check rejects even though this is the only way to name a
	// mapped view in Go.
	p.ctrl = (*control)(unsafe.Add(unsafe.Pointer(nil), base))
	p.slots = unsafe.Add(unsafe.Pointer(p.ctrl), SlotDataOffset)

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
			if prevPublish != 0 && p.ctrl.Slots[cur].Format == FormatDXGIBGRA {
				// The previous producer owned the handle. Keep the sequence
				// monotonic, but do not announce the retired GPU texture again.
				atomic.StoreUint64(&p.ctrl.Publish, 0)
			}
		}
		for i := 0; i < SlotCount; i++ {
			atomic.CompareAndSwapUint32(&p.ctrl.Slots[i].Claim, claimWriter, claimFree)
		}
		atomic.AddUint64(&p.ctrl.Generation, 1)
		atomic.StoreUint64(&p.ctrl.Dropped, 0)
		atomic.StoreUint64(&p.ctrl.AdapterLUID, 0)
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

// LastPublishNanos reports how long the most recent commit took, in
// nanoseconds: filling in the slot metadata, storing the publish word and
// signalling the event. Zero before the first picture.
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

// ConsumerAllowsGPUShare requires an attached reader that understands GPU slots.
// Older readers of the same mapping format continue to receive CPU pixels.
func (p *Publisher) ConsumerAllowsGPUShare() bool {
	if p.ctrl == nil {
		return false
	}
	const required = ConsumerFlagAttached | ConsumerFlagGPUShare
	const mask = required | ConsumerFlagNoGPUShare
	return atomic.LoadUint32(&p.ctrl.ConsumerFlags)&mask == required
}

// ConsumerGate decides whether a decoded picture is worth downloading from the
// GPU, colour-converting and publishing. When the UI is minimised, occluded or
// its raster thread has stalled, nothing adopts the published pictures; doing
// that work anyway burns power and frame time for pixels nobody sees.
//
// The gate never suppresses decoding: P/B frames are references, so the caller
// still decodes every access unit. It only suppresses the download, the colour
// conversion and the publish.
//
// A gated producer still emits a probe picture at probeEvery, because adoption
// is what re-arms the gate and a publish word that never changes is never
// re-adopted. A probe only consumes its interval once it actually commits a
// picture (callers report the outcome through Published), so an access unit
// that yields no frame cannot postpone the next probe. A hidden reader pays for
// one probe per interval; a reader that was merely idle (static sender) gets a
// short resume burst so the first frames after the pause are not gated.
type ConsumerGate struct {
	staleAfter   int64 // nanoseconds since the last adoption before gating
	probeEvery   int64 // nanoseconds between probe pictures while gated
	resumeBurst  int64 // nanoseconds of full-rate frames after a gated probe commits
	nextProbe    int64 // nanoseconds; the earliest next probe
	graceUntil   int64 // nanoseconds; allow everything until then (resume burst)
	probePending bool  // a probe was allowed and its outcome is not reported yet
	clock        func() int64
}

// Default consumer-gating timings. A second of silence means the reader is not
// rasterising: normal display adopts every rendered frame (about 16 ms at 60Hz).
// The same second bounds how long a restored window can show a frozen picture
// before the probe publishes a new one.
const (
	ConsumerStaleAfter  = time.Second
	ConsumerProbeEvery  = time.Second
	ConsumerResumeBurst = 500 * time.Millisecond
)

// NewConsumerGate builds a gate with the given timings.
func NewConsumerGate(staleAfter, probeEvery time.Duration) *ConsumerGate {
	return &ConsumerGate{
		staleAfter:  int64(staleAfter),
		probeEvery:  int64(probeEvery),
		resumeBurst: int64(ConsumerResumeBurst),
		clock:       clockNanos,
	}
}

// DefaultConsumerGate is the gate the frame sink uses.
func DefaultConsumerGate() *ConsumerGate {
	return NewConsumerGate(ConsumerStaleAfter, ConsumerProbeEvery)
}

// clockNanos reports QPC as nanoseconds, or zero when the counter is unavailable.
func clockNanos() int64 {
	now := nowQPC()
	if now <= 0 {
		return 0
	}
	return int64(float64(now) * 1e9 / float64(qpcFrequency()))
}

// Allow reports whether this picture should be produced for the consumer
// described by c.
//
//   - Before any reader attaches there is nothing to display, so only probes
//     are produced (one immediately, so the first picture is ready for a UI that
//     starts afterwards).
//   - A reader that attached but has not adopted a picture yet is seeded with
//     full-rate frames: it cannot stamp freshness without something to adopt.
//   - A recent adoption means the reader is consuming; produce everything.
//   - An old adoption with nothing un-adopted means the screen is static, not
//     that the reader is gone; produce everything and open a resume burst so
//     the first frames after the pause are not gated while the reader wakes.
//   - Only when published pictures are waiting un-adopted for longer than
//     staleAfter is the reader considered stalled; then probes are produced.
//
// A clock failure fails open: without QPC there is no way to measure staleness,
// so the producer behaves as it always did.
func (g *ConsumerGate) Allow(c ConsumerStats) bool {
	if g == nil {
		return true
	}
	now := g.clock()
	if now <= 0 {
		return true
	}
	allow := false
	switch {
	case c.ConsumerPID == 0 && c.Adopted == 0:
		allow = g.requestProbe(now)
	case c.Adopted == 0:
		// Attached, but no picture adopted yet: publish normally to seed it.
		g.probePending = false
		g.nextProbe = now + g.probeEvery
		allow = true
	case c.ConsumerAgeNanos <= g.staleAfter:
		g.probePending = false
		g.nextProbe = now + g.probeEvery
		g.graceUntil = 0
		allow = true
	case c.Published <= c.Adopted:
		// Every published picture was adopted; the reader is idle, not stalled.
		g.probePending = false
		g.nextProbe = now + g.probeEvery
		g.graceUntil = now + g.resumeBurst
		allow = true
	case now < g.graceUntil:
		g.probePending = false
		allow = true
	default:
		allow = g.requestProbe(now)
	}
	return allow
}

// Published reports the outcome of an allowed picture. It must be called once
// for every Allow that returned true: committed says whether a picture was
// actually published. A probe only starts its interval on commit, so a probe
// access unit that produced no frame does not waste the next second.
func (g *ConsumerGate) Published(committed bool) {
	if g == nil {
		return
	}
	g.probePending = false
	if !committed {
		return
	}
	now := g.clock()
	if now <= 0 {
		return
	}
	g.nextProbe = now + g.probeEvery
	// No resume burst here: a probe committed while the reader is hidden would
	// otherwise run the full pipeline for the burst window (~30 frames) even
	// though nobody is watching. On restore, the reader adopts the probe frame
	// and its fresh timestamp re-arms full rate; the static-screen wake path
	// above opens the burst where it is actually needed.
}

// requestProbe reports whether a probe picture is due. A probe stays pending
// until Published reports its outcome.
func (g *ConsumerGate) requestProbe(now int64) bool {
	if g.probePending || now < g.nextProbe {
		return false
	}
	g.probePending = true
	return true
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

// Publish writes one picture by copying it into a claimed slot. pixels must be
// tightly packed rows of width*4 bytes; imgStride is the row stride inside
// pixels. Producers that can render straight into the mapping (the video
// decoder) use Acquire/Commit instead and skip this copy entirely.
//
// It never blocks: when no slot is free the frame is dropped and recorded in the
// control block.
func (p *Publisher) Publish(width, height, imgStride int, pixels []byte) error {
	rowBytes := width * 4
	if width <= 0 || height <= 0 {
		return errors.New("frames: empty image")
	}
	if imgStride < rowBytes {
		return fmt.Errorf("frames: stride %d shorter than a row (%d)", imgStride, rowBytes)
	}
	if need := (height-1)*imgStride + rowBytes; len(pixels) < need {
		return fmt.Errorf("frames: pixels are %d bytes, need %d", len(pixels), need)
	}

	c, err := p.Acquire(width, height)
	if err != nil {
		if errors.Is(err, ErrNoSlot) {
			return nil // dropped, not an error: this is the designed backpressure
		}
		return err
	}
	dst := c.Pixels()
	if imgStride == rowBytes {
		copy(dst, pixels)
	} else {
		for y := 0; y < height; y++ {
			copy(dst[y*rowBytes:(y+1)*rowBytes], pixels[y*imgStride:y*imgStride+rowBytes])
		}
	}
	c.Commit()
	return nil
}

// Claim is one slot reserved for a picture. The slot stays in the writer state,
// where a reader cannot adopt it, until Commit publishes it or Cancel gives it
// back. While a claim is outstanding, Publish keeps pointing at the previously
// published slot, so a half-written picture is never visible.
//
// A Claim is owned by the goroutine that acquired it and must be settled with
// Commit or Cancel.
type Claim struct {
	pub       *Publisher
	index     int
	width     int
	height    int
	rowStride int
	pixels    []byte
	done      bool
}

// Acquire reserves a free slot for a picture of width x height and returns it
// ready to be filled. Pixels reports the mapped buffer to render into and
// Stride its row stride; the slot keeps one row's worth of padding inside the
// mapped memory, so the reader's frame_byte_count contract is unchanged.
//
// It returns ErrNoSlot when every slot is held. The caller must settle the
// claim with Commit or Cancel before the next Acquire.
func (p *Publisher) Acquire(width, height int) (*Claim, error) {
	if p.ctrl == nil {
		return nil, errors.New("frames: publisher is closed")
	}
	if width <= 0 || height <= 0 {
		return nil, errors.New("frames: empty image")
	}
	rowStride := width * 4
	need := uint64(rowStride) * uint64(height)
	if need > uint64(p.stride) {
		return nil, fmt.Errorf("frames: frame of %dx%d needs %d bytes, slot holds %d", width, height, need, p.stride)
	}

	i := p.claimSlot()
	if i < 0 {
		// Every slot is busy. Record the drop so the difference between "the
		// sender stopped" and "we could not display it" stays visible.
		atomic.AddUint64(&p.ctrl.Dropped, 1)
		return nil, ErrNoSlot
	}
	dst := unsafe.Slice((*byte)(unsafe.Add(p.slots, uintptr(i)*uintptr(p.stride))), int(p.stride))
	return &Claim{
		pub:       p,
		index:     i,
		width:     width,
		height:    height,
		rowStride: rowStride,
		pixels:    dst[:need],
	}, nil
}

// Index reports the claimed slot index (0..SlotCount-1), or -1 if nil.
func (c *Claim) Index() int {
	if c == nil {
		return -1
	}
	return c.index
}

// Pixels is the mapped buffer to render the picture into: height rows of
// rowStride bytes. Only the first width*4 bytes of each row are visible to the
// reader.
func (c *Claim) Pixels() []byte { return c.pixels }

// Stride is the row stride, in bytes, to render each row at. It is always
// width*4, which is the contract the C++ reader expects.
func (c *Claim) Stride() int { return c.rowStride }

// Commit fills the slot metadata for a CPU RGBA picture, stores the publish
// word and wakes the reader. It must be called after the pixels are in place.
// It is a no-op once the claim has been settled.
//
// Ordering: the pixel and metadata writes are sequenced before the atomics that
// release the slot and store Publish. The slot is released first, so a reader
// that sees the new word can always claim it and a reader still holding the
// old word can never claim this slot early.
func (c *Claim) Commit() {
	c.commit(FormatRGBA, 0, 0)
}

// CommitGPU publishes a DXGI shared B8G8R8A8_UNORM texture for this slot
// without writing CPU pixels into shared memory. It must be called only after
// GPU rendering into the slot's shared texture has completed.
func (c *Claim) CommitGPU(sharedHandle uintptr, adapterLUID uint64) {
	c.commit(FormatDXGIBGRA, uint64(sharedHandle), adapterLUID)
}

func (c *Claim) commit(format uint32, sharedHandle uint64, adapterLUID uint64) {
	if c == nil || c.done {
		return
	}
	c.done = true
	p := c.pub
	start := nowQPC()

	atomic.StoreUint64(&p.ctrl.AdapterLUID, adapterLUID)
	atomic.StoreUint32(&p.ctrl.PixelFormat, format)

	slot := &p.ctrl.Slots[c.index]
	p.seq++
	slot.Seq++
	slot.FrameSeq = p.seq
	slot.Width = uint32(c.width)
	slot.Height = uint32(c.height)
	slot.Stride = uint32(c.rowStride)
	slot.Format = format
	slot.SharedHandle = sharedHandle
	slot.WriterPID = p.ctrl.ProducerPID
	slot.WriteQPC = uint64(nowQPC())

	// The publish word carries the slot index so a reader never has to guess
	// which slot the new metadata belongs to. Release the slot first: a reader
	// that sees the new word can always claim it, while a reader still holding
	// the old word can never claim this slot early. The metadata and pixel
	// writes are sequenced before both atomics.
	atomic.StoreUint32(&slot.Claim, claimFree)
	atomic.StoreUint64(&p.ctrl.Publish, p.seq<<2|uint64(c.index))
	atomic.AddUint64(&p.ctrl.FrameSeq, 1)
	p.current = c.index

	if p.event != 0 {
		_ = windows.SetEvent(p.event)
	}
	published := nowQPC()
	atomic.StoreUint64(&p.ctrl.LastPublishQPC, uint64(published))
	atomic.StoreUint64(&p.ctrl.HeartbeatQPC, uint64(published))
	if start > 0 && published >= start {
		p.lastPublishNanos.Store(qpcNanos(published - start))
	}
}

// Cancel gives the slot back without publishing it. Nothing a reader can see
// changes: the publish word still names the previous picture.
func (c *Claim) Cancel() {
	if c == nil || c.done {
		return
	}
	c.done = true
	atomic.StoreUint32(&c.pub.ctrl.Slots[c.index].Claim, claimFree)
}

// Picture is a detached copy of one published frame.
type Picture struct {
	Width, Height int
	Stride        int
	Pixels        []byte
	Seq           uint64
}

// LatestSlot reports the slot index and pixel format of the newest published
// picture without copying pixels. ok is false before the first picture.
// As with Latest, the caller must serialise this with writes to the publisher.
func (p *Publisher) LatestSlot() (slotIndex int, format uint32, ok bool) {
	if p.ctrl == nil {
		return 0, 0, false
	}
	publish := atomic.LoadUint64(&p.ctrl.Publish)
	if publish == 0 {
		return 0, 0, false
	}
	i := int(publish & 0x3)
	if i >= SlotCount {
		return 0, 0, false
	}
	return i, p.ctrl.Slots[i].Format, true
}

// Latest copies the newest published CPU RGBA picture out of its slot. ok is
// false before the first picture or when the newest picture is a GPU-resident
// shared texture (FormatDXGIBGRA). The caller must serialise this with Publish,
// Acquire, Commit and Cancel (the frame sink's mutex does that), so the copy
// cannot race a claim.
func (p *Publisher) Latest() (Picture, bool) {
	i, format, ok := p.LatestSlot()
	if !ok || format != FormatRGBA {
		return Picture{}, false
	}
	slot := &p.ctrl.Slots[i]
	width, height, stride := int(slot.Width), int(slot.Height), int(slot.Stride)
	if width <= 0 || height <= 0 || stride < width*4 {
		return Picture{}, false
	}
	need := stride * height
	if need > int(p.stride) {
		return Picture{}, false
	}
	src := unsafe.Slice((*byte)(unsafe.Add(p.slots, uintptr(i)*uintptr(p.stride))), need)
	out := make([]byte, need)
	copy(out, src)
	return Picture{Width: width, Height: height, Stride: stride, Pixels: out, Seq: slot.FrameSeq}, true
}

// claimSlot leaves the published picture intact while the next one is prepared.
func (p *Publisher) claimSlot() int {
	slots := SlotCount
	if atomic.LoadUint64(&p.ctrl.Publish) != 0 {
		slots--
	}
	for offset := 1; offset <= slots; offset++ {
		i := (p.current + offset) % SlotCount
		if atomic.CompareAndSwapUint32(&p.ctrl.Slots[i].Claim, claimFree, claimWriter) {
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
