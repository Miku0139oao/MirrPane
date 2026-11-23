package audio

import (
	"bytes"
	"encoding/binary"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
)

// Exercise the packet path with no sockets, native codec or real-time waits.
type jitterFixture struct {
	r         *Receiver
	now       time.Time
	logs      bytes.Buffer
	decodes   int
	delivered int
}

func newJitterFixture(t *testing.T) *jitterFixture {
	t.Helper()
	f := &jitterFixture{now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
	f.r = &Receiver{
		log:    log.New(&f.logs, "", 0),
		keys:   []Key{{Name: "test", Key: bytes.Repeat([]byte{0x11}, 16)}},
		iv:     bytes.Repeat([]byte{0x22}, 16),
		chosen: -1,
		now:    func() time.Time { return f.now },
		dec: &testAudioDecoder{
			onDecode: func([]byte) (*decode.AudioFrame, error) {
				f.decodes++
				return &decode.AudioFrame{Samples: make([]int16, 522*2), Channels: 2, SampleRate: 48000}, nil
			},
			onConfigure: func([]byte) error { return nil },
			onFlush:     func() {},
		},
		handlers: Handlers{OnFrame: func(*decode.AudioFrame) { f.delivered++ }},
	}
	return f
}

func jitterSyncPacket(rtp uint32, ntp uint64, next uint32) []byte {
	pkt := make([]byte, 20)
	pkt[0], pkt[1] = 0x80, typeSync|0x80
	binary.BigEndian.PutUint32(pkt[4:8], rtp)
	binary.BigEndian.PutUint64(pkt[8:16], ntp)
	binary.BigEndian.PutUint32(pkt[16:20], next)
	return pkt
}

func (f *jitterFixture) sync(rtp uint32) {
	f.r.handleSync(jitterSyncPacket(rtp, 0x123456789abcdef0, rtp+7497))
}

func (f *jitterFixture) packet(seq uint16, rtp uint32) {
	pkt := testAudioPacket(seq)
	binary.BigEndian.PutUint32(pkt[4:8], rtp)
	f.r.handlePacket(pkt)
}

func TestPacketDeadlineUsesSenderClockAndTolerance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta int64
		late  bool
	}{
		{"current", 0, false},
		{"30ms_behind", -1323, false},
		{"exactly_40ms_behind", -1764, false},
		{"one_tick_past_margin", -1765, true},
		{"100ms_behind", -4410, true},
		{"30ms_ahead", 1323, false},
		{"one_second_ahead", 44100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJitterFixture(t)
			f.sync(100000)
			f.now = f.now.Add(time.Second)
			// Exactly 44,100 RTP units, independent of 48,000 output samples.
			f.packet(1, uint32(144100+tc.delta))
			got, dropped, decoded := f.r.Stats()
			if tc.late {
				if got != 1 || dropped != 1 || decoded != 0 || f.r.lateDropped != 1 || f.decodes != 0 || f.delivered != 0 {
					t.Fatalf("late packet reached the decoder: stats=(%d,%d,%d) late=%d decodes=%d delivered=%d", got, dropped, decoded, f.r.lateDropped, f.decodes, f.delivered)
				}
				if f.r.chosen != -1 {
					t.Fatal("late packet reached decryption/key selection")
				}
				if !f.r.lastFrame.IsZero() {
					t.Fatal("late packet refreshed decoded-audio liveness")
				}
			} else if got != 1 || dropped != 0 || decoded != 1 || f.r.lateDropped != 0 || f.decodes != 1 || f.delivered != 1 {
				t.Fatalf("valid packet was not delivered: stats=(%d,%d,%d), late=%d", got, dropped, decoded, f.r.lateDropped)
			}
		})
	}
}

func TestPacketDeadlineRequiresFreshSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
		age  time.Duration
		late bool
	}{
		{"absent", false, 0, false},
		{"fresh_zero_RTP", true, 0, true},
		{"exactly_five_seconds", true, 5 * time.Second, true},
		{"expired", true, 5*time.Second + time.Nanosecond, false},
		{"local_clock_before_receipt", true, -time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJitterFixture(t)
			if tc.sync {
				f.sync(0) // zero is a valid RTP origin, not an absent-sync marker
			}
			f.now = f.now.Add(tc.age)
			f.packet(1, ^uint32(0)-4410)
			if tc.late {
				if f.decodes != 0 || f.r.lateDropped != 1 {
					t.Fatalf("fresh sync failed to reject old packet: decodes=%d late=%d", f.decodes, f.r.lateDropped)
				}
			} else if f.decodes != 1 || f.r.lateDropped != 0 {
				t.Fatalf("missing/stale clock suppressed fallback: decodes=%d late=%d", f.decodes, f.r.lateDropped)
			}
		})
	}
}

func TestPacketDeadlineWrapsRTPClock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		syncRTP   uint32
		age       time.Duration
		packetRTP uint32
		late      bool
	}{
		{"extrapolated_current_after_wrap", ^uint32(0) - 2000, 100 * time.Millisecond, 2409, false},
		{"old_packet_before_wrap", ^uint32(0) - 2000, 100 * time.Millisecond, ^uint32(0) - 500, true},
		{"future_packet_after_wrap", ^uint32(0) - 1000, 0, 1000, false},
		{"past_packet_before_wrap", 1000, 0, ^uint32(0) - 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJitterFixture(t)
			f.sync(tc.syncRTP)
			f.now = f.now.Add(tc.age)
			f.packet(1, tc.packetRTP)
			if got := f.r.lateDropped == 1; got != tc.late || (f.decodes == 0) != tc.late {
				t.Fatalf("wrap decision: late=%d decodes=%d, want late=%t", f.r.lateDropped, f.decodes, tc.late)
			}
		})
	}
}

func TestSequenceDedupePrecedesDeadlineAndCountsLateOnce(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	f.packet(10, 100000)
	f.packet(10, 50000) // duplicate with an old timestamp is still a duplicate
	f.packet(9, 50000)
	f.packet(11, 50000)
	f.packet(11, 50000)
	f.packet(11, 100000) // a late packet already advanced sequence history
	got, dropped, decoded := f.r.Stats()
	if got != 6 || dropped != 5 || decoded != 1 || f.r.lateDropped != 1 || f.decodes != 1 {
		t.Fatalf("dedupe/deadline accounting = (%d,%d,%d), late=%d decodes=%d", got, dropped, decoded, f.r.lateDropped, f.decodes)
	}
}

func TestLateDropDoesNotBreakSequenceWrap(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	f.packet(65535, 50000)
	f.packet(0, 100000)
	f.packet(0, 50000)
	if f.r.lateDropped != 1 || f.decodes != 1 || f.r.lastSeq != 0 {
		t.Fatalf("sequence wrap after a late packet: late=%d decodes=%d seq=%d", f.r.lateDropped, f.decodes, f.r.lastSeq)
	}
}

func TestFreshSyncReanchorsDeadlineAfterExpiry(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	f.now = f.now.Add(6 * time.Second)
	f.packet(1, 0) // expired mapping preserves the original fallback
	f.r.handleSync(jitterSyncPacket(500000, ^uint64(0), 507497))
	f.packet(2, 498235) // one tick more than 40 ms behind the NEW origin
	f.packet(3, 500000)
	if f.r.lateDropped != 1 || f.decodes != 2 || f.delivered != 2 || !f.r.syncClock.localTime.Equal(f.now) {
		t.Fatalf("new sync did not replace expired timing: late=%d decodes=%d clock=%+v", f.r.lateDropped, f.decodes, f.r.syncClock)
	}
}

func TestCatchUpBurstDropsOldAudioBeforeDecoder(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	f.now = f.now.Add(300 * time.Millisecond)
	// A 300 ms catch-up burst of 28 unique 480-tick packets, each repeated
	// three times. Only the last four are within 40 ms of current playout.
	for i := range 28 {
		for range 3 {
			f.packet(uint16(i+1), uint32(100000+i*480))
		}
	}
	got, dropped, decoded := f.r.Stats()
	if got != 84 || dropped != 80 || decoded != 4 || f.r.lateDropped != 24 || f.decodes != 4 || f.delivered != 4 {
		t.Fatalf("catch-up burst queued obsolete audio: stats=(%d,%d,%d) late=%d decodes=%d delivered=%d", got, dropped, decoded, f.r.lateDropped, f.decodes, f.delivered)
	}
}

