// Package decode D3D11 VideoProcessor zero-copy surface pool (Windows amd64, CGO_ENABLED=0).
// All COM vtable indices and struct layouts match Windows 10 SDK (10.0.26100.0) d3d11.h, d3d11_1.h, d3d10.h, dxgi.h.
package decode

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const gpuSlotCount = 3

// DXGI / D3D11 constants from dxgiformat.h, dxgicommon.h, d3d11.h.
const (
	dxgiFormatB8G8R8A8Unorm uint32 = 87
	dxgiFormatNV12          uint32 = 103

	dxgiColorSpaceRGBFullG22NoneP709      uint32 = 0
	dxgiColorSpaceRGBFullG22NoneP2020     uint32 = 17
	dxgiColorSpaceYCbCrStudioG22LeftP601  uint32 = 6
	dxgiColorSpaceYCbCrFullG22LeftP601    uint32 = 7
	dxgiColorSpaceYCbCrStudioG22LeftP709  uint32 = 8
	dxgiColorSpaceYCbCrFullG22LeftP709    uint32 = 9
	dxgiColorSpaceYCbCrStudioG22LeftP2020 uint32 = 10
	dxgiColorSpaceYCbCrFullG22LeftP2020   uint32 = 11

	d3d11UsageDefault uint32 = 0
	d3d11UsageStaging uint32 = 3

	d3d11BindShaderResource uint32 = 0x8
	d3d11BindRenderTarget   uint32 = 0x20

	d3d11CPUAccessRead uint32 = 0x20000

	d3d11ResourceMiscShared uint32 = 0x2

	d3d11MapRead uint32 = 1

	d3d11QueryEvent uint32 = 0

	d3d11VideoFrameFormatProgressive uint32 = 0
	d3d11VideoUsageOptimalSpeed      uint32 = 1

	d3d11VideoProcessorFormatSupportInput  uint32 = 0x1
	d3d11VideoProcessorFormatSupportOutput uint32 = 0x2

	d3d11VideoProcessorNominalRange16_235 uint32 = 1
	d3d11VideoProcessorNominalRange0_255  uint32 = 2
	d3d11VideoProcessorAlphaFillOpaque    uint32 = 0

	d3d11VPIVDimensionTexture2D uint32 = 1
	d3d11VPOVDimensionTexture2D uint32 = 1

	hrSOK    uintptr = 0
	hrSFalse uintptr = 1
)

// Verified COM vtable method indices (Windows 10 SDK 10.0.26100.0).
const (
	// IUnknown
	vtblQueryInterface = 0
	vtblAddRef         = 1
	vtblRelease        = 2

	// ID3D11DeviceChild / ID3D11Texture2D
	vtblDeviceChildGetDevice = 3
	vtblTexture2DGetDesc     = 10

	// ID3D11Device
	vtblDeviceCreateTexture2D     = 5
	vtblDeviceCreateQuery         = 24
	vtblDeviceOpenSharedResource  = 28
	vtblDeviceGetRemovedReason    = 39
	vtblDeviceGetImmediateContext = 40

	// ID3D11DeviceContext
	vtblContextMap          = 14
	vtblContextUnmap        = 15
	vtblContextEnd          = 28
	vtblContextGetData      = 29
	vtblContextCopyResource = 47
	vtblContextFlush        = 111

	// ID3D10Multithread
	vtblMultithreadEnter        = 3
	vtblMultithreadLeave        = 4
	vtblMultithreadSetProtected = 5

	// ID3D11VideoDevice (inherits IUnknown directly)
	vtblVideoDeviceCreateVideoProcessor           = 4
	vtblVideoDeviceCreateVideoProcessorInputView  = 8
	vtblVideoDeviceCreateVideoProcessorOutputView = 9
	vtblVideoDeviceCreateVideoProcessorEnumerator = 10

	// ID3D11VideoProcessorEnumerator (inherits ID3D11DeviceChild)
	vtblVPEnumCheckFormat = 8

	// ID3D11VideoProcessorEnumerator1 (inherits ID3D11VideoProcessorEnumerator)
	vtblVPEnum1CheckConversion = 13

	// ID3D11VideoContext (inherits ID3D11DeviceChild)
	vtblVideoContextSetOutputTargetRect     = 13
	vtblVideoContextSetOutputColorSpace     = 15
	vtblVideoContextSetOutputAlphaFillMode  = 16
	vtblVideoContextSetStreamFrameFormat    = 27
	vtblVideoContextSetStreamColorSpace     = 28
	vtblVideoContextSetStreamSourceRect     = 30
	vtblVideoContextSetStreamDestRect       = 31
	vtblVideoContextSetStreamAutoProcessing = 37
	vtblVideoContextVideoProcessorBlt       = 53

	// ID3D11VideoContext1 (inherits ID3D11VideoContext)
	vtblVideoContext1SetOutputColorSpace1 = 70
	vtblVideoContext1SetOutputShaderUsage = 71
	vtblVideoContext1SetStreamColorSpace1 = 74

	// DXGI interfaces
	vtblDXGIDeviceGetAdapter        = 7
	vtblDXGIAdapterGetDesc          = 8
	vtblDXGIResourceGetSharedHandle = 8
)

