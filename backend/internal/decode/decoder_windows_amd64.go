// Package decode turns AirPlay H.264 and AAC access units into pictures and PCM.
// FFmpeg's shared libraries are called through the Windows ABI without cgo.
package decode

import (
	"errors"
	"fmt"
	"image"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var ErrNoFrame = errors.New("decode: no frame ready")

// errShortDestination means the caller's buffer cannot hold the picture. It is
// a contract error, not a decode failure, so it must not trip hardware recovery.
var errShortDestination = errors.New("decode: destination too small")

// QueryPerformanceCounter bindings. The decoder reports its per-stage cost in
// nanoseconds; QPC is the same clock the publisher stamps into the shared
// control block, so a timeline can be built across both sides.
var (
	qpcDLL      = syscall.NewLazyDLL("kernel32.dll")
	qpcProc     = qpcDLL.NewProc("QueryPerformanceCounter")
	qpcFreqProc = qpcDLL.NewProc("QueryPerformanceFrequency")

	qpcFreqOnce sync.Once
	qpcTicks    int64
)

// qpcNow reads the performance counter, or returns 0 when the call fails.
func qpcNow() int64 {
	var counter int64
	if r, _, _ := qpcProc.Call(uintptr(unsafe.Pointer(&counter))); r == 0 {
		return 0
	}
	return counter
}

// qpcFrequency reports ticks per second, cached after the first call.
func qpcFrequency() int64 {
	qpcFreqOnce.Do(func() {
		var freq int64
		if r, _, _ := qpcFreqProc.Call(uintptr(unsafe.Pointer(&freq))); r == 0 || freq <= 0 {
			freq = 1
		}
		qpcTicks = freq
	})
	return qpcTicks
}

// qpcElapsedNanos converts the interval from start to now into nanoseconds.
// A zero start means the counter was unavailable and yields no sample.
func qpcElapsedNanos(start int64) int64 {
	if start == 0 {
		return 0
	}
	now := qpcNow()
	if now <= start {
		return 0
	}
	return int64(float64(now-start) * 1e9 / float64(qpcFrequency()))
}

// DecodeTimings is where one picture's time went, in nanoseconds. Both fields
// are measured with QueryPerformanceCounter.
type DecodeTimings struct {
	// TransferNanos is av_hwframe_transfer_data, the GPU→system-memory
	// download. It is zero in software mode.
	TransferNanos int64
	// ConvertNanos includes swscale setup/conversion for RGBA, or the video
	// processor conversion and its GPU completion wait for a shared texture.
	ConvertNanos int64
}

// Image is one decoded picture. Pixels is reused by the next Decode call.
type Image struct {
	Width, Height int
	Stride        int
	Pixels        []byte
	Timings       DecodeTimings
}

func (i *Image) Bounds() image.Rectangle { return image.Rect(0, 0, i.Width, i.Height) }

type Options struct {
	PreferHardware bool
	ColorRange     string // auto | full | limited
	ScaleFilter    string // fast (point, 1:1) | bilinear | bicubic
}

// Decoder owns FFmpeg-allocated native objects. Calls must be serialized with
// Close; frameSink provides this for the receiver and snapshot endpoint.
//
// DecodeAccessUnit may rebuild the native state in place when D3D11VA keeps
// failing mid-stream (driver reset/TDR, dGPU power transition, session switch).
// The rebuild runs synchronously inside that call, so it is covered by the same
// serialization as every other use and never frees state under another call.
type Decoder struct {
	ff                     *ffmpegAPI
	ctx                    *codecContext
	frame, software        *avFrame
	packet                 *avPacket
	parser, scaler, device uintptr
	hardwareFormat         int32
	options                Options
	img                    Image
	ready                  bool
	decoded                bool // a picture has been produced by the current codec context
	frameReady             bool // d.frame holds a picture waiting for CopyImageTo
	pending                []byte

	health       hwFailureTracker
	reason       string // why the decoder is in software mode; "" while D3D11VA is configured
	params       []byte // last SPS/PPS seen, replayed into a rebuilt decoder
	replayParams bool

	gpu           *d3d11Pool
	gpuRetryAt    time.Time
	gpuRetryDelay time.Duration
}

// Hardware failure policy. A run of consecutive failed access units while
// D3D11VA is configured triggers recovery: either hwFailureFrames failures, or
// at least hwFailureMinFrames spanning hwFailureWindow (a static screen sends
// few frames, so a frame count alone could keep the receiver dark for long).
const (
	hwFailureFrames    = 30
	hwFailureMinFrames = 5
	hwFailureWindow    = time.Second
	hwDeviceRetries    = 1 // device recreations before permanent software mode
)

type hwState int

const (
	hwHealthy  hwState = iota // hardware configured, last access unit decoded
	hwDegraded                // hardware configured, consecutive failures below threshold
	hwSoftware                // permanently software for this decoder
)

func (s hwState) String() string {
	switch s {
	case hwHealthy:
		return "healthy"
	case hwDegraded:
		return "degraded"
	case hwSoftware:
		return "software"
	}
	return fmt.Sprintf("hwState(%d)", int(s))
}

type hwAction int

const (
	hwActionNone           hwAction = iota
	hwActionRecreateDevice          // rebuild with a fresh D3D11VA device
	hwActionUseSoftware             // rebuild in software and stay there
)

// hwFailureTracker is the pure decision logic behind hardware recovery. It
// holds no FFmpeg state so it can be tested without a GPU.
type hwFailureTracker struct {
	maxFrames, minFrames int
	window               time.Duration
	retries              int

	state        hwState
	consecutive  int
	firstFailure time.Time
}

func newHWFailureTracker() hwFailureTracker {
	return hwFailureTracker{
		maxFrames: hwFailureFrames,
		minFrames: hwFailureMinFrames,
		window:    hwFailureWindow,
		retries:   hwDeviceRetries,
	}
}

// success records a decoded picture and clears any failure run.
func (t *hwFailureTracker) success() {
	if t.state == hwSoftware {
		return
	}
	t.state = hwHealthy
	t.consecutive = 0
}

// failure records one failed access unit at now. When the run crosses the
// threshold it returns the recovery action and the run length that caused it,
// and starts counting afresh.
func (t *hwFailureTracker) failure(now time.Time) (hwAction, int) {
	if t.state == hwSoftware {
		return hwActionNone, 0
	}
	if t.consecutive == 0 {
		t.firstFailure = now
	}
	t.consecutive++
	t.state = hwDegraded
	run := t.consecutive
	if run < t.maxFrames && (run < t.minFrames || now.Sub(t.firstFailure) < t.window) {
		return hwActionNone, run
	}
	t.consecutive = 0
	if t.retries > 0 {
		t.retries--
		t.state = hwHealthy
		return hwActionRecreateDevice, run
	}
	t.state = hwSoftware
	return hwActionUseSoftware, run
}

// forceSoftware pins the tracker to software, e.g. when no device could be
// recreated.
func (t *hwFailureTracker) forceSoftware() {
	t.state = hwSoftware
	t.consecutive = 0
}

func NewDecoder() (*Decoder, error) {
	return NewDecoderWithOptions(Options{PreferHardware: true, ColorRange: "auto", ScaleFilter: "fast"})
}

func NewDecoderWithOptions(opts Options) (*Decoder, error) {
	f, err := loadFFmpeg()
	if err != nil {
		return nil, err
	}
	d := &Decoder{ff: f, options: opts, hardwareFormat: pixelNone, health: newHWFailureTracker()}
	if !opts.PreferHardware {
		d.reason = "hardware decoding not requested"
		d.health.forceSoftware()
	}
	if err := d.openBest(opts.PreferHardware); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// openBest opens the decoder, preferring D3D11VA when hardware is true and
// falling back to software (loudly) when the hardware codec cannot be opened.
// On error all native state has been released.
func (d *Decoder) openBest(hardware bool) error {
	if hardware {
		err := d.open(true)
		if err == nil {
			if d.hardwareFormat == pixelNone {
				d.health.forceSoftware()
			}
			return nil
		}
		d.release()
		d.reason = "opening the D3D11VA decoder failed: " + err.Error()
		log.Printf("decode: hardware decoding requested but %s; using software decoding", d.reason)
		d.health.forceSoftware()
	}
	if err := d.open(false); err != nil {
		d.release()
		return err
	}
	return nil
}

// open allocates the codec context, optional D3D11VA device, frame, packet and
// parser. On error the caller must release.
func (d *Decoder) open(hardware bool) error {
	f := d.ff
	d.hardwareFormat = pixelNone
	codec := f.findDecoder(codecH264)
	if codec == 0 {
		return errors.New("decode: H.264 codec unavailable")
	}
	d.ctx = f.allocContext(codec)
	if d.ctx == nil {
		return errors.New("decode: allocate H.264 context")
	}
	d.ctx.ThreadCount = 0
	d.ctx.ThreadType = threadSlice
	d.ctx.Flags |= flagLowDelay
	d.ctx.Flags2 |= flagFast
	if hardware {
		if reason := d.attachHardware(codec); reason != "" {
			d.reason = reason
			log.Printf("decode: hardware decoding requested but %s; using software decoding", reason)
			if d.device != 0 {
				f.bufferUnref(&d.device)
			}
		} else {
			d.reason = ""
		}
	}
	if ret := f.openCodec(d.ctx, codec, 0); ret < 0 {
		return f.error("open H.264", ret)
	}
	d.frame, d.packet, d.parser = f.frameAlloc(), f.packetAlloc(), f.parserInit(codecH264)
	if d.frame == nil || d.packet == nil || d.parser == 0 {
		return errors.New("decode: allocate H.264 buffers")
	}
	return nil
}

// attachHardware wires a D3D11VA device into d.ctx. It returns "" on success
// or a human-readable reason why hardware decoding is unavailable.
func (d *Decoder) attachHardware(codec uintptr) string {
	f := d.ff
	for i := int32(0); ; i++ {
		config := f.getHWConfig(codec, i)
		if config == nil {
			return "the FFmpeg H.264 decoder offers no D3D11VA configuration"
		}
		if config.Methods&hwDeviceContext == 0 || config.DeviceType != deviceD3D11VA {
			continue
		}
		if ret := f.hwDeviceCreate(&d.device, deviceD3D11VA, 0, 0, 0); ret < 0 {
			return "no D3D11VA device could be created (" + f.error("create D3D11VA device", ret).Error() + ")"
		}
		d.ctx.HWDeviceContext = f.bufferRef(d.device)
		if d.ctx.HWDeviceContext == 0 {
			return "the D3D11VA device reference could not be taken"
		}
		d.hardwareFormat = config.PixelFormat
		d.ctx.Opaque = uintptr(config.PixelFormat + 1)
		d.ctx.GetFormat = f.formatCallback
		return ""
	}
}

// rebuild replaces the FFmpeg state, keeping the last picture and the cached
// parameter sets, which are replayed into the next access unit. It runs inside
// a serialized DecodeAccessUnit call, so nothing else can be using the state.
func (d *Decoder) rebuild(hardware bool) {
	d.release()
	if err := d.openBest(hardware); err != nil {
		log.Printf("decode: rebuilding the H.264 decoder failed: %v; video decoding is stopped", err)
		return
	}
	d.replayParams = len(d.params) > 0
}

// recordHardwareFailure feeds one failed access unit into the tracker and
// performs the recovery it asks for.
func (d *Decoder) recordHardwareFailure(cause error) {
	action, run := d.health.failure(time.Now())
	switch action {
	case hwActionRecreateDevice:
		log.Printf("decode: %d consecutive D3D11VA failures (last: %v); recreating the hardware decoder", run, cause)
		d.rebuild(true)
		if d.ctx != nil && d.hardwareFormat != pixelNone {
			log.Print("decode: D3D11VA decoder recreated; waiting for the next keyframe")
		} else if d.ctx != nil {
			log.Print("decode: D3D11VA unavailable after recreation; switched to software decoding for the rest of this session")
		}
	case hwActionUseSoftware:
		d.reason = fmt.Sprintf("D3D11VA failed %d consecutive frames after device recreation (last: %v)", run, cause)
		log.Printf("decode: %s; switching to software decoding for the rest of this session", d.reason)
		d.rebuild(false)
		d.health.forceSoftware()
	}
}

// Hardware reports the current decode path. Before the current codec context
// produces a picture, it reports the configured mode; afterwards it also
// accounts for FFmpeg choosing a software format for this stream.
func (d *Decoder) Hardware() bool {
	return d.ctx != nil && d.frame != nil && d.hardwareFormat != pixelNone && (!d.decoded || d.frame.Format == d.hardwareFormat)
}

// Status describes the current decode path for logs and diagnostics.
func (d *Decoder) Status() string {
	switch {
	case d.ctx == nil:
		return "closed"
	case d.hardwareFormat == pixelNone:
		return "software (" + d.reason + ")"
	case d.decoded && !d.Hardware():
		// FFmpeg chose a software format for this stream; prefer that over a
		// transient degraded counter, which only describes hardware failures.
		reason := d.reason
		if reason == "" {
			reason = "FFmpeg declined D3D11VA for this stream"
		}
		return "software (" + reason + ")"
	case d.health.state == hwDegraded:
		return fmt.Sprintf("D3D11VA (degraded: %d consecutive failures)", d.health.consecutive)
	}
	return "D3D11VA"
}

func (d *Decoder) LastImage() *Image {
	if !d.ready {
		return nil
	}
	return &d.img
}

func (d *Decoder) Close() {
	d.release()
	if d.gpu != nil {
		d.gpu.close()
		d.gpu = nil
	}
	d.pending = nil
	d.params = nil
	d.img = Image{}
	d.ready = false
	d.frameReady = false
}

// ResetStream starts a fresh codec and hardware recovery budget for the next
// sender. The GPU slot pool outlives a session while a reader holds a texture.
func (d *Decoder) ResetStream() error {
	d.release()
	d.params = nil
	d.pending = nil
	d.img = Image{}
	d.ready = false
	d.health = newHWFailureTracker()
	d.reason = ""
	if !d.options.PreferHardware {
		d.health.forceSoftware()
		d.reason = "hardware decoding not requested"
	}
	return d.openBest(d.options.PreferHardware)
}

// release frees every FFmpeg object. The codec context (and its worker
// threads) is always freed before the hardware device reference.
func (d *Decoder) release() {
	d.hardwareFormat = pixelNone
	d.decoded = false
	d.frameReady = false
	d.replayParams = false
	d.gpuRetryAt = time.Time{}
	d.gpuRetryDelay = 0
	// A previously published texture can remain current while the FFmpeg
	// decoder recovers. Keep its pool alive for import and on-demand snapshots.
	if d.scaler != 0 {
		d.ff.scaleFree(d.scaler)
		d.scaler = 0
	}
	if d.software != nil {
		d.ff.frameFree(&d.software)
	}
	if d.frame != nil {
		d.ff.frameFree(&d.frame)
	}
	if d.packet != nil {
		d.ff.packetFree(&d.packet)
	}
	if d.parser != 0 {
		d.ff.parserClose(d.parser)
		d.parser = 0
	}
	// Free the codec (and its worker threads) before the device reference.
	if d.ctx != nil {
		d.ff.freeContext(&d.ctx)
	}
	if d.device != 0 {
		d.ff.bufferUnref(&d.device)
	}
}

// Decode parses an Annex-B byte stream. The receiver uses DecodeAccessUnit to
// avoid the parser's one-frame buffering, including when the screen is static.
func (d *Decoder) Decode(data []byte) (*Image, error) {
	if d.ctx == nil {
		return nil, errors.New("decode: decoder is closed")
	}
	if len(data) == 0 {
		return nil, ErrNoFrame
	}
	if len(d.pending)+len(data) > 16<<20 {
		d.pending = nil
		return nil, errors.New("decode: bitstream exceeds 16 MiB")
	}
	d.pending = append(d.pending, data...)
	// The parser may read AV_INPUT_BUFFER_PADDING_SIZE bytes past input.
	input := make([]byte, len(d.pending)+inputPadding)
	copy(input, d.pending)
	consumed := 0
	for consumed < len(d.pending) {
		var parsed *byte
		var size int32
		n := d.ff.parserParse(d.parser, d.ctx, &parsed, &size, &input[consumed], int32(len(d.pending)-consumed), noPTS, noPTS, 0)
		if n < 0 {
			return nil, d.ff.error("parse H.264", n)
		}
		consumed += int(n)
		if size > 0 {
			// Copy parser output into a padded, reference-counted native packet.
			img, err := d.DecodeAccessUnit(unsafe.Slice(parsed, int(size)))
			runtime.KeepAlive(input)
			if !errors.Is(err, ErrNoFrame) {
				d.pending = append(d.pending[:0], d.pending[consumed:]...)
				return img, err
			}
		}
		if n == 0 {
			break
		}
	}
	d.pending = append(d.pending[:0], d.pending[consumed:]...)
	runtime.KeepAlive(input)
	return nil, ErrNoFrame
}

// DecodeAccessUnit decodes one access unit and converts it into the decoder's
// own reused buffer. Callers that can render straight into their destination
// (the frame sink renders into the shared-memory slot) use ReceiveAccessUnit
// followed by CopyImageTo instead and skip that copy entirely.
func (d *Decoder) DecodeAccessUnit(data []byte) (*Image, error) {
	if err := d.ReceiveAccessUnit(data, true); err != nil {
		return nil, err
	}
	w, h := d.FrameSize()
	if w <= 0 || h <= 0 {
		// Surface the conversion path's own error (invalid size, or a failed
		// hardware download before it) so hardware recovery still sees it.
		_, err := d.CopyImageTo(nil, 0)
		return nil, err
	}
	stride := w * 4
	need := stride * h
	if cap(d.img.Pixels) < need {
		d.img.Pixels = make([]byte, need)
	}
	d.img = Image{Width: w, Height: h, Stride: stride, Pixels: d.img.Pixels[:need]}
	timings, err := d.CopyImageTo(d.img.Pixels, stride)
	if err != nil {
		return nil, err
	}
	d.img.Timings = timings
	d.ready = true
	return &d.img, nil
}

// ReceiveAccessUnit decodes one access unit and leaves the picture in the
// decoder for CopyImageTo. It never touches the GPU→system-memory download or
// the colour converter: that work is deferred so a producer with no consumer
// can skip it without skipping the decode itself (P/B frames are references,
// so every access unit must still be fed to FFmpeg).
//
// convert tells the decoder whether the caller will follow with CopyImageTo.
// When it is false the access unit is accounted for as a healthy decode at
// once, so a deliberately skipped conversion is not mistaken for a hardware
// failure by the recovery tracker.
func (d *Decoder) ReceiveAccessUnit(data []byte, convert bool) error {
	if d.ctx == nil {
		return errors.New("decode: decoder is closed")
	}
	if len(data) == 0 {
		return ErrNoFrame
	}
	// AirPlay sends SPS/PPS once, ahead of the first frame. Keep a copy so a
	// rebuilt decoder can be primed without waiting for the sender.
	if ps := annexBParameterSets(data); len(ps) > 0 {
		d.params = mergeParameterSets(d.params, ps)
	}
	hardware := d.hardwareFormat != pixelNone
	err := d.receiveAccessUnit(data)
	if hardware {
		switch {
		case err == nil:
			if !convert {
				d.health.success()
			}
			if d.frame.Format != d.hardwareFormat && d.reason == "" {
				d.reason = "FFmpeg declined D3D11VA for this stream"
				log.Printf("decode: hardware decoding requested but %s; using software decoding", d.reason)
			}
		case !errors.Is(err, ErrNoFrame):
			// May rebuild the native state; data and the frame are not used after.
			d.recordHardwareFailure(err)
		}
	}
	return err
}

// FrameSize reports the size of the picture ReceiveAccessUnit left waiting for
// CopyImageTo, or zero when there is none.
func (d *Decoder) FrameSize() (width, height int) {
	if d.frame == nil || !d.frameReady {
		return 0, 0
	}
	return int(d.frame.Width), int(d.frame.Height)
}

// CanRenderGPU reports whether the received picture currently waiting in the
// decoder is a D3D11VA hardware surface eligible for zero-copy GPU conversion.
func (d *Decoder) CanRenderGPU() bool {
	return d.ctx != nil &&
		d.frameReady &&
		d.frame != nil &&
		d.hardwareFormat != pixelNone &&
		d.frame.Format == d.hardwareFormat &&
		d.options.ScaleFilter != "bilinear" &&
		d.options.ScaleFilter != "bicubic" &&
		(d.gpuRetryAt.IsZero() || !time.Now().Before(d.gpuRetryAt))
}

// RenderFrameGPU converts the received D3D11VA NV12 picture on the GPU via
// ID3D11VideoProcessor into slotIndex's DXGI shared B8G8R8A8_UNORM texture and
// waits for GPU completion. On failure d.frameReady stays true so the caller
// can fall back to CopyImageTo without losing the decoded picture.
func (d *Decoder) RenderFrameGPU(slotIndex int) (GPUSurface, error) {
	if d.ctx == nil {
		return GPUSurface{}, errors.New("decode: decoder is closed")
	}
	if !d.frameReady || d.frame == nil {
		return GPUSurface{}, errors.New("decode: no received picture")
	}
	if slotIndex < 0 || slotIndex >= gpuSlotCount {
		return GPUSurface{}, fmt.Errorf("decode: invalid GPU slot index %d", slotIndex)
	}
	if !d.CanRenderGPU() {
		return GPUSurface{}, errors.New("decode: GPU zero-copy unavailable for current picture")
	}
	if d.gpu == nil {
		d.gpu = newD3D11Pool()
	}
	matrix, full := d.colorConversion()
	surf, err := d.gpu.renderFrame(d3d11Picture{
		texture:    (*comObject)(unsafe.Pointer(d.frame.Data[0])),
		arraySlice: *(*uintptr)(unsafe.Pointer(&d.frame.Data[1])), // FFmpeg stores an integer, not a pointer.
		width:      d.frame.Width,
		height:     d.frame.Height,
		matrix:     matrix,
		fullRange:  full,
	}, slotIndex)
	if err != nil {
		// A late completion query or device reset can recover. Keep publishing
		// RGBA in the meantime and bound the cost on unsupported hardware.
		d.gpuRetryDelay = min(time.Minute, max(time.Second, 2*d.gpuRetryDelay))
		d.gpuRetryAt = time.Now().Add(d.gpuRetryDelay)
		return GPUSurface{}, err
	}
	d.gpuRetryAt = time.Time{}
	d.gpuRetryDelay = 0
	d.frameReady = false
	d.health.success()
	return surf, nil
}

// ReadbackSlotGPU reads back the specified GPU shared slot texture (or the most
// recently rendered GPU slot when slotIndex < 0) into a CPU RGBA Image for
// on-demand screenshots (/api/snapshot).
func (d *Decoder) ReadbackSlotGPU(slotIndex int) (Image, error) {
	if d.gpu == nil {
		return Image{}, errors.New("decode: no active GPU surface pool")
	}
	return d.gpu.readbackSlotRGBA(slotIndex)
}

// ReleaseGPUSlot drops the producer's reference once a CPU picture replaces
// this mailbox slot. Other slots can still belong to readers on the old device.
func (d *Decoder) ReleaseGPUSlot(index int) {
	if d.gpu == nil {
		return
	}
	d.gpu.slots[index].tex.release()
	d.gpu.slots[index] = d3d11SlotSurface{}
	if d.gpu.lastSlot == index {
		d.gpu.lastSlot = -1
	}
	for _, slot := range d.gpu.slots {
		if slot.tex != nil {
			return
		}
	}
	d.gpu.close()
	d.gpu = nil
}

func (d *Decoder) receiveAccessUnit(data []byte) error {
	d.frameReady = false
	if d.replayParams {
		// Prepend the complete cache even if this packet supplies only SPS or
		// PPS. Repeating a parameter set is valid H.264 and keeps both present.
		primed := make([]byte, 0, len(d.params)+len(data))
		data = append(append(primed, d.params...), data...)
	}
	if err := d.ff.packet(d.packet, data); err != nil {
		return err
	}
	ret := d.ff.sendPacket(d.ctx, d.packet)
	d.ff.packetUnref(d.packet)
	if ret < 0 {
		return d.ff.error("send H.264 packet", ret)
	}
	d.replayParams = false
	ret = d.ff.receiveFrame(d.ctx, d.frame)
	if ret == errorAgain || ret == errorEOF {
		return ErrNoFrame
	}
	if ret < 0 {
		return d.ff.error("receive H.264 frame", ret)
	}
	d.frameReady = true
	d.decoded = true
	return nil
}

// CopyImageTo downloads the received picture from the GPU when needed and runs
// sws_scale into dst, which must hold height rows of dstStride bytes with at
// least width*4 bytes used per row. It reports where the time went. The
// picture is consumed: the next conversion needs another ReceiveAccessUnit.
func (d *Decoder) CopyImageTo(dst []byte, dstStride int) (DecodeTimings, error) {
	if d.ctx == nil {
		return DecodeTimings{}, errors.New("decode: decoder is closed")
	}
	if !d.frameReady || d.frame == nil {
		return DecodeTimings{}, errors.New("decode: no received picture")
	}
	hardware := d.hardwareFormat != pixelNone
	timings, err := d.copyImageTo(dst, dstStride)
	d.frameReady = false
	if err != nil {
		// A short destination is the caller's mistake, not a hardware fault.
		if hardware && !errors.Is(err, errShortDestination) {
			d.recordHardwareFailure(err)
		}
		return timings, err
	}
	if hardware {
		d.health.success()
	}
	return timings, nil
}

func (d *Decoder) copyImageTo(dst []byte, dstStride int) (DecodeTimings, error) {
	var timings DecodeTimings
	f := d.frame
	src := f
	if d.hardwareFormat != pixelNone && f.Format == d.hardwareFormat {
		if d.software == nil {
			d.software = d.ff.frameAlloc()
		}
		if d.software == nil {
			return timings, errors.New("decode: allocate hardware transfer frame")
		}
		d.ff.frameUnref(d.software)
		start := qpcNow()
		ret := d.ff.hwFrameTransfer(d.software, f, 0)
		timings.TransferNanos = qpcElapsedNanos(start)
		if ret < 0 {
			return timings, d.ff.error("download hardware frame", ret)
		}
		src = d.software
	}
	w, h := int(src.Width), int(src.Height)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return timings, fmt.Errorf("decode: invalid frame size %dx%d", w, h)
	}
	rowBytes := w * 4
	if dstStride < rowBytes {
		return timings, fmt.Errorf("%w: stride %d shorter than a row (%d)", errShortDestination, dstStride, rowBytes)
	}
	if need := (h-1)*dstStride + rowBytes; len(dst) < need {
		return timings, fmt.Errorf("%w: buffer is %d bytes, need %d", errShortDestination, len(dst), need)
	}
	convertStart := qpcNow()
	// The frame is never rescaled (source and destination are the same size),
	// so point sampling is exact for luma and avoids swscale's filter cost:
	// measured ~1.5x faster than fast bilinear at 1080p and ~1.4x at 1440p.
	flags := int32(scalePoint)
	switch d.options.ScaleFilter {
	case "bilinear":
		flags = scaleBilinear
	case "bicubic":
		flags = scaleBicubic
	}
	d.scaler = d.ff.scaleContext(d.scaler, src.Width, src.Height, src.Format, src.Width, src.Height, pixelRGBA, flags, 0, 0, 0)
	if d.scaler == 0 {
		return timings, errors.New("decode: allocate color converter")
	}
	matrix, full := d.colorConversion()
	coeff := d.ff.scaleCoefficients(matrix)
	if ret := d.ff.scaleColorspace(d.scaler, coeff, full, coeff, 1, 0, 1<<16, 1<<16); ret < 0 {
		return timings, d.ff.error("set color range", ret)
	}
	output := [4]*byte{&dst[0]}
	lines := [4]int32{int32(dstStride)}
	rows := d.ff.scale(d.scaler, &src.Data[0], &src.LineSize[0], 0, src.Height, &output[0], &lines[0])
	runtime.KeepAlive(dst)
	timings.ConvertNanos = qpcElapsedNanos(convertStart)
	if rows != src.Height {
		return timings, fmt.Errorf("decode: converted %d rows, want %d", rows, src.Height)
	}
	return timings, nil
}

// colorConversion uses the frame metadata and the same defaults in both
// conversion paths so a CPU fallback preserves the user's color settings.
func (d *Decoder) colorConversion() (matrix, full int32) {
	f := d.frame
	if d.options.ColorRange == "full" || (d.options.ColorRange != "limited" && (f.ColorRange == colorFull || f.Format == pixelYUVJ420P)) {
		full = 1
	}
	matrix = matrixDefault
	switch f.ColorSpace {
	case spaceBT709:
		matrix = matrixBT709
	case spaceBT2020NCL, spaceBT2020CL:
		matrix = matrixBT2020
	case spaceSMPTE170M, spaceBT470BG:
		matrix = matrixBT601
	}
	return matrix, full
}

// annexBParameterSets returns copies of the H.264 SPS and PPS NAL units (each
// with a 4-byte start code) that precede the first slice of an Annex-B access
// unit, or nil. Scanning stops at the first slice, so a frame without
// parameter sets costs only a start-code search over its leading bytes.
func annexBParameterSets(au []byte) []byte {
	var out []byte
	_, i := findStartCode(au, 0)
	for i >= 0 && i < len(au) {
		nalType := au[i] & 0x1f
		if nalType >= 1 && nalType <= 5 {
			break
		}
		end, next := findStartCode(au, i)
		if end < 0 {
			end = len(au)
		}
		if nalType == 7 || nalType == 8 {
			out = append(out, 0, 0, 0, 1)
			out = append(out, au[i:end]...)
		}
		i = next
	}
	return out
}

// mergeParameterSets replaces each parameter-set type supplied by updates,
// retaining the other type when SPS and PPS arrive in separate packets.
func mergeParameterSets(previous, updates []byte) []byte {
	var out []byte
	for _, typ := range []byte{7, 8} {
		var replacement []byte
		for _, sets := range [][]byte{previous, updates} {
			var selected []byte
			start, payload := findStartCode(sets, 0)
			for payload >= 0 && payload < len(sets) {
				end, next := findStartCode(sets, payload)
				if end < 0 {
					end = len(sets)
				}
				if sets[payload]&0x1f == typ {
					selected = append(selected, sets[start:end]...)
				}
				start, payload = end, next
			}
			if len(selected) > 0 {
				replacement = selected
			}
		}
		out = append(out, replacement...)
	}
	return out
}

// findStartCode locates the first 00 00 01 at or after from. It returns the
// index where the start code begins (including a leading zero of a 4-byte
// code) and the index of the NAL payload, or -1, -1.
func findStartCode(b []byte, from int) (start, payload int) {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			start = i
			if start > from && b[start-1] == 0 {
				start--
			}
			return start, i + 3
		}
	}
	return -1, -1
}

// Version reports the loaded runtime, or the concrete loader error.
func Version() string {
	f, err := loadFFmpeg()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return f.versionInfo()
}
