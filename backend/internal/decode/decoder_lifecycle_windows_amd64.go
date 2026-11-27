//go:build windows && amd64

package decode

import (
	"errors"
	"fmt"
	"log"
	"time"
)

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