var (
	iidID3D11Texture2D     = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
	iidID3D10Multithread   = windows.GUID{Data1: 0x9b7e4e00, Data2: 0x342c, Data3: 0x4106, Data4: [8]byte{0xa1, 0x9f, 0x4f, 0x27, 0x04, 0xf6, 0x89, 0xf0}}
	iidID3D11VideoDevice   = windows.GUID{Data1: 0x10ec4d5b, Data2: 0x975a, Data3: 0x4689, Data4: [8]byte{0xb9, 0xe4, 0xd0, 0xaa, 0xc3, 0x0f, 0xe3, 0x33}}
	iidID3D11VideoContext  = windows.GUID{Data1: 0x61f21c45, Data2: 0x3c0e, Data3: 0x4a74, Data4: [8]byte{0x9c, 0xea, 0x67, 0x10, 0x0d, 0x9a, 0xd5, 0xe4}}
	iidID3D11VideoContext1 = windows.GUID{Data1: 0xa7f026da, Data2: 0xa5f8, Data3: 0x4487, Data4: [8]byte{0xa5, 0x64, 0x15, 0xe3, 0x43, 0x57, 0x65, 0x1e}}
	iidID3D11VPEnum1       = windows.GUID{Data1: 0x465217f2, Data2: 0x5568, Data3: 0x43cf, Data4: [8]byte{0xb5, 0xb9, 0xf6, 0x1d, 0x54, 0x53, 0x1c, 0xa1}}
	iidIDXGIResource       = windows.GUID{Data1: 0x035f3ab4, Data2: 0x482e, Data3: 0x4e50, Data4: [8]byte{0xb4, 0x1f, 0x8a, 0x7f, 0x8b, 0xd8, 0x96, 0x0b}}
	iidIDXGIDevice         = windows.GUID{Data1: 0x54ec77fa, Data2: 0x1377, Data3: 0x44e6, Data4: [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
)

// C ABI struct layouts verified against Windows 10 SDK (x64).
type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

var _ [44 - unsafe.Sizeof(d3d11Texture2DDesc{})]byte
var _ [unsafe.Sizeof(d3d11Texture2DDesc{}) - 44]byte

type d3d11MappedSubresource struct {
	PData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

var _ [16 - unsafe.Sizeof(d3d11MappedSubresource{})]byte
var _ [unsafe.Sizeof(d3d11MappedSubresource{}) - 16]byte

type d3d11QueryDesc struct {
	Query     uint32
	MiscFlags uint32
}

var _ [8 - unsafe.Sizeof(d3d11QueryDesc{})]byte
var _ [unsafe.Sizeof(d3d11QueryDesc{}) - 8]byte

type d3d11VideoProcessorContentDesc struct {
	InputFrameFormat      uint32
	InputRateNumerator    uint32
	InputRateDenominator  uint32
	InputWidth            uint32
	InputHeight           uint32
	OutputRateNumerator   uint32
	OutputRateDenominator uint32
	OutputWidth           uint32
	OutputHeight          uint32
	Usage                 uint32
}

var _ [40 - unsafe.Sizeof(d3d11VideoProcessorContentDesc{})]byte
var _ [unsafe.Sizeof(d3d11VideoProcessorContentDesc{}) - 40]byte

type d3d11VideoProcessorInputViewDesc struct {
	FourCC        uint32
	ViewDimension uint32
	MipSlice      uint32
	ArraySlice    uint32
}

var _ [16 - unsafe.Sizeof(d3d11VideoProcessorInputViewDesc{})]byte
var _ [unsafe.Sizeof(d3d11VideoProcessorInputViewDesc{}) - 16]byte

type d3d11VideoProcessorOutputViewDesc struct {
	ViewDimension uint32
	MipSlice      uint32
	_             [2]uint32 // union with D3D11_TEX2D_ARRAY_VPOV (3 UINTs)
}

var _ [16 - unsafe.Sizeof(d3d11VideoProcessorOutputViewDesc{})]byte
var _ [unsafe.Sizeof(d3d11VideoProcessorOutputViewDesc{}) - 16]byte

type d3d11VideoProcessorStream struct {
	Enable                int32
	OutputIndex           uint32
	InputFrameOrField     uint32
	PastFrames            uint32
	FutureFrames          uint32
	PPPastSurfaces        uintptr
	PInputSurface         uintptr
	PPFutureSurfaces      uintptr
	PPPastSurfacesRight   uintptr
	PInputSurfaceRight    uintptr
	PPFutureSurfacesRight uintptr
}

var _ [72 - unsafe.Sizeof(d3d11VideoProcessorStream{})]byte
var _ [unsafe.Sizeof(d3d11VideoProcessorStream{}) - 72]byte
var _ [32 - unsafe.Offsetof(d3d11VideoProcessorStream{}.PInputSurface)]byte
var _ [unsafe.Offsetof(d3d11VideoProcessorStream{}.PInputSurface) - 32]byte

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

var _ [16 - unsafe.Sizeof(winRect{})]byte
var _ [unsafe.Sizeof(winRect{}) - 16]byte

type dxgiAdapterDesc struct {
	Description           [128]uint16
	VendorId              uint32
	DeviceId              uint32
	SubSysId              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuidLow        uint32
	AdapterLuidHigh       int32
}

var _ [304 - unsafe.Sizeof(dxgiAdapterDesc{})]byte
var _ [unsafe.Sizeof(dxgiAdapterDesc{}) - 304]byte
var _ [296 - unsafe.Offsetof(dxgiAdapterDesc{}.AdapterLuidLow)]byte
var _ [unsafe.Offsetof(dxgiAdapterDesc{}.AdapterLuidLow) - 296]byte

// comObject is a native C++/COM interface pointer whose first word points at its vtable.
type comObject struct {
	vtbl *uintptr
}

func (o *comObject) method(index uintptr) uintptr {
	return *(*uintptr)(unsafe.Add(unsafe.Pointer(o.vtbl), index*unsafe.Sizeof(uintptr(0))))
}

func (o *comObject) addRef() {
	if o == nil {
		return
	}
	_, _, _ = syscall.SyscallN(o.method(vtblAddRef), uintptr(unsafe.Pointer(o)))
}

func (o *comObject) release() {
	if o == nil {
		return
	}
	_, _, _ = syscall.SyscallN(o.method(vtblRelease), uintptr(unsafe.Pointer(o)))
}

func (o *comObject) queryInterface(iid *windows.GUID) (*comObject, error) {
	if o == nil {
		return nil, errors.New("d3d11: nil COM object")
	}
	var out *comObject
	r1, _, _ := syscall.SyscallN(
		o.method(vtblQueryInterface),
		uintptr(unsafe.Pointer(o)),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&out)),
	)
	if hrFailed(r1) || out == nil {
		return nil, hresultErr("QueryInterface", r1)
	}
	return out, nil
}

func hrFailed(hr uintptr) bool {
	return int32(uint32(hr)) < 0
}

func hresultErr(op string, hr uintptr) error {
	return fmt.Errorf("decode: d3d11 %s failed (HRESULT 0x%08x)", op, uint32(hr))
}

// GPUSurface describes a rendered DXGI shared B8G8R8A8_UNORM slot surface.
type GPUSurface struct {
	SharedHandle uintptr
	AdapterLUID  uint64
	Width        int
	Height       int
	Timings      DecodeTimings
}

type d3d11Picture struct {
	texture           *comObject
	arraySlice        uintptr
	width, height     int32
	matrix, fullRange int32
}

type d3d11SlotSurface struct {
	tex          *comObject
	device       *comObject // borrowed: the texture keeps its creating device alive
	sharedHandle uintptr
	width        uint32
	height       uint32
}

// d3d11Pool owns the VideoProcessor, completion query, last published surface
// per logical slot, and on-demand staging surface for PNG snapshots.
type d3d11Pool struct {
	device        *comObject
	context       *comObject
	multithread   *comObject
	videoDevice   *comObject
	videoContext  *comObject
	videoContext1 *comObject
	query         *comObject
	adapterLUID   uint64

	vpEnum      *comObject
	vp          *comObject
	inTexW      uint32
	inTexH      uint32
	inputFormat uint32
	outW        uint32
	outH        uint32
	colorMatrix int32
	fullRange   int32
	configured  bool

	slots    [gpuSlotCount]d3d11SlotSurface
	lastSlot int

	staging  *comObject
	stagingW uint32
	stagingH uint32
}

func newD3D11Pool() *d3d11Pool {
	return &d3d11Pool{lastSlot: -1}
}

func (p *d3d11Pool) lockContext() {
	if p.multithread != nil {
		// The native critical section is owned by an OS thread. Keep Enter
		// and Leave on the same thread even if Go preempts the goroutine.
		runtime.LockOSThread()
		_, _, _ = syscall.SyscallN(p.multithread.method(vtblMultithreadEnter), uintptr(unsafe.Pointer(p.multithread)))
	}
}

func (p *d3d11Pool) unlockContext() {
	if p.multithread != nil {
		_, _, _ = syscall.SyscallN(p.multithread.method(vtblMultithreadLeave), uintptr(unsafe.Pointer(p.multithread)))
		runtime.UnlockOSThread()
	}
}

func (p *d3d11Pool) ensureDevice(dev *comObject) (err error) {
	if p.device == dev && p.videoDevice != nil && p.videoContext != nil && p.query != nil {
		return nil
	}
	p.closeDevice()
	defer func() {
		if err != nil {
			p.closeDevice()
		}
	}()

	dev.addRef()
	p.device = dev

	var ctx *comObject
	_, _, _ = syscall.SyscallN(
		dev.method(vtblDeviceGetImmediateContext),
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(&ctx)),
	)
	if ctx == nil {
		return errors.New("decode: d3d11 GetImmediateContext returned nil")
	}
	p.context = ctx

	if mt, err := ctx.queryInterface(&iidID3D10Multithread); err == nil {
		p.multithread = mt
		_, _, _ = syscall.SyscallN(mt.method(vtblMultithreadSetProtected), uintptr(unsafe.Pointer(mt)), 1)
	} else if mt, err := dev.queryInterface(&iidID3D10Multithread); err == nil {
		p.multithread = mt
		_, _, _ = syscall.SyscallN(mt.method(vtblMultithreadSetProtected), uintptr(unsafe.Pointer(mt)), 1)
	}

	vdev, err := dev.queryInterface(&iidID3D11VideoDevice)
	if err != nil {
		return fmt.Errorf("query ID3D11VideoDevice: %w", err)
	}
	p.videoDevice = vdev

	vctx, err := ctx.queryInterface(&iidID3D11VideoContext)
	if err != nil {
		return fmt.Errorf("query ID3D11VideoContext: %w", err)
	}
	p.videoContext = vctx

	if vctx1, err := ctx.queryInterface(&iidID3D11VideoContext1); err == nil {
		p.videoContext1 = vctx1
	}

	qdesc := d3d11QueryDesc{Query: d3d11QueryEvent}
	var query *comObject
	r1, _, _ := syscall.SyscallN(
		dev.method(vtblDeviceCreateQuery),
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(&qdesc)),
		uintptr(unsafe.Pointer(&query)),
	)
	if hrFailed(r1) || query == nil {
		return hresultErr("CreateQuery(EVENT)", r1)
	}
	p.query = query

	luid, err := queryAdapterLUID(dev)
	if err != nil {
		return err
	}
	p.adapterLUID = luid
	return nil
}

