//go:build windows

// Package framefeed reads decoded RGBA pictures published into a Windows named
// shared-memory section by internal/frames.
//
// The reader and internal/frames share the section layout and slot claim protocol.
package framefeed

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultMappingName is the default named shared-memory section used by the
// MirrPane receiver process.
const DefaultMappingName = `Local\YingpingFrames`

const (
	magic          uint64 = 0x314D4853524D4641 // "AFRMSHM1" little endian
	version        uint32 = 1
	headerBytes           = 0x1000
	slotCount             = 3
	slotDataOffset uint64 = 0x10000

	formatRGBA     uint32 = 1
	formatBGRA     uint32 = 2
	formatDXGIBGRA uint32 = 3

	consumerFlagAttached   uint32 = 1 << 0
	consumerFlagNoGPUShare uint32 = 1 << 1
	consumerFlagGPUShare   uint32 = 1 << 2

	claimFree   uint32 = 0
	claimWriter uint32 = 1
	claimReader uint32 = 2
)

// slotEntry mirrors internal/frames.slotEntry, exactly 64 bytes.
type slotEntry struct {
	Seq          uint64 // 0x00: monotonic per-slot sequence counter
	Claim        uint32 // 0x08: claim state (0=free, 1=writer, 2=reader)
	Flags        uint32 // 0x0C: slot flags
	FrameSeq     uint64 // 0x10: global frame sequence matching Publish >> 2
	Width        uint32 // 0x18: frame width in pixels
	Height       uint32 // 0x1C: frame height in pixels
	Stride       uint32 // 0x20: row stride in bytes
	Format       uint32 // 0x24: pixel format (1=RGBA, 2=BGRA, 3=DXGI BGRA)
	WriteQPC     uint64 // 0x28: producer QPC timestamp when written
	WriterPID    uint32 // 0x30: producer process ID
	Reserved0    uint32 // 0x34: reserved / alignment padding
	SharedHandle uint64 // 0x38: DXGI shared HANDLE when Format == formatDXGIBGRA
}

// control mirrors internal/frames.control, exactly 0x1000 bytes (4 KiB).
type control struct {
	Magic           uint64                    // 0x000: 0x314D4853524D4641 ("AFRMSHM1" LE)
	Version         uint32                    // 0x008: layout version (1)
	HeaderBytes     uint32                    // 0x00C: control block size (0x1000)
	SlotCount       uint32                    // 0x010: number of frame slots (3)
	SlotStride      uint32                    // 0x014: byte stride between slot buffers
	SlotDataOffset  uint64                    // 0x018: byte offset of slot 0 pixel data (0x10000)
	Width           uint32                    // 0x020: advertised display width
	Height          uint32                    // 0x024: advertised display height
	Stride          uint32                    // 0x028: advertised display stride
	PixelFormat     uint32                    // 0x02C: active pixel format
	Generation      uint64                    // 0x030: incremented on producer restart
	ProducerPID     uint32                    // 0x038: producer process ID
	Flags           uint32                    // 0x03C: control flags
	Publish         uint64                    // 0x040: packed publish word (seq<<2 | slotIndex)
	FrameSeq        uint64                    // 0x048: total published frame count
	LastPublishQPC  uint64                    // 0x050: QPC timestamp of last publish
	HeartbeatQPC    uint64                    // 0x058: QPC timestamp of producer heartbeat
	FramePTS        uint64                    // 0x060: frame presentation timestamp
	Dropped         uint64                    // 0x068: frames dropped by producer due to no free slot
	AdapterLUID     uint64                    // 0x070: producer DXGI adapter LUID
	Reserved1       uint64                    // 0x078: reserved
	ConsumerPID     uint32                    // 0x080: consumer process ID
	ConsumerFlags   uint32                    // 0x084: consumer capability flags
	ConsumerQPC     uint64                    // 0x088: QPC timestamp of last consumer adoption
	ConsumerSlot    uint32                    // 0x090: slot index last adopted by consumer
	Reserved2       uint32                    // 0x094: reserved / alignment padding
	ConsumerAdopted uint64                    // 0x098: atomic count of adopted pictures
	Reserved3       [12]uint64                // 0x0A0: reserved up to 0x100
	Slots           [slotCount]slotEntry      // 0x100: 3 * 64 = 192 bytes (0x100..0x1C0)
	Tail            [headerBytes - 0x1C0]byte // 0x1C0: padding to 0x1000 bytes
}

