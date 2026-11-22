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

	fresh := ConsumerStats{ConsumerPID: 7, Adopted: 120, Published: 121, ConsumerAgeNanos: int64(16 * time.Millisecond)}
	if !g.Allow(fresh) {
		t.Fatal("fresh consumer was gated")
	}
	g.Published(true)

	// The reader stops adopting while pictures keep coming.
	stale := ConsumerStats{ConsumerPID: 7, Adopted: 120, Published: 200, ConsumerAgeNanos: int64(2 * time.Second)}
	if g.Allow(stale) {
		t.Fatal("stalled consumer was allowed before the probe interval elapsed")
	}

	// A full interval after the last commit the probe becomes due.
	now += int64(500 * time.Millisecond)
	if g.Allow(stale) {
		t.Fatal("stalled consumer was allowed before the probe was due")
	}
	now += int64(500 * time.Millisecond)
	if !g.Allow(stale) {
		t.Fatal("no probe was produced after a full interval")
	}
	g.Published(true)

	// Committing a probe after a gated stretch opens a resume burst so the
	// reader can catch up without waiting a full second.
	now += int64(100 * time.Millisecond)
	if !g.Allow(stale) {
		t.Fatal("resume burst did not allow the first picture after the probe")
	}
	g.Published(true)

	// Once the burst ends and the reader is still not adopting, gating resumes.
	now += int64(500 * time.Millisecond)
	if g.Allow(stale) {
		t.Fatal("gate never resumed gating after the resume burst")
	}

	// A fresh adoption re-arms the gate immediately, not merely at the next probe.
	now += int64(time.Millisecond)
	if !g.Allow(fresh) {
		t.Fatal("gate did not re-arm on a fresh adoption")
	}
}

// A probe access unit that produced no picture must not consume the interval:
// the very next access unit may probe again.
func TestConsumerGateProbeRetriesAfterNoFrame(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	if !g.Allow(ConsumerStats{}) {
		t.Fatal("first picture with no consumer was not a probe")
	}
	g.Published(false) // the access unit yielded no frame
	if !g.Allow(ConsumerStats{}) {
		t.Fatal("a failed probe consumed the probe interval")
	}
	g.Published(true)
	if g.Allow(ConsumerStats{}) {
		t.Fatal("two probes inside one interval")
	}
	now += int64(time.Second)
	if !g.Allow(ConsumerStats{}) {
		t.Fatal("the next probe never became due")
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
		g.Published(true)
		now += int64(16 * time.Millisecond)
	}
}

// A static sender stops producing for longer than staleAfter while the window
// is visible. The old adoption timestamp must not be mistaken for a stalled
// reader, and the first frames after motion resumes must not be gated while
// the reader wakes up.
func TestConsumerGateStaticScreenWakeDoesNotStutter(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	// Caught up: everything published has been adopted, but the last adoption
	// is old because the sender was static.
	caughtUp := ConsumerStats{ConsumerPID: 7, Adopted: 100, Published: 100, ConsumerAgeNanos: int64(5 * time.Second)}
	if !g.Allow(caughtUp) {
		t.Fatal("static screen was treated as a stalled reader")
	}
	g.Published(true)

	// Motion resumes: the reader has not caught up yet and its last adoption is
	// still old. The resume burst must allow the following frames.
	waking := ConsumerStats{ConsumerPID: 7, Adopted: 100, Published: 101, ConsumerAgeNanos: int64(5 * time.Second)}
	for i := 0; i < 5; i++ {
		now += int64(16 * time.Millisecond)
		if !g.Allow(waking) {
			t.Fatalf("frame %d after a static-screen pause was gated", i+1)
		}
		g.Published(true)
		waking.Published++
	}

	// The burst ends; a reader that still has not adopted is gated again.
	now += int64(600 * time.Millisecond)
	if g.Allow(waking) {
		t.Fatal("gate never resumed after the resume burst")
	}
}

// The zero-value stats (no reader at all) only get probes.
func TestConsumerGateNoConsumer(t *testing.T) {
	now := int64(time.Second)
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return now }

	if !g.Allow(ConsumerStats{}) {
		t.Fatal("the first picture with no consumer was not a probe")
	}
	g.Published(true)
	if g.Allow(ConsumerStats{}) {
		t.Fatal("pictures kept being produced with no consumer attached")
	}
	now += int64(time.Second)
	if !g.Allow(ConsumerStats{}) {
		t.Fatal("the next probe never became due")
	}
	g.Published(true)
	if g.Allow(ConsumerStats{}) {
		t.Fatal("more than one picture per probe interval")
	}
}

// Without QPC there is no way to tell staleness, so the gate fails open and the
// producer behaves as it did before gating existed.
func TestConsumerGateFailsOpenWithoutClock(t *testing.T) {
	g := NewConsumerGate(time.Second, time.Second)
	g.clock = func() int64 { return 0 }
	stale := ConsumerStats{ConsumerPID: 1, Adopted: 5, Published: 50, ConsumerAgeNanos: int64(time.Hour)}
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