func queryAdapterLUID(dev *comObject) (uint64, error) {
	dxgiDev, err := dev.queryInterface(&iidIDXGIDevice)
	if err != nil {
		return 0, fmt.Errorf("query IDXGIDevice: %w", err)
	}
	defer dxgiDev.release()

	var adapter *comObject
	r1, _, _ := syscall.SyscallN(
		dxgiDev.method(vtblDXGIDeviceGetAdapter),
		uintptr(unsafe.Pointer(dxgiDev)),
		uintptr(unsafe.Pointer(&adapter)),
	)
	if hrFailed(r1) || adapter == nil {
		return 0, hresultErr("IDXGIDevice::GetAdapter", r1)
	}
	defer adapter.release()

	var desc dxgiAdapterDesc
	r1, _, _ = syscall.SyscallN(
		adapter.method(vtblDXGIAdapterGetDesc),
		uintptr(unsafe.Pointer(adapter)),
		uintptr(unsafe.Pointer(&desc)),
	)
	if hrFailed(r1) {
		return 0, hresultErr("IDXGIAdapter::GetDesc", r1)
	}
	return uint64(desc.AdapterLuidLow) | (uint64(uint32(desc.AdapterLuidHigh)) << 32), nil
}

func (p *d3d11Pool) ensureVideoProcessor(inTexW, inTexH, outW, outH, srcFormat uint32) error {
	if p.vpEnum != nil && p.vp != nil &&
		p.inTexW == inTexW && p.inTexH == inTexH &&
		p.inputFormat == srcFormat &&
		p.outW == outW && p.outH == outH {
		return nil
	}
	p.configured = false
	if p.vp != nil {
		p.vp.release()
		p.vp = nil
	}
	if p.vpEnum != nil {
		p.vpEnum.release()
		p.vpEnum = nil
	}

	cdesc := d3d11VideoProcessorContentDesc{
		InputFrameFormat:      d3d11VideoFrameFormatProgressive,
		InputRateNumerator:    60,
		InputRateDenominator:  1,
		InputWidth:            inTexW,
		InputHeight:           inTexH,
		OutputRateNumerator:   60,
		OutputRateDenominator: 1,
		OutputWidth:           outW,
		OutputHeight:          outH,
		Usage:                 d3d11VideoUsageOptimalSpeed,
	}
	var vpEnum *comObject
	r1, _, _ := syscall.SyscallN(
		p.videoDevice.method(vtblVideoDeviceCreateVideoProcessorEnumerator),
		uintptr(unsafe.Pointer(p.videoDevice)),
		uintptr(unsafe.Pointer(&cdesc)),
		uintptr(unsafe.Pointer(&vpEnum)),
	)
	if hrFailed(r1) || vpEnum == nil {
		return hresultErr("CreateVideoProcessorEnumerator", r1)
	}

	var inSupport, outSupport uint32
	rIn, _, _ := syscall.SyscallN(
		vpEnum.method(vtblVPEnumCheckFormat),
		uintptr(unsafe.Pointer(vpEnum)),
		uintptr(srcFormat),
		uintptr(unsafe.Pointer(&inSupport)),
	)
	rOut, _, _ := syscall.SyscallN(
		vpEnum.method(vtblVPEnumCheckFormat),
		uintptr(unsafe.Pointer(vpEnum)),
		uintptr(dxgiFormatB8G8R8A8Unorm),
		uintptr(unsafe.Pointer(&outSupport)),
	)
	if hrFailed(rIn) || inSupport&d3d11VideoProcessorFormatSupportInput == 0 {
		vpEnum.release()
		return fmt.Errorf("decode: d3d11 VideoProcessor does not support input DXGI_FORMAT %d", srcFormat)
	}
	if hrFailed(rOut) || outSupport&d3d11VideoProcessorFormatSupportOutput == 0 {
		vpEnum.release()
		return errors.New("decode: d3d11 VideoProcessor does not support BGRA8 output")
	}

	var vp *comObject
	r1, _, _ = syscall.SyscallN(
		p.videoDevice.method(vtblVideoDeviceCreateVideoProcessor),
		uintptr(unsafe.Pointer(p.videoDevice)),
		uintptr(unsafe.Pointer(vpEnum)),
		0,
		uintptr(unsafe.Pointer(&vp)),
	)
	if hrFailed(r1) || vp == nil {
		vpEnum.release()
		return hresultErr("CreateVideoProcessor", r1)
	}

	p.vpEnum = vpEnum
	p.vp = vp
	p.inTexW = inTexW
	p.inTexH = inTexH
	p.inputFormat = srcFormat
	p.outW = outW
	p.outH = outH
	return nil
}