// Compile-time size and key offset assertions matching internal/frames.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(control{})-headerBytes]
	_ = [1]struct{}{}[unsafe.Sizeof(slotEntry{})-64]
	_ = [1]struct{}{}[unsafe.Offsetof(control{}.ConsumerPID)-0x080]
	_ = [1]struct{}{}[unsafe.Offsetof(control{}.ConsumerAdopted)-0x098]
	_ = [1]struct{}{}[unsafe.Offsetof(control{}.Slots)-0x100]
	_ = [1]struct{}{}[unsafe.Offsetof(slotEntry{}.SharedHandle)-0x38]
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procOpenFileMappingW        = kernel32.NewProc("OpenFileMappingW")
	procQueryPerformanceCounter = kernel32.NewProc("QueryPerformanceCounter")
)

func nowQPC() int64 {
	var counter int64
	if r, _, _ := procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&counter))); r == 0 {
		return 0
	}
	return counter
}

func releaseSlot(slot *slotEntry) {
	atomic.StoreUint32(&slot.Claim, claimFree)
}

// Frame represents one adopted RGBA picture backed directly by mapped slot
// memory. Pixels contains Height rows of Stride bytes, with Width*4 bytes used
// per row.
//
// While the slot is held, no producer writes it, so the pixels stay exactly as
// published. The reader holds the slot until it adopts a different picture (a
// Frame with another Seq) on a later Latest call, or until Close. A producer
// restart that only bumps the section's generation keeps the same slot, so the
// picture the UI already shows never flickers or turns invalid underneath it.
type Frame struct {
	Pixels []byte
	Width  int
	Height int
	Stride int
	Seq    uint64
}

// Reader maps an existing frames shared-memory section as a CPU RGBA consumer.
type Reader struct {
	name       string
	mapped     windows.Handle
	base       uintptr
	ctrl       *control
	slots      unsafe.Pointer
	slotStride uint32
	event      windows.Handle
	stopEvent  windows.Handle

	closed  atomic.Bool
	closeMu sync.RWMutex

	// stale is set when the producer replaced the section with an incompatible
	// layout, so the mapped slots no longer describe valid pictures. Latest
	// stops returning pictures then; the mapping stays usable, so a picture
	// already handed out can keep showing until Close.
	stale atomic.Bool

	mu             sync.Mutex
	heldSlot       int
	heldFrame      Frame
	lastPublish    uint64
	lastSkipped    uint64 // newest publish word that held no CPU RGBA picture
	lastGeneration uint64
}

