package decode

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

var (
	d3d11DLL              = syscall.NewLazyDLL("d3d11.dll")
	procD3D11CreateDevice = d3d11DLL.NewProc("D3D11CreateDevice")
)

func createTestD3D11Device(t testing.TB) (*comObject, *comObject) {
	t.Helper()
	const (
		driverTypeHardware      = 1
		createDeviceBGRASupport = 0x20
		d3d11SDKVersion         = 7
	)
	var dev, ctx *comObject
	var featureLevel uint32
	r1, _, _ := procD3D11CreateDevice.Call(
		0,
		driverTypeHardware,
		0,
		createDeviceBGRASupport,
		0,
		0,
		d3d11SDKVersion,
		uintptr(unsafe.Pointer(&dev)),
		uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&ctx)),
	)
	if hrFailed(r1) || dev == nil || ctx == nil {
		t.Skipf("hardware D3D11 device unavailable (HRESULT 0x%08x)", uint32(r1))
	}
	return dev, ctx
}

func openSharedTestTexture(t testing.TB, d *comObject, handle uintptr) *comObject {
	t.Helper()
	var texture *comObject
	hr, _, _ := syscall.SyscallN(
		d.method(vtblDeviceOpenSharedResource),
		uintptr(unsafe.Pointer(d)),
		handle,
		uintptr(unsafe.Pointer(&iidID3D11Texture2D)),
		uintptr(unsafe.Pointer(&texture)),
	)
	if hrFailed(hr) || texture == nil {
		t.Fatalf("consumer OpenSharedResource(0x%x) failed: HRESULT 0x%08x", handle, uint32(hr))
	}
	return texture
}