func (p *d3d11Pool) newSharedSurface(outW, outH uint32) (d3d11SlotSurface, error) {
	desc := d3d11Texture2DDesc{
		Width:       outW,
		Height:      outH,
		MipLevels:   1,
		ArraySize:   1,
		Format:      dxgiFormatB8G8R8A8Unorm,
		SampleCount: 1,
		Usage:       d3d11UsageDefault,
		BindFlags:   d3d11BindRenderTarget | d3d11BindShaderResource,
		MiscFlags:   d3d11ResourceMiscShared,
	}
	var tex *comObject
	r1, _, _ := syscall.SyscallN(
		p.device.method(vtblDeviceCreateTexture2D),
		uintptr(unsafe.Pointer(p.device)),
		uintptr(unsafe.Pointer(&desc)),
		0,
		uintptr(unsafe.Pointer(&tex)),
	)
	if hrFailed(r1) || tex == nil {
		return d3d11SlotSurface{}, hresultErr("CreateTexture2D(shared BGRA8)", r1)
	}
	dxgiRes, err := tex.queryInterface(&iidIDXGIResource)
	if err != nil {
		tex.release()
		return d3d11SlotSurface{}, fmt.Errorf("query IDXGIResource: %w", err)
	}
	var handle uintptr
	r1, _, _ = syscall.SyscallN(
		dxgiRes.method(vtblDXGIResourceGetSharedHandle),
		uintptr(unsafe.Pointer(dxgiRes)),
		uintptr(unsafe.Pointer(&handle)),
	)
	dxgiRes.release()
	if hrFailed(r1) || handle == 0 {
		tex.release()
		return d3d11SlotSurface{}, hresultErr("IDXGIResource::GetSharedHandle", r1)
	}
	return d3d11SlotSurface{tex: tex, device: p.device, sharedHandle: handle, width: outW, height: outH}, nil
}

