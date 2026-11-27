// Package decode turns AirPlay H.264 and AAC access units into pictures and PCM.
// FFmpeg's shared libraries are called through the Windows ABI without cgo.
package decode

import (
	"errors"
	"fmt"
	"image"
	"log"
	"runtime"
	"time"
	"unsafe"
)

var ErrNoFrame = errors.New("decode: no frame ready")

// errShortDestination means the caller's buffer cannot hold the picture. It is
// a contract error, not a decode failure, so it must not trip hardware recovery.
var errShortDestination = errors.New("decode: destination too small")

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