// Open attaches to an existing named shared-memory section, validates its
// control block header, marks the consumer attached for CPU RGBA frames, and
// optionally opens the "<name>.frame" wake event.
func Open(name string) (*Reader, error) {
	if name == "" {
		return nil, errors.New("framefeed: empty mapping name")
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("framefeed: name: %w", err)
	}
	r1, _, callErr := procOpenFileMappingW.Call(
		uintptr(windows.FILE_MAP_READ|windows.FILE_MAP_WRITE),
		0,
		uintptr(unsafe.Pointer(namePtr)),
	)
	if r1 == 0 {
		if callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_FILE_NOT_FOUND
		}
		return nil, fmt.Errorf("framefeed: open section %q: %w", name, callErr)
	}
	mapped := windows.Handle(r1)

	base, err := windows.MapViewOfFile(mapped, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(mapped)
		return nil, fmt.Errorf("framefeed: map section %q: %w", name, err)
	}

	cleanupMapping := func() {
		_ = windows.UnmapViewOfFile(base)
		_ = windows.CloseHandle(mapped)
	}

	var mbi windows.MemoryBasicInformation
	if err := windows.VirtualQuery(base, &mbi, unsafe.Sizeof(mbi)); err != nil {
		cleanupMapping()
		return nil, fmt.Errorf("framefeed: query mapped view: %w", err)
	}
	if mbi.RegionSize < headerBytes {
		cleanupMapping()
		return nil, fmt.Errorf("framefeed: mapped view too small (%d bytes)", mbi.RegionSize)
	}

	// MapViewOfFile returns a uintptr; unsafe.Add from nil converts it without
	// triggering go vet's unsafeptr check (matching internal/frames).
	ctrl := (*control)(unsafe.Add(unsafe.Pointer(nil), base))

	gotMagic := atomic.LoadUint64(&ctrl.Magic)
	gotVersion := atomic.LoadUint32(&ctrl.Version)
	gotSlotCount := atomic.LoadUint32(&ctrl.SlotCount)
	gotOffset := atomic.LoadUint64(&ctrl.SlotDataOffset)
	gotStride := atomic.LoadUint32(&ctrl.SlotStride)
	if gotMagic != magic ||
		gotVersion != version ||
		gotSlotCount != slotCount ||
		gotOffset != slotDataOffset ||
		gotStride == 0 {
		cleanupMapping()
		return nil, fmt.Errorf(
			"framefeed: invalid header (magic=%#x version=%d slots=%d offset=%#x stride=%d)",
			gotMagic, gotVersion, gotSlotCount, gotOffset, gotStride,
		)
	}

	needBytes := gotOffset + uint64(gotSlotCount)*uint64(gotStride)
	if needBytes < gotOffset || uint64(mbi.RegionSize) < needBytes {
		cleanupMapping()
		return nil, fmt.Errorf("framefeed: mapped view (%d bytes) smaller than slots (%d bytes)", mbi.RegionSize, needBytes)
	}

	atomic.StoreUint32(&ctrl.ConsumerPID, uint32(windows.GetCurrentProcessId()))
	atomic.StoreUint32(&ctrl.ConsumerFlags, consumerFlagAttached|consumerFlagNoGPUShare)

	var ev windows.Handle
	if evPtr, err := windows.UTF16PtrFromString(name + ".frame"); err == nil {
		if h, err := windows.OpenEvent(windows.SYNCHRONIZE, false, evPtr); err == nil {
			ev = h
		}
	}

	stopEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		if ev != 0 {
			_ = windows.CloseHandle(ev)
		}
		cleanupMapping()
		return nil, fmt.Errorf("framefeed: create stop event: %w", err)
	}

	return &Reader{
		name:           name,
		mapped:         mapped,
		base:           base,
		ctrl:           ctrl,
		slots:          unsafe.Add(unsafe.Pointer(ctrl), slotDataOffset),
		slotStride:     gotStride,
		event:          ev,
		stopEvent:      stopEv,
		heldSlot:       -1,
		lastGeneration: atomic.LoadUint64(&ctrl.Generation),
	}, nil
}