// TestDecoderD3D11ZeroCopyAndCrossDeviceImport exercises the full real-GPU
// D3D11VA -> ID3D11VideoProcessor -> DXGI shared handle -> cross-device
// OpenSharedResource (simulating Flutter ANGLE) + ReadbackSlotGPU path.
func TestDecoderD3D11ZeroCopyAndCrossDeviceImport(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := accessUnits(stream)
	if len(units) == 0 {
		t.Fatal("no access units in sample.h264")
	}

	d, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer d.Close()

	swDec, err := NewDecoderWithOptions(Options{PreferHardware: false, ColorRange: "auto", ScaleFilter: "fast"})
	if err != nil {
		t.Fatalf("NewDecoderWithOptions(software): %v", err)
	}
	defer swDec.Close()

	consumerDev, consumerCtx := createTestD3D11Device(t)
	defer consumerCtx.release()
	defer consumerDev.release()

	consumerLUID, err := queryAdapterLUID(consumerDev)
	if err != nil {
		t.Fatalf("queryAdapterLUID(consumer): %v", err)
	}

	var (
		rendered     int
		totalConvert int64
		coldConvert  int64
		minConvert   int64 = 1 << 62
		maxConvert   int64
	)

	for _, u := range units {
		swImg, swErr := swDec.DecodeAccessUnit(u)
		if swErr != nil && !errors.Is(swErr, ErrNoFrame) {
			t.Fatalf("software DecodeAccessUnit: %v", swErr)
		}

		err := d.ReceiveAccessUnit(u, true)
		if errors.Is(err, ErrNoFrame) {
			continue
		}
		if err != nil {
			t.Fatalf("ReceiveAccessUnit: %v", err)
		}
		if !d.Hardware() {
			t.Skipf("D3D11VA unavailable on this adapter (%s)", d.Status())
		}
		if !d.CanRenderGPU() {
			t.Fatalf("CanRenderGPU() = false while Hardware() = true (status=%s)", d.Status())
		}
		if rendered == 0 {
			filter := d.options.ScaleFilter
			for _, explicit := range []string{"bilinear", "bicubic"} {
				d.options.ScaleFilter = explicit
				if d.CanRenderGPU() {
					t.Fatalf("explicit %s filter must use the CPU scaler", explicit)
				}
			}
			d.options.ScaleFilter = filter
		}

		slotIndex := rendered % gpuSlotCount
		surf, err := d.RenderFrameGPU(slotIndex)
		if err != nil {
			t.Fatalf("RenderFrameGPU(slot %d): %v", slotIndex, err)
		}
		if surf.SharedHandle == 0 {
			t.Fatal("RenderFrameGPU returned zero SharedHandle")
		}
		if surf.AdapterLUID == 0 {
			t.Fatal("RenderFrameGPU returned zero AdapterLUID")
		}
		if surf.AdapterLUID != consumerLUID {
			t.Fatalf("AdapterLUID = %#x, default consumer adapter = %#x", surf.AdapterLUID, consumerLUID)
		}
		if surf.Width != 320 || surf.Height != 240 {
			t.Fatalf("GPU surface size = %dx%d, want 320x240", surf.Width, surf.Height)
		}
		if surf.Timings.TransferNanos != 0 {
			t.Fatalf("TransferNanos = %d, want 0 in zero-copy mode", surf.Timings.TransferNanos)
		}
		if surf.Timings.ConvertNanos <= 0 {
			t.Fatalf("ConvertNanos = %d, want > 0", surf.Timings.ConvertNanos)
		}

		totalConvert += surf.Timings.ConvertNanos
		if rendered == 0 {
			coldConvert = surf.Timings.ConvertNanos
		}
		if surf.Timings.ConvertNanos < minConvert {
			minConvert = surf.Timings.ConvertNanos
		}
		if surf.Timings.ConvertNanos > maxConvert {
			maxConvert = surf.Timings.ConvertNanos
		}

		// Simulate Flutter's ANGLE D3D11 device opening the DXGI shared handle.
		importedTex := openSharedTestTexture(t, consumerDev, surf.SharedHandle)

		var importedDesc d3d11Texture2DDesc
		_, _, _ = syscall.SyscallN(
			importedTex.method(vtblTexture2DGetDesc),
			uintptr(unsafe.Pointer(importedTex)),
			uintptr(unsafe.Pointer(&importedDesc)),
		)
		if importedDesc.Width != 320 || importedDesc.Height != 240 || importedDesc.Format != dxgiFormatB8G8R8A8Unorm {
			importedTex.release()
			t.Fatalf("imported desc = %dx%d fmt=%d, want 320x240 fmt=%d",
				importedDesc.Width, importedDesc.Height, importedDesc.Format, dxgiFormatB8G8R8A8Unorm)
		}

		// Verify cross-device pixel contents on the first rendered frame and compare
		// both ReadbackSlotGPU and CPU software sws_scale output.
		if rendered == 0 {
			sdesc := d3d11Texture2DDesc{
				Width:          320,
				Height:         240,
				MipLevels:      1,
				ArraySize:      1,
				Format:         dxgiFormatB8G8R8A8Unorm,
				SampleCount:    1,
				Usage:          d3d11UsageStaging,
				CPUAccessFlags: d3d11CPUAccessRead,
			}
			var staging *comObject
			r1, _, _ := syscall.SyscallN(
				consumerDev.method(vtblDeviceCreateTexture2D),
				uintptr(unsafe.Pointer(consumerDev)),
				uintptr(unsafe.Pointer(&sdesc)),
				0,
				uintptr(unsafe.Pointer(&staging)),
			)
			if hrFailed(r1) || staging == nil {
				importedTex.release()
				t.Fatalf("consumer CreateTexture2D(staging) failed: 0x%08x", uint32(r1))
			}
			_, _, _ = syscall.SyscallN(
				consumerCtx.method(vtblContextCopyResource),
				uintptr(unsafe.Pointer(consumerCtx)),
				uintptr(unsafe.Pointer(staging)),
				uintptr(unsafe.Pointer(importedTex)),
			)
			var mapped d3d11MappedSubresource
			r1, _, _ = syscall.SyscallN(
				consumerCtx.method(vtblContextMap),
				uintptr(unsafe.Pointer(consumerCtx)),
				uintptr(unsafe.Pointer(staging)),
				0,
				uintptr(d3d11MapRead),
				0,
				uintptr(unsafe.Pointer(&mapped)),
			)
			if hrFailed(r1) || mapped.PData == nil {
				staging.release()
				importedTex.release()
				t.Fatalf("consumer Map(staging) failed: 0x%08x", uint32(r1))
			}

			readbackImg, err := d.ReadbackSlotGPU(slotIndex)
			if err != nil {
				_, _, _ = syscall.SyscallN(consumerCtx.method(vtblContextUnmap), uintptr(unsafe.Pointer(consumerCtx)), uintptr(unsafe.Pointer(staging)), 0)
				staging.release()
				importedTex.release()
				t.Fatalf("ReadbackSlotGPU: %v", err)
			}

			var minB, maxB byte = 255, 0
			for y := 0; y < 240; y++ {
				srcPtr := unsafe.Add(mapped.PData, uintptr(y)*uintptr(mapped.RowPitch))
				bgraRow := unsafe.Slice((*byte)(srcPtr), 320*4)
				rgbaRow := readbackImg.Pixels[y*320*4 : (y+1)*320*4]
				for x := 0; x < 320*4; x += 4 {
					b, g, r, a := bgraRow[x], bgraRow[x+1], bgraRow[x+2], bgraRow[x+3]
					if a != 0xff {
						t.Fatalf("pixel (%d,%d) alpha = 0x%02x, want 0xff", x/4, y, a)
					}
					if b < minB {
						minB = b
					}
					if b > maxB {
						maxB = b
					}
					if rgbaRow[x] != r || rgbaRow[x+1] != g || rgbaRow[x+2] != b || rgbaRow[x+3] != 0xff {
						t.Fatalf("ReadbackSlotGPU swizzle mismatch at (%d,%d): got RGBA(%d,%d,%d,%d), consumer BGRA(%d,%d,%d,%d)",
							x/4, y, rgbaRow[x], rgbaRow[x+1], rgbaRow[x+2], rgbaRow[x+3], b, g, r, a)
					}
				}
			}
			_, _, _ = syscall.SyscallN(consumerCtx.method(vtblContextUnmap), uintptr(unsafe.Pointer(consumerCtx)), uintptr(unsafe.Pointer(staging)), 0)
			staging.release()

			if maxB-minB < 8 {
				importedTex.release()
				t.Fatalf("imported GPU frame looks uniform/blank: blue range %d..%d", minB, maxB)
			}

			// Verify color accuracy against CPU sws_scale (no swapped R/B or wrong range).
			if swImg != nil && len(swImg.Pixels) == len(readbackImg.Pixels) {
				var diffSum int64
				channels := 320 * 240 * 3
				for i := 0; i < len(swImg.Pixels); i += 4 {
					for c := 0; c < 3; c++ {
						d := int(readbackImg.Pixels[i+c]) - int(swImg.Pixels[i+c])
						if d < 0 {
							d = -d
						}
						diffSum += int64(d)
					}
				}
				mad := float64(diffSum) / float64(channels)
				t.Logf("GPU VideoProcessor vs CPU sws_scale mean abs RGB diff = %.2f / 255", mad)
				if mad > 12.0 {
					importedTex.release()
					t.Fatalf("GPU color conversion diverged from CPU sws_scale: MAD=%.2f", mad)
				}
			}
		}

		importedTex.release()
		rendered++
	}

	if rendered == 0 {
		t.Fatal("no frames rendered on GPU")
	}
	avgNanos := totalConvert / int64(rendered)
	t.Logf("GPU D3D11 zero-copy rendered %d frames: avg ConvertNanos=%d (%.3f ms), min=%d (%.3f ms), max=%d (%.3f ms)",
		rendered, avgNanos, float64(avgNanos)/1e6, minConvert, float64(minConvert)/1e6, maxConvert, float64(maxConvert)/1e6)
	if rendered > 1 {
		t.Logf("GPU D3D11 first frame %.3f ms, remaining-frame average %.3f ms",
			float64(coldConvert)/1e6, float64(totalConvert-coldConvert)/float64(rendered-1)/1e6)
	}
	lastSlot := (rendered - 1) % gpuSlotCount
	lastFrame, err := d.ReadbackSlotGPU(lastSlot)
	if err != nil {
		t.Fatalf("last GPU snapshot: %v", err)
	}
	d.rebuild(false)
	retained, err := d.ReadbackSlotGPU(lastSlot)
	if err != nil {
		t.Fatalf("GPU snapshot while waiting for first software frame: %v", err)
	}
	if !bytes.Equal(lastFrame.Pixels, retained.Pixels) {
		t.Fatal("rebuilding the decoder changed the still-published GPU frame")
	}
}

