package frames

import (
	"testing"
	"time"
)

// The gate must let a consuming reader have every picture, and let a reader
// that stopped rendering have only periodic probes. Adoption is what re-arms
// it, so the probe schedule is part of the state machine under test.
func TestConsumerGateStaleVsFresh(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	fresh := ConsumerStats{ConsumerPID: 7, Adopted: 120, ConsumerAgeNanos: int64(16 * time.Millisecond)}
	stale := ConsumerStats{ConsumerPID: 7, Adopted: 120, ConsumerAgeNanos: int64(2 * time.Second)}

	if !g.Allow(fresh) {
		t.Fatal("fresh consumer was gated")
	}
	// A fresh adoption schedules the next probe a full interval later.
	now += int64(500 * time.Millisecond)
	if g.Allow(stale) {
		t.Fatal("stale consumer was allowed before the probe interval elapsed")
	}
	now += int64(500 * time.Millisecond)
	if !g.Allow(stale) {
		t.Fatal("no probe was produced after a full stale interval")
	}
	now += int64(500 * time.Millisecond)
	if g.Allow(stale) {
		t.Fatal("stale consumer got a second picture inside the probe interval")
	}
	now += int64(500 * time.Millisecond)
	if !g.Allow(stale) {
		t.Fatal("the next probe never became due")
	}

	// The reader adopts again: the gate re-arms immediately, not merely at the
	// next probe.
	now += int64(time.Millisecond)
	if !g.Allow(fresh) {
		t.Fatal("gate did not re-arm on a fresh adoption")
	}
}

// An attached reader that has not adopted anything yet must be seeded at full
// rate; otherwise it can never stamp the freshness the gate waits for.
func TestConsumerGateAttachedNotYetAdopted(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	attached := ConsumerStats{ConsumerPID: 99}
	for i := 0; i < 10; i++ {
		if !g.Allow(attached) {
			t.Fatalf("attached reader was gated on picture %d before adopting", i+1)
		}
		now += int64(16 * time.Millisecond)
	}
}

// With no reader at all there is nothing to display, so only probes are
// produced. The first call probes immediately, so a UI that starts later finds
// a picture already published.
func TestConsumerGateNoConsumer(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	if !g.Allow(ConsumerStats{}) {
		t.Fatal("the first picture with no consumer was not a probe")
	}
	if g.Allow(ConsumerStats{}) {
		t.Fatal("pictures kept being produced with no consumer attached")
	}
	now += int64(time.Second)
	if !g.Allow(ConsumerStats{}) {
		t.Fatal("the next probe never became due")
	}
	if g.Allow(ConsumerStats{}) {
		t.Fatal("more than one picture per probe interval")
	}
}

// Without QPC there is no way to tell staleness, so the gate fails open and the
// producer behaves as it did before gating existed.
func TestConsumerGateFailsOpenWithoutClock(t *testing.T) {
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return 0 }
	stale := ConsumerStats{ConsumerPID: 1, Adopted: 5, ConsumerAgeNanos: int64(time.Hour)}
	for i := 0; i < 5; i++ {
		if !g.Allow(stale) {
			t.Fatal("gate gated with no clock available")
		}
	}
	var nilGate *ConsumerGate
	if !nilGate.Allow(stale) {
		t.Fatal("nil gate gated")
	}
}
