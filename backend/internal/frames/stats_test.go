package frames

import (
	"sync/atomic"
	"testing"
)

func TestRollingStatsEmpty(t *testing.T) {
	var r RollingStats
	if r.Len() != 0 || r.Mean() != 0 || r.P95() != 0 {
		t.Fatalf("empty window = len %d mean %v p95 %v", r.Len(), r.Mean(), r.P95())
	}
}

func TestRollingStatsSingle(t *testing.T) {
	var r RollingStats
	r.Add(7)
	if r.Len() != 1 {
		t.Fatalf("len = %d, want 1", r.Len())
	}
	if r.Mean() != 7 || r.P95() != 7 {
		t.Fatalf("single sample = mean %v p95 %v", r.Mean(), r.P95())
	}
}

func TestRollingStatsMeanAndP95(t *testing.T) {
	var r RollingStats
	for i := 1; i <= 100; i++ {
		r.Add(float64(i))
	}
	if got := r.Mean(); got != 50.5 {
		t.Fatalf("mean = %v, want 50.5", got)
	}
	// Nearest rank: ceil(0.95*100) = 95, so the 95th smallest sample.
	if got := r.P95(); got != 95 {
		t.Fatalf("p95 = %v, want 95", got)
	}
}

// Once the window is full, the oldest samples evict first and the aggregates
// describe only the most recent StatsWindow samples.
func TestRollingStatsWindowWraps(t *testing.T) {
	var r RollingStats
	for i := 0; i < 200; i++ {
		r.Add(float64(i))
	}
	if r.Len() != StatsWindow {
		t.Fatalf("len = %d, want %d", r.Len(), StatsWindow)
	}
	// The window now holds 80..199; mean and nearest-rank p95 follow.
	if got := r.Mean(); got != 139.5 {
		t.Fatalf("mean = %v, want 139.5", got)
	}
	if got := r.P95(); got != 193 {
		t.Fatalf("p95 = %v, want 193", got)
	}
	var reset RollingStats
	reset.Add(1)
	reset.Reset()
	if reset.Len() != 0 || reset.Mean() != 0 {
		t.Fatalf("reset left %d samples", reset.Len())
	}
}

func TestConsumerStatsLag(t *testing.T) {
	p, err := NewPublisher(`Local\MirrPaneFramesTestStats`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	img := make([]byte, 32*16*4)
	for i := 0; i < 10; i++ {
		if err := p.Publish(32, 16, 32*4, img); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// A reader that mapped but has not adopted anything yet is not dropping.
	atomic.StoreUint64(&p.ctrl.ConsumerAdopted, 0)
	if got := p.ConsumerStats().LagFrames; got != 0 {
		t.Fatalf("lag before first adoption = %d, want 0", got)
	}

	// The reader adopted four of the ten published pictures.
	atomic.StoreUint64(&p.ctrl.ConsumerAdopted, 4)
	atomic.StoreUint32(&p.ctrl.ConsumerPID, 4242)
	atomic.StoreUint32(&p.ctrl.ConsumerSlot, 2)
	got := p.ConsumerStats()
	if got.Published != 10 {
		t.Fatalf("published = %d, want 10", got.Published)
	}
	if got.Adopted != 4 || got.LagFrames != 6 {
		t.Fatalf("adopted/lag = %d/%d, want 4/6", got.Adopted, got.LagFrames)
	}
	if got.ConsumerPID != 4242 || got.ConsumerSlot != 2 {
		t.Fatalf("consumer pid/slot = %d/%d", got.ConsumerPID, got.ConsumerSlot)
	}

	// A stale counter from a previous producer session must clamp, not wrap.
	atomic.StoreUint64(&p.ctrl.ConsumerAdopted, 99)
	if got := p.ConsumerStats().LagFrames; got != 0 {
		t.Fatalf("lag after counter reset = %d, want 0", got)
	}
}

func TestConsumerAge(t *testing.T) {
	p, err := NewPublisher(`Local\MirrPaneFramesTestAge`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	freq := qpcFrequency()
	if freq <= 1 {
		t.Skip("QueryPerformanceFrequency unavailable")
	}
	// Stamp the consumer one millisecond in the past.
	oneMs := freq / 1000
	now := nowQPC()
	if now <= oneMs {
		t.Skip("performance counter too small for an age sample")
	}
	atomic.StoreUint64(&p.ctrl.ConsumerQPC, uint64(now-oneMs))

	age := p.ConsumerStats().ConsumerAgeNanos
	if age < int64(0.9e6) || age > int64(50e6) {
		t.Fatalf("consumer age = %d ns, want about 1 ms", age)
	}
}

// Publish must stamp real QPC values, not wall-clock nanoseconds, and report
// how long it took.
func TestPublishStampsQPCAndDuration(t *testing.T) {
	p, err := NewPublisher(`Local\MirrPaneFramesTestQPC`, 64, 64)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	defer p.Close()

	if freq := qpcFrequency(); freq <= 1 {
		t.Skip("QueryPerformanceFrequency unavailable")
	}
	img := make([]byte, 128*64*4)
	if err := p.Publish(128, 64, 128*4, img); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := atomic.LoadUint64(&p.ctrl.LastPublishQPC); got == 0 {
		t.Fatal("LastPublishQPC was not stamped")
	}
	if got := atomic.LoadUint64(&p.ctrl.HeartbeatQPC); got == 0 {
		t.Fatal("HeartbeatQPC was not stamped")
	}
	slot := &p.ctrl.Slots[p.ctrl.Publish&0x3]
	if slot.WriteQPC == 0 {
		t.Fatal("slot WriteQPC was not stamped")
	}
	// A wall-clock stamp (UnixNano) is ~1.7e18; QPC ticks count since boot and
	// stay under ~5e15. This catches a regression to time.Now().UnixNano().
	if slot.WriteQPC > 1<<52 {
		t.Fatalf("slot WriteQPC = %d looks like wall-clock nanoseconds, not QPC", slot.WriteQPC)
	}
	if p.LastPublishNanos() < 0 {
		t.Fatalf("LastPublishNanos = %d", p.LastPublishNanos())
	}
}
