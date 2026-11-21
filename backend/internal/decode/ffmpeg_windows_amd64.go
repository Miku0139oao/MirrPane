package decode

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/miku0139/airplay-go/backend/internal/ffmpeg"
	"golang.org/x/sys/windows"
)

// These signatures mirror FFmpeg's public C ABI. Native objects are allocated
// and freed by FFmpeg; no Go pointer is retained by the libraries. Windows x64
// uses one calling convention for both the DLL calls and the format callback.
type ffmpegAPI struct {
	codecVersion      func() uint32
	utilVersion       func() uint32
	scaleVersion      func() uint32
	resampleVersion   func() uint32
	versionInfo       func() string
	logSetLevel       func(int32)
	strerror          func(int32, *byte, uintptr) int32
	mallocz           func(uintptr) *byte
	findDecoder       func(int32) uintptr
	allocContext      func(uintptr) *codecContext
	openCodec         func(*codecContext, uintptr, uintptr) int32
	freeContext       func(**codecContext)
	sendPacket        func(*codecContext, *avPacket) int32
	receiveFrame      func(*codecContext, *avFrame) int32
	frameAlloc        func() *avFrame
	frameFree         func(**avFrame)
	frameUnref        func(*avFrame)
	packetAlloc       func() *avPacket
	packetFree        func(**avPacket)
	newPacket         func(*avPacket, int32) int32
	packetUnref       func(*avPacket)
	parserInit        func(int32) uintptr
	parserClose       func(uintptr)
	parserParse       func(uintptr, *codecContext, **byte, *int32, *byte, int32, int64, int64, int64) int32
	getHWConfig       func(uintptr, int32) *hwConfig
	hwDeviceCreate    func(*uintptr, int32, uintptr, uintptr, int32) int32
	bufferRef         func(uintptr) uintptr
	bufferUnref       func(*uintptr)
	hwFrameTransfer   func(*avFrame, *avFrame, int32) int32
	pixelDescriptor   func(int32) *pixelDescriptor
	scaleContext      func(uintptr, int32, int32, int32, int32, int32, int32, int32, uintptr, uintptr, uintptr) uintptr
	scaleFree         func(uintptr)
	scale             func(uintptr, **byte, *int32, int32, int32, **byte, *int32) int32
	scaleCoefficients func(int32) *int32
	scaleColorspace   func(uintptr, *int32, int32, *int32, int32, int32, int32, int32) int32
	layoutDefault     func(*channelLayout, int32)
	layoutUninit      func(*channelLayout)
	resampleAlloc     func(*uintptr, *channelLayout, int32, int32, *channelLayout, int32, int32, int32, uintptr) int32
	resampleInit      func(uintptr) int32
	resampleFree      func(*uintptr)
	resampleConvert   func(uintptr, **byte, int32, **byte, int32) int32
	formatCallback    uintptr
}

var native struct {
	once      sync.Once
	api       *ffmpegAPI
	err       error
	directory string
}

func loadFFmpeg() (*ffmpegAPI, error) {
	native.once.Do(func() {
		native.directory, native.err = ffmpeg.Find()
		if native.err == nil {
			native.api, native.err = openFFmpeg(native.directory)
		}
	})
	return native.api, native.err
}

// PrepareRuntime resolves and validates the native ABI before decoder objects
// are created. Automatic discovery can replace an incompatible installation;
// an explicit MIRRPANE_FFMPEG_DIR remains authoritative. Tests that construct a
// decoder directly never download anything implicitly.
func PrepareRuntime(ctx context.Context, cache string, progress func(string)) (string, error) {
	native.once.Do(func() {
		native.directory, native.err = ffmpeg.Ensure(ctx, cache, progress, func(dir string) error {
			var err error
			native.api, err = openFFmpeg(dir)
			return err
		})
	})
	return native.directory, native.err
}

func checkVersion(name string, actual, minimum uint32) error {
	if actual>>16 != minimum>>16 || actual < minimum {
		return fmt.Errorf("decode: incompatible %s version %d.%d.%d (requires ABI %d, at least %d.%d.%d)",
			name, actual>>16, (actual>>8)&255, actual&255,
			minimum>>16, minimum>>16, (minimum>>8)&255, minimum&255)
	}
	return nil
}