func (p *d3d11Pool) configureVideoProcessor(pic d3d11Picture) error {
	if p.configured && pic.matrix == p.colorMatrix && pic.fullRange == p.fullRange {
		return nil
	}
	if err := p.configureColors(pic.matrix, pic.fullRange != 0); err != nil {
		return err
	}

	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetStreamFrameFormat),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		0,
		uintptr(d3d11VideoFrameFormatProgressive),
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetStreamAutoProcessing),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		0,
		0,
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetOutputAlphaFillMode),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		uintptr(d3d11VideoProcessorAlphaFillOpaque),
		0,
	)

	// FFmpeg applies right/bottom cropping to the reported width and height
	// for hardware frames. The underlying texture can still include padding.
	rect := winRect{Right: pic.width, Bottom: pic.height}
	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetStreamSourceRect),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		0,
		1,
		uintptr(unsafe.Pointer(&rect)),
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetStreamDestRect),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		0,
		1,
		uintptr(unsafe.Pointer(&rect)),
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextSetOutputTargetRect),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		1,
		uintptr(unsafe.Pointer(&rect)),
	)
	p.colorMatrix = pic.matrix
	p.fullRange = pic.fullRange
	p.configured = true
	return nil
}

func (p *d3d11Pool) configureColors(matrix int32, full bool) error {
	if p.videoContext1 == nil {
		if matrix == matrixBT2020 {
			return errors.New("decode: d3d11 legacy VideoProcessor cannot convert BT.2020")
		}
		// D3D11_VIDEO_PROCESSOR_COLOR_SPACE packs the matrix and range into bits 2 and 4..5.
		outCS := d3d11VideoProcessorNominalRange0_255 << 4
		inCS := d3d11VideoProcessorNominalRange16_235 << 4
		if full {
			inCS = d3d11VideoProcessorNominalRange0_255 << 4
		}
		if matrix == matrixBT709 {
			inCS |= 1 << 2
		}
		_, _, _ = syscall.SyscallN(
			p.videoContext.method(vtblVideoContextSetOutputColorSpace),
			uintptr(unsafe.Pointer(p.videoContext)),
			uintptr(unsafe.Pointer(p.vp)),
			uintptr(unsafe.Pointer(&outCS)),
		)
		_, _, _ = syscall.SyscallN(
			p.videoContext.method(vtblVideoContextSetStreamColorSpace),
			uintptr(unsafe.Pointer(p.videoContext)),
			uintptr(unsafe.Pointer(p.vp)),
			0,
			uintptr(unsafe.Pointer(&inCS)),
		)
		return nil
	}

	limited, expanded := dxgiColorSpaceYCbCrStudioG22LeftP601, dxgiColorSpaceYCbCrFullG22LeftP601
	outputCS := dxgiColorSpaceRGBFullG22NoneP709
	switch matrix {
	case matrixBT709:
		limited, expanded = dxgiColorSpaceYCbCrStudioG22LeftP709, dxgiColorSpaceYCbCrFullG22LeftP709
	case matrixBT2020:
		limited, expanded = dxgiColorSpaceYCbCrStudioG22LeftP2020, dxgiColorSpaceYCbCrFullG22LeftP2020
		// Match swscale's matrix conversion without an additional gamut transform.
		outputCS = dxgiColorSpaceRGBFullG22NoneP2020
	}
	inputCS := limited
	if full {
		inputCS = expanded
	}
	if enum1, err := p.vpEnum.queryInterface(&iidID3D11VPEnum1); err == nil {
		defer enum1.release()
		var supported int32
		hr, _, _ := syscall.SyscallN(
			enum1.method(vtblVPEnum1CheckConversion),
			uintptr(unsafe.Pointer(enum1)),
			uintptr(p.inputFormat),
			uintptr(inputCS),
			uintptr(dxgiFormatB8G8R8A8Unorm),
			uintptr(outputCS),
			uintptr(unsafe.Pointer(&supported)),
		)
		if hrFailed(hr) {
			return hresultErr("CheckVideoProcessorFormatConversion", hr)
		}
		if supported == 0 {
			return fmt.Errorf("decode: d3d11 VideoProcessor does not support color conversion %d -> %d", inputCS, outputCS)
		}
	}
	_, _, _ = syscall.SyscallN(
		p.videoContext1.method(vtblVideoContext1SetOutputColorSpace1),
		uintptr(unsafe.Pointer(p.videoContext1)),
		uintptr(unsafe.Pointer(p.vp)),
		uintptr(outputCS),
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext1.method(vtblVideoContext1SetOutputShaderUsage),
		uintptr(unsafe.Pointer(p.videoContext1)),
		uintptr(unsafe.Pointer(p.vp)),
		1,
	)
	_, _, _ = syscall.SyscallN(
		p.videoContext1.method(vtblVideoContext1SetStreamColorSpace1),
		uintptr(unsafe.Pointer(p.videoContext1)),
		uintptr(unsafe.Pointer(p.vp)),
		0,
		uintptr(inputCS),
	)
	return nil
}

