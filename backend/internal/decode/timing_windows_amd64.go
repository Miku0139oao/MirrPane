//go:build windows && amd64

package decode

import (
	"sync"
	"syscall"
	"unsafe"
)

// QueryPerformanceCounter bindings. The decoder reports its per-stage cost in
// nanoseconds; QPC is the same clock the publisher stamps into the shared
// control block, so a timeline can be built across both sides.
var (
	qpcDLL      = syscall.NewLazyDLL("kernel32.dll")
	qpcProc     = qpcDLL.NewProc("QueryPerformanceCounter")
	qpcFreqProc = qpcDLL.NewProc("QueryPerformanceFrequency")

	qpcFreqOnce sync.Once
	qpcTicks    int64
)

// qpcNow reads the performance counter, or returns 0 when the call fails.
func qpcNow() int64 {
	var counter int64
	if r, _, _ := qpcProc.Call(uintptr(unsafe.Pointer(&counter))); r == 0 {
		return 0
	}
	return counter
}

// qpcFrequency reports ticks per second, cached after the first call.
func qpcFrequency() int64 {
	qpcFreqOnce.Do(func() {
		var freq int64
		if r, _, _ := qpcFreqProc.Call(uintptr(unsafe.Pointer(&freq))); r == 0 || freq <= 0 {
			freq = 1
		}
		qpcTicks = freq
	})
	return qpcTicks
}

// qpcElapsedNanos converts the interval from start to now into nanoseconds.
// A zero start means the counter was unavailable and yields no sample.
func qpcElapsedNanos(start int64) int64 {
	if start == 0 {
		return 0
	}
	now := qpcNow()
	if now <= start {
		return 0
	}
	return int64(float64(now-start) * 1e9 / float64(qpcFrequency()))
}
