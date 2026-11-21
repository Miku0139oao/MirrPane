// Package decode turns the AirPlay mirror stream's H.264 access units into RGBA
// images using FFmpeg's libavcodec and libswscale.
//
// The build needs the FFmpeg development package: headers, import libraries and
// the runtime DLLs. See third_party/README.md for how it is fetched. Both the
// CFLAGS and LDFLAGS below resolve relative to this file, so the package builds
// from any working directory.
package decode

/*
#cgo CFLAGS: -I${SRCDIR}/../../../third_party/ffmpeg/include
#cgo LDFLAGS: -L${SRCDIR}/../../../third_party/ffmpeg/lib -lavcodec -lavutil -lswscale

#include <stdlib.h>
#include <string.h>
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/error.h>
#include <libavutil/log.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_d3d11va.h>
#include <libswscale/swscale.h>

// FFmpeg writes a deprecation warning on every frame for the full-range YUV
// pixel format iOS sends. It is not an error and it floods the receiver log, so
// only errors are printed; failures surface through the API anyway.
static void ffmpeg_quiet(void) { av_log_set_level(AV_LOG_ERROR); }

// Decoding state lives in C so the Go side never touches FFmpeg structs, whose
// layout changes between versions and cannot be described safely in cgo.
typedef struct {
    AVCodecContext *ctx;
    AVCodecParserContext *parser;
    const AVFrame  *frame;
    AVPacket       *packet;
    AVPacket       *parsed;
    struct SwsContext *sws;
    // Hardware decoding: hw_device is the D3D11VA device, swframe receives the
    // downloaded picture, and hw_pix_fmt is the format the decoder chose
    // (AV_PIX_FMT_NONE when decoding in software).
    AVBufferRef    *hw_device;
    AVFrame        *swframe;
    enum AVPixelFormat hw_pix_fmt;
    int             hw_ok;
    int             color_range_mode; // 0=auto, 1=full, 2=limited
    int             sws_flags;
    // pending holds input the parser has not yet turned into an access unit.
    // One Decode call returns one picture, so anything beyond it waits here.
    unsigned char *pending;
    int pending_len, pending_cap;
} decoder_t;

// decoder_get_format picks the D3D11 pixel format when the device exists.
//
// libavcodec calls this while opening: the list is what the stream can be
// decoded to. Only the format that pairs with a D3D11VA device context
// (AV_PIX_FMT_D3D11, found through avcodec_get_hw_config) is acceptable — the
// list also offers vulkan, cuda, dxva2 and the legacy d3d11va_vld, each of which
// fails against our device and silently falls back to software.
static enum AVPixelFormat decoder_get_format(AVCodecContext *ctx,
                                             const enum AVPixelFormat *pix_fmts) {
    decoder_t *d = (decoder_t *) ctx->opaque;
    if (d && d->hw_ok && d->hw_pix_fmt != AV_PIX_FMT_NONE) {
        for (const enum AVPixelFormat *p = pix_fmts; *p != AV_PIX_FMT_NONE; p++) {
            if (*p == d->hw_pix_fmt) return *p;
        }
    }
    // No hardware format offered: pick the first software one.
    for (const enum AVPixelFormat *p = pix_fmts; *p != AV_PIX_FMT_NONE; p++) {
        const AVPixFmtDescriptor *desc = av_pix_fmt_desc_get(*p);
        if (desc && !(desc->flags & AV_PIX_FMT_FLAG_HWACCEL)) return *p;
    }
    return pix_fmts[0];
}

// decoder_find_hw_format returns the pixel format the codec decodes to on a
// D3D11VA device, or AV_PIX_FMT_NONE when the codec has no such path.
static enum AVPixelFormat decoder_find_hw_format(const AVCodec *codec) {
    for (int i = 0;; i++) {
        const AVCodecHWConfig *cfg = avcodec_get_hw_config(codec, i);
        if (!cfg) return AV_PIX_FMT_NONE;
        if ((cfg->methods & AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX) &&
            cfg->device_type == AV_HWDEVICE_TYPE_D3D11VA) {
            return cfg->pix_fmt;
        }
    }
}

static void decoder_free(decoder_t *d);

static decoder_t* decoder_new(int prefer_hw, int color_range_mode, int sws_flags, int *err) {
    decoder_t *d = (decoder_t *) calloc(1, sizeof(decoder_t));
    if (!d) { *err = AVERROR(ENOMEM); return NULL; }

    const AVCodec *codec = avcodec_find_decoder(AV_CODEC_ID_H264);
    if (!codec) { *err = AVERROR_DECODER_NOT_FOUND; free(d); return NULL; }

    d->ctx = avcodec_alloc_context3(codec);
    if (!d->ctx) { *err = AVERROR(ENOMEM); free(d); return NULL; }
    d->ctx->thread_count = 0;   // let FFmpeg pick, it uses every core
    d->ctx->thread_type = FF_THREAD_SLICE; // never queue frames for frame-level threading
    d->ctx->flags |= AV_CODEC_FLAG_LOW_DELAY;
    d->ctx->flags2 |= AV_CODEC_FLAG2_FAST;
    d->hw_pix_fmt = AV_PIX_FMT_NONE;
    d->color_range_mode = color_range_mode;
    d->sws_flags = sws_flags ? sws_flags : SWS_FAST_BILINEAR;

    // Hardware decoding is optional: a machine without D3D11, or a driver that
    // refuses, keeps the software path and still works.
    if (prefer_hw) {
        d->hw_pix_fmt = decoder_find_hw_format(codec);
        if (d->hw_pix_fmt != AV_PIX_FMT_NONE &&
            av_hwdevice_ctx_create(&d->hw_device, AV_HWDEVICE_TYPE_D3D11VA,
                                   NULL, NULL, 0) >= 0) {
            d->hw_ok = 1;
            d->ctx->opaque = d;
            d->ctx->get_format = decoder_get_format;
            d->ctx->hw_device_ctx = av_buffer_ref(d->hw_device);
            if (!d->ctx->hw_device_ctx) d->hw_ok = 0;
        }
    }

    int ret = avcodec_open2(d->ctx, codec, NULL);
    if (ret < 0) { *err = ret; decoder_free(d); return NULL; }

    d->frame = av_frame_alloc();
    d->packet = av_packet_alloc();
    d->parsed = av_packet_alloc();
    d->parser = av_parser_init(AV_CODEC_ID_H264);
    if (!d->frame || !d->packet || !d->parsed || !d->parser) {
        *err = AVERROR(ENOMEM);
        decoder_free(d);
        return NULL;
    }
    *err = 0;
    return d;
}

static void decoder_free(decoder_t *d) {
    if (!d) return;
    if (d->sws) sws_freeContext(d->sws);
    if (d->swframe) av_frame_free(&d->swframe);
    if (d->hw_device) av_buffer_unref(&d->hw_device);
    if (d->parsed) av_packet_free(&d->parsed);
    if (d->pending) free(d->pending);
    if (d->parser) av_parser_close(d->parser);
    if (d->packet) av_packet_free(&d->packet);
    if (d->frame) av_frame_free((AVFrame **) &d->frame);
    if (d->ctx) avcodec_free_context(&d->ctx);
    free(d);
}

static const char* decoder_error(int code) {
    static char buf[AV_ERROR_MAX_STRING_SIZE];
    av_strerror(code, buf, sizeof(buf));
    return buf;
}

// decoder_feed appends input to the pending buffer.
static int decoder_feed(decoder_t *d, const unsigned char *data, int len) {
    if (len <= 0) return 0;
    if (d->pending_len + len > d->pending_cap) {
        int cap = d->pending_cap ? d->pending_cap : 65536;
        while (cap < d->pending_len + len) cap *= 2;
        unsigned char *p = (unsigned char *) realloc(d->pending, cap);
        if (!p) return AVERROR(ENOMEM);
        d->pending = p;
        d->pending_cap = cap;
    }
    memcpy(d->pending + d->pending_len, data, len);
    d->pending_len += len;
    return 0;
}

// decoder_decode feeds a chunk of Annex-B bitstream and returns 1 when a frame
// is ready, 0 when more data is needed, or a negative AVERROR.
//
// The bitstream goes through FFmpeg's H.264 parser first. AirPlay hands over
// access units, but a byte-stream capture or a packet that spans a frame
// boundary would otherwise be fed to the decoder as one enormous access unit,
// which libavcodec does not split on its own.
static int decoder_decode(decoder_t *d, const unsigned char *data, int len) {
    int ret = decoder_feed(d, data, len);
    if (ret < 0) return ret;

    int consumed_total = 0;
    while (consumed_total < d->pending_len) {
        int consumed = av_parser_parse2(d->parser, d->ctx,
            &d->parsed->data, &d->parsed->size,
            d->pending + consumed_total, d->pending_len - consumed_total,
            AV_NOPTS_VALUE, AV_NOPTS_VALUE, 0);
        if (consumed < 0) return consumed;
        consumed_total += consumed;

        if (d->parsed->size > 0) {
            d->parsed->pts = AV_NOPTS_VALUE;
            ret = avcodec_send_packet(d->ctx, d->parsed);
            av_packet_unref(d->parsed);
            if (ret < 0 && ret != AVERROR(EAGAIN)) return ret;

            ret = avcodec_receive_frame(d->ctx, (AVFrame *) d->frame);
            if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) continue;
            if (ret < 0) return ret;

            // A picture came out; keep the rest of the input for the next call.
            memmove(d->pending, d->pending + consumed_total, d->pending_len - consumed_total);
            d->pending_len -= consumed_total;
            return 1;
        }
        if (consumed == 0) break;   // parser needs more input to make progress
    }

    memmove(d->pending, d->pending + consumed_total, d->pending_len - consumed_total);
    d->pending_len -= consumed_total;
    // Keep the buffer bounded: an input the parser never accepts would grow
    // without limit. A 4 MiB backlog is already far past a single access unit.
    if (d->pending_len > (4 << 20)) d->pending_len = 0;

    ret = avcodec_receive_frame(d->ctx, (AVFrame *) d->frame);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    if (ret < 0) return ret;
    return 1;
}

// decoder_decode_packet decodes one complete Annex-B access unit directly,
// skipping av_parser_parse2. The H.264 parser holds an access unit until the
// next start code arrives, which adds one full frame (~16.7 ms) of latency and
// stalls the last frame when the sender's screen goes static.
static int decoder_decode_packet(decoder_t *d, const unsigned char *data, int len) {
    if (len <= 0) return 0;
    d->packet->data = (uint8_t *) data;
    d->packet->size = len;
    d->packet->pts = AV_NOPTS_VALUE;
    int ret = avcodec_send_packet(d->ctx, d->packet);
    d->packet->data = NULL;
    d->packet->size = 0;
    if (ret < 0 && ret != AVERROR(EAGAIN)) return ret;

    ret = avcodec_receive_frame(d->ctx, (AVFrame *) d->frame);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    if (ret < 0) return ret;
    return 1;
}

static int decoder_width(decoder_t *d)  { return d->frame->width; }
static int decoder_height(decoder_t *d) { return d->frame->height; }

// decoder_hardware reports whether the last picture was decoded on the GPU.
// It looks at the frame itself: libavcodec falls back to software silently
// when hwaccel setup fails, so the configured format proves nothing.
static int decoder_hardware(decoder_t *d) {
    return d->hw_pix_fmt != AV_PIX_FMT_NONE && d->frame->format == d->hw_pix_fmt;
}

// decoder_copy_rgba converts the current frame to RGBA and writes exactly
// stride*height bytes into dst. Returns 0 on success, a negative AVERROR on
// failure.
//
// A hardware-decoded picture lives in GPU memory, which sws_scale cannot read,
// so it is downloaded into swframe first. That download is unavoidable: the
// texture Flutter renders from is CPU memory.
static int decoder_copy_rgba(decoder_t *d, unsigned char *dst, int stride) {
    AVFrame *f = (AVFrame *) d->frame;

    if (d->hw_pix_fmt != AV_PIX_FMT_NONE && f->format == d->hw_pix_fmt) {
        if (!d->swframe) {
            d->swframe = av_frame_alloc();
            if (!d->swframe) return AVERROR(ENOMEM);
        }
        av_frame_unref(d->swframe);
        int ret = av_hwframe_transfer_data(d->swframe, f, 0);
        if (ret < 0) return ret;
        f = d->swframe;
    }

    d->sws = sws_getCachedContext(d->sws,
        f->width, f->height, (enum AVPixelFormat) f->format,
        f->width, f->height, AV_PIX_FMT_RGBA,
        d->sws_flags, NULL, NULL, NULL);
    if (!d->sws) return AVERROR(EINVAL);

    // Colour range and matrix come from the decoded frame, not the pixel
    // format. iOS sends full-range video: software decoding reports it as
    // yuvj420p, which swscale understands, but the hardware download is plain
    // NV12 and would be treated as limited range — washed-out blacks and
    // clipped whites. Tell swscale explicitly, for both paths, or honour an
    // explicit user override.
    const AVFrame *src = d->frame;
    int full;
    if (d->color_range_mode == 1) {
        full = 1;
    } else if (d->color_range_mode == 2) {
        full = 0;
    } else {
        full = src->color_range == AVCOL_RANGE_JPEG ||
               src->format == AV_PIX_FMT_YUVJ420P;
    }
    int cs = SWS_CS_DEFAULT;
    switch (src->colorspace) {
    case AVCOL_SPC_BT709:      cs = SWS_CS_ITU709; break;
    case AVCOL_SPC_BT2020_NCL:
    case AVCOL_SPC_BT2020_CL:  cs = SWS_CS_BT2020; break;
    case AVCOL_SPC_SMPTE170M:
    case AVCOL_SPC_BT470BG:    cs = SWS_CS_ITU601; break;
    default: break;
    }
    const int *coef = sws_getCoefficients(cs);
    sws_setColorspaceDetails(d->sws, coef, full, coef, 1, 0, 1 << 16, 1 << 16);

    uint8_t *dst_data[4] = { dst, NULL, NULL, NULL };
    int dst_linesize[4] = { stride, 0, 0, 0 };
    int rows = sws_scale(d->sws, (const uint8_t * const *) f->data, f->linesize,
                         0, f->height, dst_data, dst_linesize);
    if (rows != f->height) return AVERROR(EINVAL);
    return 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

// ErrNoFrame reports that the bitstream needs more data before a picture exists.
var ErrNoFrame = errors.New("decode: no frame ready")

func init() { C.ffmpeg_quiet() }

// Image is one decoded picture in RGBA byte order, rows packed with Stride
// bytes each. The buffer is reused between calls: copy it if you need to keep it.
type Image struct {
	Width, Height int
	Stride        int
	Pixels        []byte // len == Stride*Height
}

// Decoder is an H.264 decoder. It is not safe for concurrent use.
type Decoder struct {
	h     *C.decoder_t
	img   Image
	ready bool
}

// Options configures how the H.264 decoder opens and converts pictures.
type Options struct {
	PreferHardware bool
	ColorRange     string // "auto" | "full" | "limited"
	ScaleFilter    string // "fast" | "bilinear" | "bicubic"
}

// NewDecoder starts an H.264 decoder with default hardware-first options.
func NewDecoder() (*Decoder, error) {
	return NewDecoderWithOptions(Options{
		PreferHardware: true,
		ColorRange:     "auto",
		ScaleFilter:    "fast",
	})
}

// NewDecoderWithOptions starts an H.264 decoder configured with opts.
func NewDecoderWithOptions(opts Options) (*Decoder, error) {
	preferHW := C.int(0)
	if opts.PreferHardware {
		preferHW = 1
	}
	colorMode := C.int(0)
	switch opts.ColorRange {
	case "full":
		colorMode = 1
	case "limited":
		colorMode = 2
	}
	swsFlags := C.int(C.SWS_FAST_BILINEAR)
	switch opts.ScaleFilter {
	case "bilinear":
		swsFlags = C.SWS_BILINEAR
	case "bicubic":
		swsFlags = C.SWS_BICUBIC
	}
	var cerr C.int
	h := C.decoder_new(preferHW, colorMode, swsFlags, &cerr)
	if h == nil {
		return nil, fmt.Errorf("decode: open h264: %s", C.GoString(C.decoder_error(cerr)))
	}
	return &Decoder{h: h}, nil
}

// Hardware reports whether pictures are being decoded on the GPU. It is only
// meaningful once a frame has been decoded: the format is chosen when the first
// slice arrives, not when the decoder is opened.
func (d *Decoder) Hardware() bool {
	if d.h == nil {
		return false
	}
	return C.decoder_hardware(d.h) != 0
}

// LastImage returns the most recently decoded RGBA picture, or nil if none has
// been decoded yet. Must not be called concurrently with Decode/DecodeAccessUnit.
func (d *Decoder) LastImage() *Image {
	if !d.ready || len(d.img.Pixels) == 0 {
		return nil
	}
	return &d.img
}

// Close releases the decoder and every buffer it owns.
func (d *Decoder) Close() {
	if d.h != nil {
		C.decoder_free(d.h)
		d.h = nil
	}
	d.img = Image{}
	d.ready = false
}

// Decode feeds a chunk of Annex-B bitstream through the H.264 parser. Use
// DecodeAccessUnit when the caller already frames complete access units.
func (d *Decoder) Decode(accessUnit []byte) (*Image, error) {
	if d.h == nil {
		return nil, errors.New("decode: decoder is closed")
	}
	if len(accessUnit) == 0 {
		return nil, ErrNoFrame
	}

	ret := C.decoder_decode(d.h,
		(*C.uchar)(unsafe.Pointer(&accessUnit[0])), C.int(len(accessUnit)))
	return d.finishDecode(ret)
}

// DecodeAccessUnit decodes one complete Annex-B access unit immediately without
// parser buffering, saving one frame (~16.7 ms) of latency.
func (d *Decoder) DecodeAccessUnit(accessUnit []byte) (*Image, error) {
	if d.h == nil {
		return nil, errors.New("decode: decoder is closed")
	}
	if len(accessUnit) == 0 {
		return nil, ErrNoFrame
	}

	ret := C.decoder_decode_packet(d.h,
		(*C.uchar)(unsafe.Pointer(&accessUnit[0])), C.int(len(accessUnit)))
	return d.finishDecode(ret)
}

func (d *Decoder) finishDecode(ret C.int) (*Image, error) {
	switch {
	case ret < 0:
		return nil, fmt.Errorf("decode: %s", C.GoString(C.decoder_error(ret)))
	case ret == 0:
		return nil, ErrNoFrame
	}

	w := int(C.decoder_width(d.h))
	h := int(C.decoder_height(d.h))
	if w <= 0 || h <= 0 {
		return nil, ErrNoFrame
	}

	stride := w * 4
	need := stride * h
	if !d.ready || len(d.img.Pixels) < need || d.img.Width != w || d.img.Height != h {
		d.img = Image{Width: w, Height: h, Stride: stride, Pixels: make([]byte, need)}
		d.ready = true
	}

	ret = C.decoder_copy_rgba(d.h, (*C.uchar)(unsafe.Pointer(&d.img.Pixels[0])), C.int(stride))
	if ret < 0 {
		return nil, fmt.Errorf("decode: convert to rgba: %s", C.GoString(C.decoder_error(ret)))
	}
	return &d.img, nil
}

// Bounds reports the decoded picture's rectangle.
func (i *Image) Bounds() image.Rectangle {
	return image.Rect(0, 0, i.Width, i.Height)
}

// Version reports the FFmpeg build the process is linked against, for the about
// screen and for diagnosing a mismatched DLL.
func Version() string {
	return C.GoString(C.av_version_info())
}
