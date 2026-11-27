//go:build windows

package audioout

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows waveOut constants.
const (
	waveMapper                 = 0xFFFFFFFF
	callbackNull               = 0x00000000
	callbackEvent              = 0x00050000
	whdrDone                   = 0x00000001
	waveFormatPCM              = 1
	threadPriorityTimeCritical = 15
)

var (
	winmm    = windows.NewLazySystemDLL("winmm.dll")
	avrt     = windows.NewLazySystemDLL("avrt.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procWaveOutOpen       = winmm.NewProc("waveOutOpen")
	procWaveOutClose      = winmm.NewProc("waveOutClose")
	procWaveOutPrepare    = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepare  = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite      = winmm.NewProc("waveOutWrite")
	procWaveOutReset      = winmm.NewProc("waveOutReset")
	procWaveOutGetNumDevs = winmm.NewProc("waveOutGetNumDevs")
	procWaveOutMessage    = winmm.NewProc("waveOutMessage")
	procTimeBeginPeriod   = winmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod     = winmm.NewProc("timeEndPeriod")

	procAvSetMmThreadCharacteristicsW   = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMmThreadCharacteristics = avrt.NewProc("AvRevertMmThreadCharacteristics")
	procSetThreadPriority               = kernel32.NewProc("SetThreadPriority")
)

var errNoOutputDevice = errors.New("audioout: no output device")

// waveformatex mirrors the Win32 structure. Only the plain PCM fields are used.
type waveformatex struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

// wavehdr mirrors WAVEHDR.
type wavehdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

// elevatePumpThread binds the audio pump to one OS thread and joins Windows's
// MMCSS "Pro Audio" class so 60 fps D3D11VA/sws_scale work cannot preempt
// waveOut header refills.
func elevatePumpThread() func() {
	runtime.LockOSThread()
	var mmcss uintptr
	if procAvSetMmThreadCharacteristicsW.Find() == nil {
		if name, err := windows.UTF16PtrFromString("Pro Audio"); err == nil {
			var taskIndex uint32
			h, _, _ := procAvSetMmThreadCharacteristicsW.Call(
				uintptr(unsafe.Pointer(name)),
				uintptr(unsafe.Pointer(&taskIndex)))
			runtime.KeepAlive(name)
			mmcss = h
		}
	}
	if mmcss == 0 && procSetThreadPriority.Find() == nil {
		if th, err := windows.GetCurrentThread(); err == nil {
			procSetThreadPriority.Call(uintptr(th), uintptr(threadPriorityTimeCritical))
		}
	}
	return func() {
		if mmcss != 0 && procAvRevertMmThreadCharacteristics.Find() == nil {
			procAvRevertMmThreadCharacteristics.Call(mmcss)
		}
		runtime.UnlockOSThread()
	}
}

// Ask the same wave mapper used by waveOutOpen for the current endpoint ID.
// Two endpoints can share a model and display name, and wave IDs can be reused.
func defaultDeviceSignature() (string, error) {
	const (
		preferredGet = 0x2000 + 21 // DRVM_MAPPER_PREFERRED_GET, mmddk.h
		instanceID   = 0x0800 + 17 // DRV_QUERYFUNCTIONINSTANCEID
		instanceSize = 0x0800 + 18 // DRV_QUERYFUNCTIONINSTANCEIDSIZE
	)
	var id, flags, size uint32
	ret, _, _ := procWaveOutMessage.Call(waveMapper, preferredGet, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&flags)))
	if ret != 0 {
		return "", fmt.Errorf("audioout: query preferred wave device: %d", ret)
	}
	if id == waveMapper {
		return "", errNoOutputDevice
	}
	ret, _, _ = procWaveOutMessage.Call(uintptr(id), instanceSize, uintptr(unsafe.Pointer(&size)), 0)
	if ret != 0 || size < 2 || size > 65536 || size%2 != 0 {
		return "", fmt.Errorf("audioout: query endpoint ID size: %d (%d bytes)", ret, size)
	}
	buf := make([]uint16, size/2)
	ret, _, _ = procWaveOutMessage.Call(uintptr(id), instanceID, uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
	if ret != 0 {
		return "", fmt.Errorf("audioout: query endpoint ID: %d", ret)
	}
	return windows.UTF16ToString(buf), nil
}