func openFFmpeg(dir string) (_ *ffmpegAPI, err error) {
	libs := make(map[string]windows.Handle)
	var handles []windows.Handle
	defer func() {
		if err != nil {
			for i := len(handles) - 1; i >= 0; i-- {
				_ = windows.FreeLibrary(handles[i])
			}
		}
	}()
	for _, name := range ffmpeg.DLLs {
		// Use absolute paths and restrict dependent DLL lookup to this runtime
		// and Windows itself. No process-wide DLL search path is changed.
		h, loadErr := windows.LoadLibraryEx(filepath.Join(dir, name), 0,
			windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
		if loadErr != nil {
			return nil, fmt.Errorf("decode: load %s: %w", name, loadErr)
		}
		libs[name] = h
		handles = append(handles, h)
	}
	f := &ffmpegAPI{}
	bind := func(dll, name string, target any) error {
		symbol, err := windows.GetProcAddress(libs[dll], name)
		if err != nil {
			return fmt.Errorf("decode: %s: %w", name, err)
		}
		purego.RegisterFunc(target, symbol)
		return nil
	}
	// Version checks run before any structure is read or written.
	for _, version := range []struct {
		dll, symbol string
		target      *func() uint32
		minimum     uint32
	}{
		{"avcodec-63.dll", "avcodec_version", &f.codecVersion, minCodecVersion},
		{"avutil-61.dll", "avutil_version", &f.utilVersion, minUtilVersion},
		{"swscale-10.dll", "swscale_version", &f.scaleVersion, minScaleVersion},
		{"swresample-7.dll", "swresample_version", &f.resampleVersion, minResampleVersion},
	} {
		if err := bind(version.dll, version.symbol, version.target); err != nil {
			return nil, err
		}
		if err := checkVersion(version.dll, (*version.target)(), version.minimum); err != nil {
			return nil, err
		}
	}
	for _, function := range []struct {
		dll, name string
		target    any
	}{
		{"avutil-61.dll", "av_version_info", &f.versionInfo},
		{"avutil-61.dll", "av_log_set_level", &f.logSetLevel},
		{"avutil-61.dll", "av_strerror", &f.strerror},
		{"avutil-61.dll", "av_mallocz", &f.mallocz},
		{"avcodec-63.dll", "avcodec_find_decoder", &f.findDecoder},
		{"avcodec-63.dll", "avcodec_alloc_context3", &f.allocContext},
		{"avcodec-63.dll", "avcodec_open2", &f.openCodec},
		{"avcodec-63.dll", "avcodec_free_context", &f.freeContext},
		{"avcodec-63.dll", "avcodec_send_packet", &f.sendPacket},
		{"avcodec-63.dll", "avcodec_receive_frame", &f.receiveFrame},
		{"avutil-61.dll", "av_frame_alloc", &f.frameAlloc},
		{"avutil-61.dll", "av_frame_free", &f.frameFree},
		{"avutil-61.dll", "av_frame_unref", &f.frameUnref},
		{"avcodec-63.dll", "av_packet_alloc", &f.packetAlloc},
		{"avcodec-63.dll", "av_packet_free", &f.packetFree},
		{"avcodec-63.dll", "av_new_packet", &f.newPacket},
		{"avcodec-63.dll", "av_packet_unref", &f.packetUnref},
		{"avcodec-63.dll", "av_parser_init", &f.parserInit},
		{"avcodec-63.dll", "av_parser_close", &f.parserClose},
		{"avcodec-63.dll", "av_parser_parse2", &f.parserParse},
		{"avcodec-63.dll", "avcodec_get_hw_config", &f.getHWConfig},
		{"avutil-61.dll", "av_hwdevice_ctx_create", &f.hwDeviceCreate},
		{"avutil-61.dll", "av_buffer_ref", &f.bufferRef},
		{"avutil-61.dll", "av_buffer_unref", &f.bufferUnref},
		{"avutil-61.dll", "av_hwframe_transfer_data", &f.hwFrameTransfer},
		{"avutil-61.dll", "av_pix_fmt_desc_get", &f.pixelDescriptor},
		{"swscale-10.dll", "sws_getCachedContext", &f.scaleContext},
		{"swscale-10.dll", "sws_freeContext", &f.scaleFree},
		{"swscale-10.dll", "sws_scale", &f.scale},
		{"swscale-10.dll", "sws_getCoefficients", &f.scaleCoefficients},
		{"swscale-10.dll", "sws_setColorspaceDetails", &f.scaleColorspace},
		{"avutil-61.dll", "av_channel_layout_default", &f.layoutDefault},
		{"avutil-61.dll", "av_channel_layout_uninit", &f.layoutUninit},
		{"swresample-7.dll", "swr_alloc_set_opts2", &f.resampleAlloc},
		{"swresample-7.dll", "swr_init", &f.resampleInit},
		{"swresample-7.dll", "swr_free", &f.resampleFree},
		{"swresample-7.dll", "swr_convert", &f.resampleConvert},
	} {
		if err := bind(function.dll, function.name, function.target); err != nil {
			return nil, err
		}
	}
	f.logSetLevel(logError)
	// One callback for the process, not one per connection: Windows callbacks
	// cannot be released. Opaque holds an integer pixel-format tag, never a Go
	// pointer, and the callback remains valid through decoder worker shutdown.
	f.formatCallback = purego.NewCallback(func(ctx *codecContext, formats *int32) uintptr {
		preferred := int32(ctx.Opaque) - 1
		fallback := int32(pixelNone)
		for i := 0; i < 256; i++ {
			format := *(*int32)(unsafe.Add(unsafe.Pointer(formats), i*4))
			if format == pixelNone {
				break
			}
			if format == preferred {
				return uintptr(uint32(format))
			}
			if descriptor := f.pixelDescriptor(format); descriptor != nil && descriptor.Flags&pixelFlagHardware == 0 && fallback == pixelNone {
				fallback = format
			}
		}
		return uintptr(uint32(fallback))
	})
	// Handles are intentionally retained until process exit; all decoder objects
	// and the one callback depend on these libraries remaining loaded.
	return f, nil
}

func (f *ffmpegAPI) error(operation string, code int32) error {
	var buffer [256]byte
	f.strerror(code, &buffer[0], uintptr(len(buffer)))
	n := 0
	for n < len(buffer) && buffer[n] != 0 {
		n++
	}
	return fmt.Errorf("decode: %s: %s (%d)", operation, buffer[:n], code)
}

func (f *ffmpegAPI) packet(packet *avPacket, data []byte) error {
	if len(data) > 16<<20 {
		return fmt.Errorf("decode: packet exceeds 16 MiB")
	}
	if ret := f.newPacket(packet, int32(len(data))); ret < 0 {
		return f.error("allocate packet", ret)
	}
	copy(unsafe.Slice(packet.Data, len(data)), data)
	packet.PTS = noPTS
	return nil
}