func (p *d3d11Pool) renderFrame(pic d3d11Picture, slotIndex int) (GPUSurface, error) {
	if pic.texture == nil {
		return GPUSurface{}, errors.New("decode: missing D3D11 texture in hardware frame")
	}
	w, h := int(pic.width), int(pic.height)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return GPUSurface{}, fmt.Errorf("decode: invalid GPU frame size %dx%d", w, h)
	}

	convertStart := qpcNow()

	srcTex := pic.texture
	var srcDesc d3d11Texture2DDesc
	_, _, _ = syscall.SyscallN(
		srcTex.method(vtblTexture2DGetDesc),
		uintptr(unsafe.Pointer(srcTex)),
		uintptr(unsafe.Pointer(&srcDesc)),
	)
	if srcDesc.Width < uint32(w) || srcDesc.Height < uint32(h) {
		return GPUSurface{}, fmt.Errorf("decode: D3D11 texture %dx%d smaller than frame %dx%d",
			srcDesc.Width, srcDesc.Height, w, h)
	}
	if pic.arraySlice >= uintptr(srcDesc.ArraySize) {
		return GPUSurface{}, fmt.Errorf("decode: D3D11 array slice %d >= %d", pic.arraySlice, srcDesc.ArraySize)
	}

	var dev *comObject
	_, _, _ = syscall.SyscallN(
		srcTex.method(vtblDeviceChildGetDevice),
		uintptr(unsafe.Pointer(srcTex)),
		uintptr(unsafe.Pointer(&dev)),
	)
	if dev == nil {
		return GPUSurface{}, errors.New("decode: ID3D11Texture2D::GetDevice returned nil")
	}
	defer dev.release()

	if err := p.ensureDevice(dev); err != nil {
		return GPUSurface{}, err
	}

	p.lockContext()
	slot, err := p.renderLocked(pic, &srcDesc)
	p.unlockContext()
	if err != nil {
		return GPUSurface{}, err
	}

	if err := p.waitQueryCompletion(); err != nil {
		slot.tex.release()
		return GPUSurface{}, err
	}

	// A slot claim protects the reader's acquisition of a resource, but
	// Flutter may keep sampling it after it releases the claim. Replace the
	// slot's texture so a later frame cannot alter an in-flight draw.
	p.slots[slotIndex].tex.release()
	p.slots[slotIndex] = slot
	p.lastSlot = slotIndex
	return GPUSurface{
		SharedHandle: slot.sharedHandle,
		AdapterLUID:  p.adapterLUID,
		Width:        w,
		Height:       h,
		Timings: DecodeTimings{
			TransferNanos: 0,
			ConvertNanos:  qpcElapsedNanos(convertStart),
		},
	}, nil
}