// TestD3D11PoolMacroblockCropAndRotation verifies GPU cropping when the decoded
// NV12 array texture is macroblock-aligned (320x256 -> visible 320x250 on
// non-zero array slice), immutability after reusing a logical slot, and rotation.
func TestD3D11PoolMacroblockCropAndRotation(t *testing.T) {
	dev, ctx := createTestD3D11Device(t)
	defer ctx.release()
	defer dev.release()

	pool := newD3D11Pool()
	defer pool.close()

	texLandscape := makeTestNV12Array(t, dev, ctx, 320, 256)
	defer texLandscape.release()

	pic := d3d11Picture{
		texture: texLandscape, arraySlice: 1,
		width: 320, height: 250, // cropped from 256 macroblock height
		matrix: matrixBT709,
	}
	surf1, err := pool.renderFrame(pic, 0)
	if err != nil {
		t.Fatalf("renderFrame(320x250 cropped from 320x256): %v", err)
	}
	if surf1.Width != 320 || surf1.Height != 250 {
		t.Fatalf("cropped surface = %dx%d, want 320x250", surf1.Width, surf1.Height)
	}
	img1, err := pool.readbackSlotRGBA(0)
	if err != nil {
		t.Fatalf("readbackSlotRGBA(0): %v", err)
	}
	if img1.Width != 320 || img1.Height != 250 {
		t.Fatalf("readback = %dx%d, want 320x250", img1.Width, img1.Height)
	}
	// At (64, 2) the NV12 fixture has Y=128, U=96, V=160. Compare the
	// actual video processor output against the BT.709 RGB reference.
	checkRGB := func(img Image, want [3]int) {
		t.Helper()
		pixel := img.Pixels[2*img.Stride+64*4:]
		for i, v := range want {
			if diff := int(pixel[i]) - v; diff < -4 || diff > 4 {
				t.Fatalf("BT.709 patch RGB = %v, want %v (tolerance 4)", pixel[:3], want)
			}
		}
	}
	checkRGB(img1, [3]int{188, 120, 63})

	// Keep a consumer device's reference open across a same-size slot reuse.
	// This models an ANGLE draw that still reads the old shared resource.
	consumer := newD3D11Pool()
	consumer.device, consumer.context = createTestD3D11Device(t)
	defer consumer.close()
	consumer.slots[0] = d3d11SlotSurface{
		tex:   openSharedTestTexture(t, consumer.device, surf1.SharedHandle),
		width: 320, height: 250,
	}
	pic.fullRange = 1
	replacement, err := pool.renderFrame(pic, 0)
	if err != nil {
		t.Fatalf("renderFrame(replacing slot 0): %v", err)
	}
	if replacement.SharedHandle == surf1.SharedHandle {
		t.Fatal("slot reuse overwrote a shared texture still held by the consumer")
	}
	imgNew, err := pool.readbackSlotRGBA(0)
	if err != nil {
		t.Fatalf("readback new surface: %v", err)
	}
	if bytes.Equal(img1.Pixels, imgNew.Pixels) {
		t.Fatal("full-range conversion did not change the frame")
	}
	checkRGB(imgNew, [3]int{178, 119, 69})
	imgHeld, err := consumer.readbackSlotRGBA(0)
	if err != nil {
		t.Fatalf("consumer readback of held old surface: %v", err)
	}
	if !bytes.Equal(img1.Pixels, imgHeld.Pixels) {
		t.Fatal("held old texture pixels changed when its slot was reused")
	}

	// Simulate portrait rotation on the same slot 0 (256x320 -> visible 250x320).
	texPortrait := makeTestNV12Array(t, dev, ctx, 256, 320)
	defer texPortrait.release()
	pic.texture = texPortrait
	pic.width = 250
	pic.height = 320
	pic.fullRange = 0

	surf2, err := pool.renderFrame(pic, 0)
	if err != nil {
		t.Fatalf("renderFrame(250x320 rotated): %v", err)
	}
	if surf2.Width != 250 || surf2.Height != 320 {
		t.Fatalf("rotated surface = %dx%d, want 250x320", surf2.Width, surf2.Height)
	}

	// Recreate the FFmpeg device while slot zero is still published. A
	// newly arriving reader must be able to open it in this interval.
	old, err := pool.readbackSlotRGBA(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.ensureDevice(consumer.device); err != nil {
		t.Fatal(err)
	}
	openSharedTestTexture(t, consumer.device, surf2.SharedHandle).release()
	retained, err := pool.readbackSlotRGBA(0)
	if err != nil || !bytes.Equal(retained.Pixels, old.Pixels) {
		t.Fatalf("snapshot after device replacement changed: %v", err)
	}
}

func makeTestNV12Array(t testing.TB, dev, ctx *comObject, width, height uint32) *comObject {
	t.Helper()
	const (
		bindDecoder                  = 0x200
		vtblContextUpdateSubresource = 48
	)
	desc := d3d11Texture2DDesc{
		Width:       width,
		Height:      height,
		MipLevels:   1,
		ArraySize:   2,
		Format:      dxgiFormatNV12,
		SampleCount: 1,
		Usage:       d3d11UsageDefault,
		BindFlags:   bindDecoder,
	}
	var tex *comObject
	r1, _, _ := syscall.SyscallN(
		dev.method(vtblDeviceCreateTexture2D),
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(&desc)),
		0,
		uintptr(unsafe.Pointer(&tex)),
	)
	if hrFailed(r1) || tex == nil {
		t.Skipf("CreateTexture2D(NV12 decoder) unsupported: 0x%08x", uint32(r1))
	}
	lumaBytes := int(width * height)
	nv12 := make([]byte, lumaBytes*3/2)
	for i := range lumaBytes {
		nv12[i] = byte(64 + (i % 128))
	}
	for i := lumaBytes; i < len(nv12); i += 2 {
		nv12[i] = 96
		nv12[i+1] = 160
	}
	// Upload into array slice 1 (subresource 1 since MipLevels == 1).
	_, _, _ = syscall.SyscallN(
		ctx.method(vtblContextUpdateSubresource),
		uintptr(unsafe.Pointer(ctx)),
		uintptr(unsafe.Pointer(tex)),
		1,
		0,
		uintptr(unsafe.Pointer(&nv12[0])),
		uintptr(width),
		0,
	)
	return tex
}