// Latest returns the newest CPU RGBA frame using the shared-memory claim
// protocol. When a newer RGBA frame has been published, it claims the new slot,
// releases any previously held slot, updates consumer adoption bookkeeping, and
// returns a zero-copy Frame slice into the mapped slot memory.
//
// Non-RGBA slots (such as DXGI shared surfaces) are skipped without advancing
// adoption state. If no new RGBA frame is available, any previously held RGBA
// frame continues to be returned so redraws do not blank.
//
// The returned Pixels slice and its slot stay valid across later calls as long
// as Latest keeps returning the same Seq: the slot is only released when a
// different picture is adopted, when the producer restarts with an
// incompatible layout, or on Close. A producer restart that bumps the section's
// generation keeps the held slot, so the picture on screen is never invalidated
// or needlessly re-adopted (which would upload the same pixels again).
func (r *Reader) Latest() (Frame, bool) {
	if r == nil {
		return Frame{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ctrl == nil || r.closed.Load() || r.stale.Load() {
		return Frame{}, false
	}

	gen := atomic.LoadUint64(&r.ctrl.Generation)
	if gen != r.lastGeneration {
		if !r.layoutIntact() {
			// The producer re-created the section with another slot layout, so
			// the slot we hold may not describe these bytes any more. Stop
			// handing out pictures until the owner reopens the mapping.
			r.releaseHeldLocked()
			r.lastPublish = 0
			r.stale.Store(true)
			return Frame{}, false
		}
		// A sidecar restart reuses the section and only bumps Generation. Our
		// slot stays claimed, so the picture last returned is still intact and
		// still on screen. Keep holding it and keep lastPublish: the next new
		// picture has a different sequence and is adopted as usual.
		r.lastGeneration = gen
		r.lastSkipped = 0
		atomic.StoreUint32(&r.ctrl.ConsumerPID, uint32(windows.GetCurrentProcessId()))
		atomic.StoreUint32(&r.ctrl.ConsumerFlags, consumerFlagAttached|consumerFlagNoGPUShare)
	}

	publish := atomic.LoadUint64(&r.ctrl.Publish)
	if publish != 0 && publish != r.lastPublish && publish != r.lastSkipped {
		if idx, slot, ok := r.claimLatest(publish); ok {
			if r.isValidRGBA(slot) {
				r.adoptFrameLocked(publish, idx, slot)
			} else {
				releaseSlot(slot)
				// Remember the skip: re-claiming the same GPU surface on every
				// call would only contend with the producer for its slots. A
				// newer picture always brings a new publish word.
				r.lastSkipped = publish
			}
		}
	}

	if r.heldSlot < 0 {
		return Frame{}, false
	}
	return r.heldFrame, true
}

// Stale reports whether the producer replaced the section with a layout this
// reader cannot interpret. Latest then returns no pictures; the mapping stays
// alive, so a picture handed out before that keeps showing until Close.
func (r *Reader) Stale() bool {
	return r != nil && r.stale.Load()
}

// layoutIntact reports whether the control block still describes the layout
// this reader mapped.
func (r *Reader) layoutIntact() bool {
	return atomic.LoadUint64(&r.ctrl.Magic) == magic &&
		atomic.LoadUint32(&r.ctrl.Version) == version &&
		atomic.LoadUint32(&r.ctrl.SlotCount) == slotCount &&
		atomic.LoadUint64(&r.ctrl.SlotDataOffset) == slotDataOffset &&
		atomic.LoadUint32(&r.ctrl.SlotStride) == r.slotStride
}

func (r *Reader) claimLatest(publish uint64) (int, *slotEntry, bool) {
	index := int(publish & 0x3)
	if publish == 0 || index >= slotCount {
		return 0, nil, false
	}
	slot := &r.ctrl.Slots[index]
	if !atomic.CompareAndSwapUint32(&slot.Claim, claimFree, claimReader) {
		return 0, nil, false
	}
	if atomic.LoadUint64(&slot.FrameSeq) != (publish >> 2) {
		releaseSlot(slot)
		return 0, nil, false
	}
	return index, slot, true
}

func (r *Reader) isValidRGBA(slot *slotEntry) bool {
	if atomic.LoadUint32(&slot.Format) != formatRGBA {
		return false
	}
	w := int(atomic.LoadUint32(&slot.Width))
	h := int(atomic.LoadUint32(&slot.Height))
	stride := int(atomic.LoadUint32(&slot.Stride))
	if w <= 0 || h <= 0 || stride < w*4 {
		return false
	}
	need := uint64(stride) * uint64(h)
	return need <= uint64(r.slotStride)
}

func (r *Reader) adoptFrameLocked(publish uint64, index int, slot *slotEntry) {
	// The new slot is already reader-claimed, so releasing the previous slot now
	// bounds reader-held slots to at most two (transiently during this handoff).
	r.releaseHeldLocked()

	w := int(atomic.LoadUint32(&slot.Width))
	h := int(atomic.LoadUint32(&slot.Height))
	stride := int(atomic.LoadUint32(&slot.Stride))
	seq := atomic.LoadUint64(&slot.FrameSeq)
	need := stride * h
	ptr := unsafe.Add(r.slots, uintptr(index)*uintptr(r.slotStride))
	pixels := unsafe.Slice((*byte)(ptr), need)

	r.heldSlot = index
	r.heldFrame = Frame{
		Pixels: pixels,
		Width:  w,
		Height: h,
		Stride: stride,
		Seq:    seq,
	}
	r.lastPublish = publish

	atomic.StoreUint32(&r.ctrl.ConsumerSlot, uint32(index))
	atomic.StoreUint64(&r.ctrl.ConsumerQPC, uint64(nowQPC()))
	atomic.AddUint64(&r.ctrl.ConsumerAdopted, 1)
}

func (r *Reader) releaseHeldLocked() {
	if r.heldSlot >= 0 && r.ctrl != nil {
		releaseSlot(&r.ctrl.Slots[r.heldSlot])
	}
	r.heldSlot = -1
	r.heldFrame = Frame{}
}

// Wait blocks until the producer signals the wake event, timeout elapses, or
// Close is called. It returns true when the wake event fired, or false on
// timeout, fallback sleep, or close.
func (r *Reader) Wait(timeout time.Duration) bool {
	if r == nil {
		return false
	}
	r.closeMu.RLock()
	defer r.closeMu.RUnlock()

	if r.closed.Load() {
		return false
	}

	ms := durationToWaitMs(timeout)
	if r.event != 0 {
		ev, err := windows.WaitForMultipleObjects([]windows.Handle{r.stopEvent, r.event}, false, ms)
		return err == nil && ev == windows.WAIT_OBJECT_0+1
	}

	if timeout < 0 || ms > 8 {
		ms = 8
	}
	_, _ = windows.WaitForSingleObject(r.stopEvent, ms)
	return false
}

func durationToWaitMs(d time.Duration) uint32 {
	if d < 0 {
		return windows.INFINITE
	}
	if d == 0 {
		return 0
	}
	ms := d / time.Millisecond
	if ms == 0 {
		return 1
	}
	if ms >= windows.INFINITE {
		return windows.INFINITE - 1
	}
	return uint32(ms)
}

// Close releases any held slot, unmaps the shared-memory view, and closes all
// kernel handles. It is safe to call while the producer continues publishing
// and is idempotent.
func (r *Reader) Close() error {
	if r == nil {
		return nil
	}
	if r.closed.Swap(true) {
		return nil
	}
	if r.stopEvent != 0 {
		_ = windows.SetEvent(r.stopEvent)
	}

	r.closeMu.Lock()
	defer r.closeMu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.releaseHeldLocked()
	r.lastPublish = 0

	var firstErr error
	if r.event != 0 {
		if err := windows.CloseHandle(r.event); err != nil && firstErr == nil {
			firstErr = err
		}
		r.event = 0
	}
	if r.stopEvent != 0 {
		if err := windows.CloseHandle(r.stopEvent); err != nil && firstErr == nil {
			firstErr = err
		}
		r.stopEvent = 0
	}
	if r.base != 0 {
		if err := windows.UnmapViewOfFile(r.base); err != nil && firstErr == nil {
			firstErr = err
		}
		r.base = 0
		r.ctrl = nil
		r.slots = nil
	}
	if r.mapped != 0 {
		if err := windows.CloseHandle(r.mapped); err != nil && firstErr == nil {
			firstErr = err
		}
		r.mapped = 0
	}
	return firstErr
}