func (p *d3d11Pool) renderLocked(pic d3d11Picture, srcDesc *d3d11Texture2DDesc) (_ d3d11SlotSurface, err error) {
	outW, outH := uint32(pic.width), uint32(pic.height)
	if err := p.ensureVideoProcessor(srcDesc.Width, srcDesc.Height, outW, outH, srcDesc.Format); err != nil {
		return d3d11SlotSurface{}, err
	}
	if err := p.configureVideoProcessor(pic); err != nil {
		return d3d11SlotSurface{}, err
	}

	ivDesc := d3d11VideoProcessorInputViewDesc{
		FourCC:        0,
		ViewDimension: d3d11VPIVDimensionTexture2D,
		MipSlice:      0,
		ArraySlice:    uint32(pic.arraySlice),
	}
	var inView *comObject
	r1, _, _ := syscall.SyscallN(
		p.videoDevice.method(vtblVideoDeviceCreateVideoProcessorInputView),
		uintptr(unsafe.Pointer(p.videoDevice)),
		uintptr(unsafe.Pointer(pic.texture)),
		uintptr(unsafe.Pointer(p.vpEnum)),
		uintptr(unsafe.Pointer(&ivDesc)),
		uintptr(unsafe.Pointer(&inView)),
	)
	if hrFailed(r1) || inView == nil {
		return d3d11SlotSurface{}, hresultErr("CreateVideoProcessorInputView", r1)
	}
	defer inView.release()

	slot, err := p.newSharedSurface(outW, outH)
	if err != nil {
		return d3d11SlotSurface{}, err
	}
	defer func() {
		if err != nil {
			slot.tex.release()
		}
	}()
	ovDesc := d3d11VideoProcessorOutputViewDesc{ViewDimension: d3d11VPOVDimensionTexture2D}
	var outView *comObject
	r1, _, _ = syscall.SyscallN(
		p.videoDevice.method(vtblVideoDeviceCreateVideoProcessorOutputView),
		uintptr(unsafe.Pointer(p.videoDevice)),
		uintptr(unsafe.Pointer(slot.tex)),
		uintptr(unsafe.Pointer(p.vpEnum)),
		uintptr(unsafe.Pointer(&ovDesc)),
		uintptr(unsafe.Pointer(&outView)),
	)
	if hrFailed(r1) || outView == nil {
		return d3d11SlotSurface{}, hresultErr("CreateVideoProcessorOutputView", r1)
	}
	defer outView.release()

	stream := d3d11VideoProcessorStream{
		Enable:        1,
		PInputSurface: uintptr(unsafe.Pointer(inView)),
	}
	r1, _, _ = syscall.SyscallN(
		p.videoContext.method(vtblVideoContextVideoProcessorBlt),
		uintptr(unsafe.Pointer(p.videoContext)),
		uintptr(unsafe.Pointer(p.vp)),
		uintptr(unsafe.Pointer(outView)),
		0,
		1,
		uintptr(unsafe.Pointer(&stream)),
	)
	runtime.KeepAlive(inView)
	if hrFailed(r1) {
		return d3d11SlotSurface{}, hresultErr("VideoProcessorBlt", r1)
	}

	// Signal GPU completion marker and flush the command buffer immediately so
	// Flutter's separate ANGLE D3D11 device never reads an incomplete surface.
	_, _, _ = syscall.SyscallN(
		p.context.method(vtblContextEnd),
		uintptr(unsafe.Pointer(p.context)),
		uintptr(unsafe.Pointer(p.query)),
	)
	_, _, _ = syscall.SyscallN(
		p.context.method(vtblContextFlush),
		uintptr(unsafe.Pointer(p.context)),
	)
	return slot, nil
}

func (p *d3d11Pool) waitQueryCompletion() error {
	const timeout = 50 * time.Millisecond
	start := time.Now()
	for iter := 0; ; iter++ {
		var done int32
		// Multithread protection covers each individual D3D call. Allow GetData
		// to flush again, which some drivers need before the event can complete.
		r1, _, _ := syscall.SyscallN(
			p.context.method(vtblContextGetData),
			uintptr(unsafe.Pointer(p.context)),
			uintptr(unsafe.Pointer(p.query)),
			uintptr(unsafe.Pointer(&done)),
			unsafe.Sizeof(done),
			0,
		)
		if r1 == hrSOK && done != 0 {
			return nil
		}
		if r1 != hrSOK && r1 != hrSFalse {
			return hresultErr("ID3D11DeviceContext::GetData(EVENT)", r1)
		}
		if time.Since(start) >= timeout {
			return errors.New("decode: d3d11 GPU sync query timed out")
		}
		if iter < 64 {
			runtime.Gosched()
		} else {
			time.Sleep(100 * time.Microsecond)
		}
	}
}

func (p *d3d11Pool) readbackSlotRGBA(slotIndex int) (Image, error) {
	if slotIndex < 0 {
		slotIndex = p.lastSlot
	}
	if slotIndex < 0 || slotIndex >= gpuSlotCount {
		return Image{}, errors.New("decode: no GPU slot available for readback")
	}
	slot := &p.slots[slotIndex]
	if slot.tex == nil || slot.width == 0 || slot.height == 0 {
		return Image{}, errors.New("decode: GPU slot surface is empty")
	}
	// A decoder rebuild can change devices while this slot is still shared.
	// A staging texture and its source must belong to the same device.
	if slot.device != nil && slot.device != p.device {
		reader := newD3D11Pool()
		defer reader.close()
		if err := reader.ensureDevice(slot.device); err != nil {
			return Image{}, err
		}
		return reader.readbackSurface(slot)
	}
	return p.readbackSurface(slot)
}

