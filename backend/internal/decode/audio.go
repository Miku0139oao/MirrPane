package decode

/*
#cgo CFLAGS: -I${SRCDIR}/../../../third_party/ffmpeg/include
#cgo LDFLAGS: -L${SRCDIR}/../../../third_party/ffmpeg/lib -lavcodec -lavutil -lswresample

#include <stdlib.h>
#include <string.h>
#include <libavcodec/avcodec.h>
#include <libavutil/error.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>

typedef struct {
    AVCodecContext *ctx;
    AVFrame        *frame;
    AVPacket       *packet;
    struct SwrContext *swr;
    int in_fmt;
    int in_rate;
    int in_channels;
} aac_t;

// aac_new opens the native AAC decoder with an AudioSpecificConfig as extradata.
// AirPlay mirrored audio is AAC-ELD: AOT 39, 44100 Hz, stereo, whose ASC is
// f8 e8 50 00 (renderers/audio_renderer.c in UxPlay passes exactly that as
// codec_data). AudioSpecificConfig is also accepted so a plain AAC-LC stream can
// exercise the same path in tests.
static aac_t* aac_new(const unsigned char *extra, int extra_len, int *err) {
    aac_t *d = (aac_t *) calloc(1, sizeof(aac_t));
    if (!d) { *err = AVERROR(ENOMEM); return NULL; }

    const AVCodec *codec = avcodec_find_decoder(AV_CODEC_ID_AAC);
    if (!codec) { *err = AVERROR_DECODER_NOT_FOUND; free(d); return NULL; }

    d->ctx = avcodec_alloc_context3(codec);
    if (!d->ctx) { *err = AVERROR(ENOMEM); free(d); return NULL; }

    if (extra_len > 0) {
        d->ctx->extradata = (uint8_t *) av_mallocz(extra_len + AV_INPUT_BUFFER_PADDING_SIZE);
        if (!d->ctx->extradata) { *err = AVERROR(ENOMEM); free(d); return NULL; }
        memcpy(d->ctx->extradata, extra, extra_len);
        d->ctx->extradata_size = extra_len;
    }
    d->ctx->request_sample_fmt = AV_SAMPLE_FMT_S16;

    int ret = avcodec_open2(d->ctx, codec, NULL);
    if (ret < 0) { *err = ret; free(d); return NULL; }

    d->frame = av_frame_alloc();
    d->packet = av_packet_alloc();
    if (!d->frame || !d->packet) { *err = AVERROR(ENOMEM); free(d); return NULL; }
    *err = 0;
    return d;
}

static void aac_free(aac_t *d) {
    if (!d) return;
    if (d->swr) swr_free(&d->swr);
    if (d->packet) av_packet_free(&d->packet);
    if (d->frame) av_frame_free(&d->frame);
    if (d->ctx) avcodec_free_context(&d->ctx);
    free(d);
}

static const char* aac_error(int code) {
    static char buf[AV_ERROR_MAX_STRING_SIZE];
    av_strerror(code, buf, sizeof(buf));
    return buf;
}

// aac_decode feeds one AAC access unit. 1 = a frame is ready, 0 = none,
// negative = error.
static int aac_decode(aac_t *d, const unsigned char *data, int len) {
    int ret = av_new_packet(d->packet, len);
    if (ret < 0) return ret;
    memcpy(d->packet->data, data, len);
    d->packet->pts = AV_NOPTS_VALUE;

    ret = avcodec_send_packet(d->ctx, d->packet);
    av_packet_unref(d->packet);
    if (ret < 0 && ret != AVERROR(EAGAIN)) return ret;

    ret = avcodec_receive_frame(d->ctx, d->frame);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    if (ret < 0) return ret;
    return 1;
}

static int aac_channels(aac_t *d) { return d->frame->ch_layout.nb_channels; }
static int aac_rate(aac_t *d)     { return d->frame->sample_rate; }
static int aac_samples(aac_t *d)  { return d->frame->nb_samples; }

// aac_resample converts the current frame to interleaved signed 16-bit at a
// fixed 48 kHz stereo output, which is what the audio device wants. Returns the
// number of frames written, or a negative AVERROR.
static int aac_resample(aac_t *d, short *out, int max_frames) {
    AVFrame *f = d->frame;

    int channels = f->ch_layout.nb_channels;
    if (!d->swr || d->in_fmt != f->format || d->in_rate != f->sample_rate ||
        d->in_channels != channels) {
        if (d->swr) swr_free(&d->swr);
        AVChannelLayout out_layout;
        av_channel_layout_default(&out_layout, channels);
        int ret = swr_alloc_set_opts2(&d->swr,
            &out_layout, AV_SAMPLE_FMT_S16, 48000,
            &f->ch_layout, (enum AVSampleFormat) f->format, f->sample_rate,
            0, NULL);
        if (ret < 0) { av_channel_layout_uninit(&out_layout); return ret; }
        ret = swr_init(d->swr);
        av_channel_layout_uninit(&out_layout);
        if (ret < 0) { swr_free(&d->swr); return ret; }
        d->in_fmt = f->format;
        d->in_rate = f->sample_rate;
        d->in_channels = channels;
    }

    // Delayed samples from previous frames are flushed by asking for more than
    // the input provides; swr_convert returns what it actually produced.
    int produced = swr_convert(d->swr, (uint8_t **) &out, max_frames,
                               (const uint8_t **) f->data, f->nb_samples);
    if (produced < 0) return produced;
    return produced;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// EldConfig is the AudioSpecificConfig for AAC-ELD at 44100 Hz, stereo:
// audioObjectType 39, samplingFrequencyIndex 4, channelConfiguration 2.
var EldConfig = []byte{0xf8, 0xe8, 0x50, 0x00}

// OutputRate is the sample rate every AudioFrame is resampled to.
const OutputRate = 48000

// AudioFrame is one decoded AAC access unit, interleaved signed 16-bit at
// OutputRate. Samples is reused between calls.
type AudioFrame struct {
	Samples    []int16
	Channels   int
	SampleRate int // always OutputRate
}

// Frames reports how many sample frames the buffer holds.
func (f *AudioFrame) Frames() int {
	if f.Channels <= 0 {
		return 0
	}
	return len(f.Samples) / f.Channels
}

// AudioDecoder decodes AAC-ELD. It is not safe for concurrent use.
type AudioDecoder struct {
	h       *C.aac_t
	frame   AudioFrame
	scratch []C.short
}

// NewAudioDecoder starts an AAC decoder configured with the given
// AudioSpecificConfig. Passing nil uses EldConfig, which is what mirroring
// senders produce.
func NewAudioDecoder(config []byte) (*AudioDecoder, error) {
	if len(config) == 0 {
		config = EldConfig
	}
	var cerr C.int
	h := C.aac_new((*C.uchar)(unsafe.Pointer(&config[0])), C.int(len(config)), &cerr)
	if h == nil {
		return nil, fmt.Errorf("decode: open aac: %s", C.GoString(C.aac_error(cerr)))
	}
	// 480 input samples at 44.1 kHz become ~522 at 48 kHz; leave generous room
	// for the resampler's delay line.
	d := &AudioDecoder{h: h, scratch: make([]C.short, 8192*2)}
	d.frame.SampleRate = OutputRate
	return d, nil
}

// Close releases the decoder.
func (d *AudioDecoder) Close() {
	if d.h != nil {
		C.aac_free(d.h)
		d.h = nil
	}
}

// Decode feeds one raw AAC access unit (no ADTS header) and returns the decoded
// audio, or ErrNoFrame when the decoder is still filling its window.
func (d *AudioDecoder) Decode(accessUnit []byte) (*AudioFrame, error) {
	if d.h == nil {
		return nil, errors.New("decode: audio decoder is closed")
	}
	if len(accessUnit) == 0 {
		return nil, ErrNoFrame
	}
	ret := C.aac_decode(d.h, (*C.uchar)(unsafe.Pointer(&accessUnit[0])), C.int(len(accessUnit)))
	switch {
	case ret < 0:
		return nil, fmt.Errorf("decode: aac: %s", C.GoString(C.aac_error(ret)))
	case ret == 0:
		return nil, ErrNoFrame
	}

	n := int(C.aac_resample(d.h, &d.scratch[0], C.int(len(d.scratch)/2)))
	if n < 0 {
		return nil, fmt.Errorf("decode: aac resample: %s", C.GoString(C.aac_error(C.int(n))))
	}
	channels := int(C.aac_channels(d.h))
	if n == 0 || channels <= 0 {
		return nil, ErrNoFrame
	}

	need := n * channels
	if cap(d.frame.Samples) < need {
		d.frame.Samples = make([]int16, need)
	}
	d.frame.Samples = d.frame.Samples[:need]
	d.frame.Channels = channels
	for i := 0; i < need; i++ {
		d.frame.Samples[i] = int16(d.scratch[i])
	}
	return &d.frame, nil
}