func BenchmarkD3D11SharedTexture2K(b *testing.B) {
	dev, ctx := createTestD3D11Device(b)
	defer dev.release()
	defer ctx.release()
	pool := newD3D11Pool()
	defer pool.close()
	tex := makeTestNV12Array(b, dev, ctx, 2560, 1184)
	defer tex.release()
	pic := d3d11Picture{
		texture: tex, arraySlice: 1,
		width: 2560, height: 1178,
		matrix: matrixBT709,
	}
	consumerDev, consumerCtx := createTestD3D11Device(b)
	defer consumerDev.release()
	defer consumerCtx.release()
	for i := range gpuSlotCount {
		surface, err := pool.renderFrame(pic, i)
		if err != nil {
			b.Fatal(err)
		}
		openSharedTestTexture(b, consumerDev, surface.SharedHandle).release()
	}
	var convertNanos int64
	slot := 0
	for b.Loop() {
		surface, err := pool.renderFrame(pic, slot)
		if err != nil {
			b.Fatal(err)
		}
		openSharedTestTexture(b, consumerDev, surface.SharedHandle).release()
		convertNanos += surface.Timings.ConvertNanos
		slot = (slot + 1) % gpuSlotCount
	}
	b.ReportMetric(float64(convertNanos)/float64(b.N), "convert-ns/op")
}
