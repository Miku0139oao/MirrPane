// Package decode turns AirPlay H.264 and AAC access units into pictures and PCM.
// FFmpeg's shared libraries are called through the Windows ABI without cgo.
package decode

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"unsafe"
)

var ErrNoFrame = errors.New("decode: no frame ready")

// Image is one decoded picture. Pixels is reused by the next Decode call.
type Image struct {
	Width, Height int
	Stride        int
	Pixels        []byte
}

func (i *Image) Bounds() image.Rectangle { return image.Rect(0, 0, i.Width, i.Height) }

type Options struct {
	PreferHardware bool
	ColorRange     string // auto | full | limited
	ScaleFilter    string // fast | bilinear | bicubic
}

// Decoder owns FFmpeg-allocated native objects. Calls must be serialized with
// Close; frameSink provides this for the receiver and snapshot endpoint.
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
	pending                []byte
}

func NewDecoder() (*Decoder, error) {
	return NewDecoderWithOptions(Options{PreferHardware: true, ColorRange: "auto", ScaleFilter: "fast"})
}

func NewDecoderWithOptions(opts Options) (*Decoder, error) {
	f, err := loadFFmpeg()
	if err != nil {
		return nil, err
	}
	d := &Decoder{ff: f, options: opts, hardwareFormat: pixelNone}
	codec := f.findDecoder(codecH264)
	if codec == 0 {
		return nil, errors.New("decode: H.264 codec unavailable")
	}
	d.ctx = f.allocContext(codec)
	if d.ctx == nil {
		return nil, errors.New("decode: allocate H.264 context")
	}
	d.ctx.ThreadCount = 0
	d.ctx.ThreadType = threadSlice
	d.ctx.Flags |= flagLowDelay
	d.ctx.Flags2 |= flagFast
	if opts.PreferHardware {
		for i := int32(0); ; i++ {
			config := f.getHWConfig(codec, i)
			if config == nil {
				break
			}
			if config.Methods&hwDeviceContext != 0 && config.DeviceType == deviceD3D11VA {
				if f.hwDeviceCreate(&d.device, deviceD3D11VA, 0, 0, 0) >= 0 {
					d.ctx.HWDeviceContext = f.bufferRef(d.device)
					if d.ctx.HWDeviceContext != 0 {
						d.hardwareFormat = config.PixelFormat
						d.ctx.Opaque = uintptr(config.PixelFormat + 1)
						d.ctx.GetFormat = f.formatCallback
					}
				}
				break
			}
		}
	}
	if ret := f.openCodec(d.ctx, codec, 0); ret < 0 {
		d.Close()
		return nil, f.error("open H.264", ret)
	}
	d.frame, d.packet, d.parser = f.frameAlloc(), f.packetAlloc(), f.parserInit(codecH264)
	if d.frame == nil || d.packet == nil || d.parser == 0 {
		d.Close()
		return nil, errors.New("decode: allocate H.264 buffers")
	}
	return d, nil
}

func (d *Decoder) Hardware() bool {
	return d.ctx != nil && d.ready && d.hardwareFormat != pixelNone && d.frame.Format == d.hardwareFormat
}

func (d *Decoder) LastImage() *Image {
	if !d.ready {
		return nil
	}
	return &d.img
}

func (d *Decoder) Close() {
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
	d.pending = nil
	d.img = Image{}
	d.ready = false
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

func (d *Decoder) DecodeAccessUnit(data []byte) (*Image, error) {
	if d.ctx == nil {
		return nil, errors.New("decode: decoder is closed")
	}
	if len(data) == 0 {
		return nil, ErrNoFrame
	}
	if err := d.ff.packet(d.packet, data); err != nil {
		return nil, err
	}
	ret := d.ff.sendPacket(d.ctx, d.packet)
	d.ff.packetUnref(d.packet)
	if ret < 0 {
		return nil, d.ff.error("send H.264 packet", ret)
	}
	ret = d.ff.receiveFrame(d.ctx, d.frame)
	if ret == errorAgain || ret == errorEOF {
		return nil, ErrNoFrame
	}
	if ret < 0 {
		return nil, d.ff.error("receive H.264 frame", ret)
	}
	return d.copyImage()
}

func (d *Decoder) copyImage() (*Image, error) {
	f := d.frame
	if d.hardwareFormat != pixelNone && f.Format == d.hardwareFormat {
		if d.software == nil {
			d.software = d.ff.frameAlloc()
		}
		if d.software == nil {
			return nil, errors.New("decode: allocate hardware transfer frame")
		}
		d.ff.frameUnref(d.software)
		if ret := d.ff.hwFrameTransfer(d.software, f, 0); ret < 0 {
			return nil, d.ff.error("download hardware frame", ret)
		}
		f = d.software
	}
	w, h := int(f.Width), int(f.Height)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return nil, fmt.Errorf("decode: invalid frame size %dx%d", w, h)
	}
	flags := int32(scaleFast)
	switch d.options.ScaleFilter {
	case "bilinear":
		flags = scaleBilinear
	case "bicubic":
		flags = scaleBicubic
	}
	d.scaler = d.ff.scaleContext(d.scaler, f.Width, f.Height, f.Format, f.Width, f.Height, pixelRGBA, flags, 0, 0, 0)
	if d.scaler == 0 {
		return nil, errors.New("decode: allocate color converter")
	}
	full := int32(0)
	if d.options.ColorRange == "full" || (d.options.ColorRange != "limited" && (d.frame.ColorRange == colorFull || d.frame.Format == pixelYUVJ420P)) {
		full = 1
	}
	matrix := int32(matrixDefault)
	switch d.frame.ColorSpace {
	case spaceBT709:
		matrix = matrixBT709
	case spaceBT2020NCL, spaceBT2020CL:
		matrix = matrixBT2020
	case spaceSMPTE170M, spaceBT470BG:
		matrix = matrixBT601
	}
	coeff := d.ff.scaleCoefficients(matrix)
	if ret := d.ff.scaleColorspace(d.scaler, coeff, full, coeff, 1, 0, 1<<16, 1<<16); ret < 0 {
		return nil, d.ff.error("set color range", ret)
	}
	stride := w * 4
	need := stride * h
	if cap(d.img.Pixels) < need {
		d.img.Pixels = make([]byte, need)
	}
	d.img = Image{Width: w, Height: h, Stride: stride, Pixels: d.img.Pixels[:need]}
	output := [4]*byte{&d.img.Pixels[0]}
	lines := [4]int32{int32(stride)}
	rows := d.ff.scale(d.scaler, &f.Data[0], &f.LineSize[0], 0, f.Height, &output[0], &lines[0])
	runtime.KeepAlive(d.img.Pixels)
	if rows != f.Height {
		return nil, fmt.Errorf("decode: converted %d rows, want %d", rows, f.Height)
	}
	d.ready = true
	return &d.img, nil
}

// Version reports the loaded runtime, or the concrete loader error.
func Version() string {
	f, err := loadFFmpeg()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return f.versionInfo()
}