func (p *d3d11Pool) readbackSurface(slot *d3d11SlotSurface) (Image, error) {
	w, h := int(slot.width), int(slot.height)
	p.lockContext()
	defer p.unlockContext()

	if p.staging == nil || p.stagingW != slot.width || p.stagingH != slot.height {
		if p.staging != nil {
			p.staging.release()
			p.staging = nil
		}
		sdesc := d3d11Texture2DDesc{
			Width:          slot.width,
			Height:         slot.height,
			MipLevels:      1,
			ArraySize:      1,
			Format:         dxgiFormatB8G8R8A8Unorm,
			SampleCount:    1,
			Usage:          d3d11UsageStaging,
			CPUAccessFlags: d3d11CPUAccessRead,
		}
		var staging *comObject
		r1, _, _ := syscall.SyscallN(
			p.device.method(vtblDeviceCreateTexture2D),
			uintptr(unsafe.Pointer(p.device)),
			uintptr(unsafe.Pointer(&sdesc)),
			0,
			uintptr(unsafe.Pointer(&staging)),
		)
		if hrFailed(r1) || staging == nil {
			return Image{}, hresultErr("CreateTexture2D(staging)", r1)
		}
		p.staging = staging
		p.stagingW = slot.width
		p.stagingH = slot.height
	}

	_, _, _ = syscall.SyscallN(
		p.context.method(vtblContextCopyResource),
		uintptr(unsafe.Pointer(p.context)),
		uintptr(unsafe.Pointer(p.staging)),
		uintptr(unsafe.Pointer(slot.tex)),
	)

	var mapped d3d11MappedSubresource
	r1, _, _ := syscall.SyscallN(
		p.context.method(vtblContextMap),
		uintptr(unsafe.Pointer(p.context)),
		uintptr(unsafe.Pointer(p.staging)),
		0,
		uintptr(d3d11MapRead),
		0,
		uintptr(unsafe.Pointer(&mapped)),
	)
	if hrFailed(r1) || mapped.PData == nil {
		return Image{}, hresultErr("ID3D11DeviceContext::Map(staging)", r1)
	}
	defer syscall.SyscallN(
		p.context.method(vtblContextUnmap),
		uintptr(unsafe.Pointer(p.context)),
		uintptr(unsafe.Pointer(p.staging)),
		0,
	)

	rowBytes := w * 4
	if int(mapped.RowPitch) < rowBytes {
		return Image{}, fmt.Errorf("decode: mapped pitch %d < %d", mapped.RowPitch, rowBytes)
	}
	rgba := make([]byte, rowBytes*h)
	for y := 0; y < h; y++ {
		srcPtr := unsafe.Add(mapped.PData, uintptr(y)*uintptr(mapped.RowPitch))
		srcRow := unsafe.Slice((*byte)(srcPtr), rowBytes)
		dstRow := rgba[y*rowBytes : (y+1)*rowBytes]
		for x := 0; x < rowBytes; x += 4 {
			dstRow[x+0] = srcRow[x+2] // R <- B_G_R_A[2]
			dstRow[x+1] = srcRow[x+1] // G
			dstRow[x+2] = srcRow[x+0] // B <- B_G_R_A[0]
			dstRow[x+3] = 0xff
		}
	}
	return Image{
		Width:  w,
		Height: h,
		Stride: rowBytes,
		Pixels: rgba,
	}, nil
}

func (p *d3d11Pool) close() {
	if p == nil {
		return
	}
	for i := range p.slots {
		p.slots[i].tex.release()
		p.slots[i] = d3d11SlotSurface{}
	}
	p.lastSlot = -1
	p.closeDevice()
}

// Each shared surface belongs to its mailbox slot until a writer reclaims
// that slot. Replacing the decoder device must leave those handles valid.
func (p *d3d11Pool) closeDevice() {
	if p.staging != nil {
		p.staging.release()
		p.staging = nil
	}
	p.stagingW = 0
	p.stagingH = 0
	if p.vp != nil {
		p.vp.release()
		p.vp = nil
	}
	if p.vpEnum != nil {
		p.vpEnum.release()
		p.vpEnum = nil
	}
	p.inTexW = 0
	p.inTexH = 0
	p.inputFormat = 0
	p.outW = 0
	p.outH = 0
	p.configured = false
	if p.query != nil {
		p.query.release()
		p.query = nil
	}
	if p.videoContext1 != nil {
		p.videoContext1.release()
		p.videoContext1 = nil
	}
	if p.videoContext != nil {
		p.videoContext.release()
		p.videoContext = nil
	}
	if p.videoDevice != nil {
		p.videoDevice.release()
		p.videoDevice = nil
	}
	if p.multithread != nil {
		p.multithread.release()
		p.multithread = nil
	}
	if p.context != nil {
		p.context.release()
		p.context = nil
	}
	if p.device != nil {
		p.device.release()
		p.device = nil
	}
	p.adapterLUID = 0
}
