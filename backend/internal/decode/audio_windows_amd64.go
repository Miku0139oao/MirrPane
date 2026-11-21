package decode

import (
	"bytes"
	"errors"
	"runtime"
	"unsafe"
)

var EldConfig = []byte{0xf8, 0xe8, 0x50, 0x00}

const OutputRate = 48000

const outputChannels = 2

// AudioFrame contains interleaved 16-bit PCM, reused by the next Decode call.
type AudioFrame struct {
	Samples    []int16
	Channels   int
	SampleRate int
}

func (f *AudioFrame) Frames() int {
	if f.Channels <= 0 {
		return 0
	}
	return len(f.Samples) / f.Channels
}

// AudioDecoder owns FFmpeg objects and a reusable PCM buffer. Its owner must
// serialize Decode, Configure, Flush and Close; audio.Receiver uses decMu.
type AudioDecoder struct {
	ff                                    *ffmpegAPI
	ctx                                   *codecContext
	nativeFrame                           *avFrame
	packet                                *avPacket
	resampler                             uintptr
	inputFormat, inputRate, inputChannels int32
	frame                                 AudioFrame
	config                                []byte
}

func NewAudioDecoder(config []byte) (*AudioDecoder, error) {
	if len(config) == 0 {
		config = EldConfig
	}
	if len(config) > 65536 {
		return nil, errors.New("decode: AAC config too large")
	}
	f, err := loadFFmpeg()
	if err != nil {
		return nil, err
	}
	d := &AudioDecoder{ff: f}
	codec := f.findDecoder(codecAAC)
	if codec == 0 {
		return nil, errors.New("decode: AAC codec unavailable")
	}
	d.ctx = f.allocContext(codec)
	if d.ctx == nil {
		return nil, errors.New("decode: allocate AAC context")
	}
	d.ctx.ExtraData = f.mallocz(uintptr(len(config) + inputPadding))
	if d.ctx.ExtraData == nil {
		d.Close()
		return nil, errors.New("decode: allocate AAC config")
	}
	copy(unsafe.Slice(d.ctx.ExtraData, len(config)), config)
	d.ctx.ExtraDataSize = int32(len(config))
	d.ctx.RequestSampleFormat = sampleS16
	if ret := f.openCodec(d.ctx, codec, 0); ret < 0 {
		d.Close()
		return nil, f.error("open AAC", ret)
	}
	d.nativeFrame, d.packet = f.frameAlloc(), f.packetAlloc()
	if d.nativeFrame == nil || d.packet == nil {
		d.Close()
		return nil, errors.New("decode: allocate AAC buffers")
	}
	d.frame.SampleRate = OutputRate
	d.frame.Channels = outputChannels
	d.config = append([]byte(nil), config...)
	return d, nil
}

// Configure resets a stream, retaining the native decoder when the
// AudioSpecificConfig is unchanged. A failed replacement leaves it usable.
func (d *AudioDecoder) Configure(config []byte) error {
	if len(config) == 0 {
		config = EldConfig
	}
	if d.ctx != nil && bytes.Equal(d.config, config) {
		d.Flush()
		return nil
	}
	next, err := NewAudioDecoder(config)
	if err != nil {
		return err
	}
	d.Close()
	*d = *next
	return nil
}

// Flush drops codec history and resampler delay without reopening the codec.
func (d *AudioDecoder) Flush() {
	if d.ctx == nil {
		return
	}
	d.ff.flushBuffers(d.ctx)
	d.ff.packetUnref(d.packet)
	d.ff.frameUnref(d.nativeFrame)
	if d.resampler != 0 {
		d.ff.resampleFree(&d.resampler)
	}
	d.inputFormat, d.inputRate, d.inputChannels = 0, 0, 0
	d.frame.Samples = d.frame.Samples[:0]
}

func (d *AudioDecoder) Close() {
	if d.resampler != 0 {
		d.ff.resampleFree(&d.resampler)
	}
	if d.packet != nil {
		d.ff.packetFree(&d.packet)
	}
	if d.nativeFrame != nil {
		d.ff.frameFree(&d.nativeFrame)
	}
	if d.ctx != nil {
		d.ff.freeContext(&d.ctx)
	}
	d.frame.Samples = nil
}

func (d *AudioDecoder) Decode(data []byte) (*AudioFrame, error) {
	if d.ctx == nil {
		return nil, errors.New("decode: audio decoder is closed")
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
		return nil, d.ff.error("send AAC packet", ret)
	}
	ret = d.ff.receiveFrame(d.ctx, d.nativeFrame)
	if ret == errorAgain || ret == errorEOF {
		return nil, ErrNoFrame
	}
	if ret < 0 {
		return nil, d.ff.error("receive AAC frame", ret)
	}
	return d.convertFrame(d.nativeFrame)
}

func (d *AudioDecoder) convertFrame(f *avFrame) (*AudioFrame, error) {
	channels := f.ChannelLayout.Channels
	if channels < 1 || channels > 8 || f.SampleRate <= 0 || f.Samples <= 0 || f.Samples > 65536 {
		return nil, errors.New("decode: invalid AAC frame format")
	}
	if d.resampler == 0 || d.inputFormat != f.Format || d.inputRate != f.SampleRate || d.inputChannels != channels {
		if d.resampler != 0 {
			d.ff.resampleFree(&d.resampler)
		}
		var output channelLayout
		d.ff.layoutDefault(&output, outputChannels)
		ret := d.ff.resampleAlloc(&d.resampler, &output, sampleS16, OutputRate, &f.ChannelLayout, f.Format, f.SampleRate, 0, 0)
		d.ff.layoutUninit(&output)
		if ret < 0 {
			return nil, d.ff.error("allocate resampler", ret)
		}
		if ret = d.ff.resampleInit(d.resampler); ret < 0 {
			d.ff.resampleFree(&d.resampler)
			return nil, d.ff.error("initialize resampler", ret)
		}
		d.inputFormat, d.inputRate, d.inputChannels = f.Format, f.SampleRate, channels
	}
	// Enough output space for a full AAC frame plus resampler delay, including
	// streams whose source rate is lower than the 48 kHz output.
	capacity := (int64(f.Samples)*OutputRate+int64(f.SampleRate)-1)/int64(f.SampleRate) + 256
	if capacity > 1<<20 {
		return nil, errors.New("decode: AAC resample buffer too large")
	}
	need := int(capacity) * outputChannels
	if cap(d.frame.Samples) < need {
		d.frame.Samples = make([]int16, need)
	}
	d.frame.Samples = d.frame.Samples[:need]
	out := (*byte)(unsafe.Pointer(&d.frame.Samples[0]))
	ret := d.ff.resampleConvert(d.resampler, &out, int32(capacity), f.ExtendedData, f.Samples)
	runtime.KeepAlive(d.frame.Samples)
	if ret < 0 {
		return nil, d.ff.error("resample AAC", ret)
	}
	if ret == 0 {
		return nil, ErrNoFrame
	}
	d.frame.Samples = d.frame.Samples[:int(ret)*outputChannels]
	d.frame.Channels = outputChannels
	return &d.frame, nil
}