func TestSyncStoresMappingAndCallsHandlerUnlocked(t *testing.T) {
	f := newJitterFixture(t)
	const rtp, next = 100000, 107497
	const ntp = uint64(0xfedcba9876543210)
	called := 0
	f.r.handlers.OnSync = func(gotRTP uint32, gotNTP uint64, gotNext uint32) {
		called++
		if gotRTP != rtp || gotNTP != ntp || gotNext != next {
			t.Errorf("sync callback changed mapping: %d %x %d", gotRTP, gotNTP, gotNext)
		}
		if !f.r.mu.TryLock() {
			t.Error("sync callback held the receiver state lock")
		} else {
			f.r.mu.Unlock()
		}
	}
	f.r.handleSync(jitterSyncPacket(rtp, ntp, next))
	clock := f.r.syncClock
	if !clock.valid || clock.rtp != rtp || clock.ntp != ntp || !clock.localTime.Equal(f.now) || called != 1 {
		t.Fatalf("sync mapping not retained: %+v callbacks=%d", clock, called)
	}
	f.now = f.now.Add(time.Second)
	f.r.handleSync(make([]byte, 19))
	if f.r.syncClock != clock || called != 1 {
		t.Fatal("truncated sync refreshed or replaced the mapping")
	}
	// Raw sender NTP need not resemble the host's wall clock. Local elapsed
	// time and the first RTP field, not the 'next RTP' field, drive the check.
	f.packet(1, rtp+44100)
	if f.decodes != 1 {
		t.Fatal("deadline calculation used sender NTP or next RTP as local time")
	}
}

func TestStreamLifecycleClearsSyncButKeepsCounters(t *testing.T) {
	for _, action := range []string{"configure", "reset", "reset_key"} {
		t.Run(action, func(t *testing.T) {
			f := newJitterFixture(t)
			f.sync(100000)
			f.packet(1, 0)
			switch action {
			case "configure":
				f.r.Configure(f.r.keys, f.r.iv)
			case "reset":
				f.r.ResetStream()
			case "reset_key":
				f.r.ResetKey()
			}
			if f.r.syncClock.valid || f.r.haveLast || f.r.lateDropped != 1 {
				t.Fatalf("lifecycle kept old stream timing or reset counters: clock=%+v haveLast=%t late=%d", f.r.syncClock, f.r.haveLast, f.r.lateDropped)
			}
			if action != "reset_key" {
				f.packet(1, 0)
				if f.decodes != 1 || f.r.lateDropped != 1 {
					t.Fatal("old mapping suppressed the new stream before its first sync")
				}
			}
		})
	}
}

func TestLateDropLoggingIsSampledAndCumulative(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	for seq := uint16(1); seq <= 101; seq++ {
		f.packet(seq, 0)
	}
	f.now = f.now.Add(4 * time.Second)
	f.sync(276400)
	f.packet(102, 0)
	f.now = f.now.Add(time.Second)
	f.packet(103, 0)
	if got := strings.Count(f.logs.String(), "late RTP dropped"); got != 2 || !strings.Contains(f.logs.String(), "lateDropped=103") {
		t.Fatalf("late-drop logs were not sampled/cumulative: %s", f.logs.String())
	}
}

func TestSyncPacketsAndResetsAreSerializedWithDeadlineChecks(t *testing.T) {
	f := newJitterFixture(t)
	f.sync(100000)
	f.now = f.now.Add(100 * time.Millisecond)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 500 {
			f.packet(uint16(i+1), 101000)
		}
	}()
	go func() {
		defer wg.Done()
		for range 500 {
			f.sync(100000)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			f.r.ResetStream()
		}
	}()
	wg.Wait()
	got, dropped, decoded := f.r.Stats()
	if got != 500 || dropped+decoded != got || f.r.lateDropped > dropped || uint64(f.decodes) != decoded || uint64(f.delivered) != decoded {
		t.Fatalf("concurrent timing accounting: stats=(%d,%d,%d) late=%d decodes=%d delivered=%d", got, dropped, decoded, f.r.lateDropped, f.decodes, f.delivered)
	}
}
