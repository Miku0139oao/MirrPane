package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
)

func wireAudio(seq uint16) []byte {
	pkt := append(testAudioPacket(seq), byte(seq))
	pkt[0], pkt[1] = 0x80, 0x60
	return pkt
}

func bindFixture(t *testing.T, f *jitterFixture, from netip.AddrPort) {
	t.Helper()
	f.r.ConfigurePeer(from.Addr(), int(from.Port()), f.r.keys, f.r.iv)
	t.Cleanup(f.r.ResetKey)
}

func TestPeerBindingAndOrderedResend(t *testing.T) {
	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	f := newJitterFixture(t)
	f.r.control = local
	remote := control.LocalAddr().(*net.UDPAddr).AddrPort()
	from := netip.AddrPortFrom(remote.Addr(), 12345)
	bindFixture(t, f, remote)
	other := netip.MustParseAddrPort("192.0.2.8:9000")
	f.r.receiveDatagram(other, wireAudio(1), false)
	f.r.receiveDatagram(other, jitterSyncPacket(1, 2, 3), true)
	f.r.receiveDatagram(from, jitterSyncPacket(1, 2, 3), true)
	if f.r.haveLast || f.r.syncClock.valid || f.r.packets != 0 {
		t.Fatal("input outside the negotiated endpoints changed stream state")
	}
	var order []byte
	f.r.dec.(*testAudioDecoder).onDecode = func(data []byte) (*decode.AudioFrame, error) {
		order = append(order, data[1])
		return &decode.AudioFrame{Samples: []int16{1, 1}, Channels: 2}, nil
	}
	f.r.receiveDatagram(from, wireAudio(65535), false)
	f.r.receiveDatagram(from, wireAudio(1), false)
	resent := append([]byte{0x80, 0x80 | typeResend, 0, 0}, wireAudio(0)...)
	f.r.receiveDatagram(remote, resent, true)
	if !slices.Equal(order, []byte{255, 0, 1}) {
		t.Fatalf("decoder order across wrap: %v", order)
	}
	var req [8]byte
	control.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := control.ReadFromUDP(req[:])
	if err != nil || n != len(req) || req[1] != 0x80|typeResendReq || binary.BigEndian.Uint16(req[4:6]) != 0 || binary.BigEndian.Uint16(req[6:]) != 1 {
		t.Fatalf("resend request: %x, %v", req, err)
	}
	f.r.ResetStream()
	f.r.receiveDatagram(remote, jitterSyncPacket(111, 2, 3), true)
	f.r.receiveDatagram(from, wireAudio(2), false)
	if f.r.haveLast || f.r.syncClock.valid || len(order) != 3 {
		t.Fatal("ended stream accepted media")
	}
}

func TestMissingResendExpiresAndResetCancelsPendingFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newJitterFixture(t)
		var delivered atomic.Int64
		f.r.handlers.OnFrame = func(*decode.AudioFrame) { delivered.Add(1) }
		from := netip.MustParseAddrPort("192.0.2.1:8000")
		bindFixture(t, f, from)
		f.r.receiveDatagram(from, wireAudio(10), false)
		f.r.receiveDatagram(from, wireAudio(12), false)
		if delivered.Load() != 1 {
			t.Fatal("a packet crossed an open gap")
		}
		time.Sleep(latePacketMargin)
		synctest.Wait()
		if delivered.Load() != 2 {
			t.Fatal("packet still queued after repair deadline")
		}
		f.r.receiveDatagram(from, wireAudio(14), false)
		f.r.ResetKey()
		time.Sleep(latePacketMargin)
		synctest.Wait()
		if delivered.Load() != 2 || len(f.r.repair.packets) != 0 {
			t.Fatal("a queued packet outlived its session")
		}
	})
}

func TestUnconfirmedDataPortCannotSetSequenceOrClock(t *testing.T) {
	f := newJitterFixture(t)
	data := netip.MustParseAddrPort("192.0.2.1:5000")
	sync := netip.AddrPortFrom(data.Addr(), 5020)
	r := f.r
	r.ConfigurePeer(data.Addr(), 0, r.keys, r.iv)
	r.receiveDatagram(sync, jitterSyncPacket(42, 0, 0), true)
	incomplete := wireAudio(30000)
	incomplete[HeaderLen] = 0
	r.receiveDatagram(sync, incomplete, false)
	if r.haveLast || r.syncClock.valid || r.dataSource.IsValid() {
		t.Fatal("unconfirmed port selected a sequence baseline or clock")
	}
	r.receiveDatagram(data, wireAudio(1), false)
	r.receiveDatagram(sync, jitterSyncPacket(1, 0, 0), true)
	r.receiveDatagram(data, jitterSyncPacket(2, 0, 0), true)
	if r.lastSeq != 1 || r.dataSource != data || !r.syncClock.valid || r.syncClock.rtp != 1 {
		t.Fatal("confirmed data and sync endpoints did not retain ownership")
	}
}

func TestKeySelectionUsesDecodedPCMAndSamplesErrors(t *testing.T) {
	f := newJitterFixture(t)
	r := f.r
	r.keys = append(r.keys, Key{"compat", bytes.Repeat([]byte{0x44}, 16)})
	calls, flushes := 0, 0
	d := r.dec.(*testAudioDecoder)
	d.onFlush = func() { flushes++ }
	d.onDecode = func([]byte) (*decode.AudioFrame, error) {
		calls++
		if calls == 2 {
			return &decode.AudioFrame{Samples: []int16{4, 4}, Channels: 2}, nil
		}
		return nil, errors.New("invalid AAC")
	}
	r.handlePacket(wireAudio(1)) // short, unencrypted tail looks plausible to both candidates
	if r.chosen != 1 || calls != 2 || flushes != 1 || f.delivered != 1 {
		t.Fatalf("chosen %d, calls %d, flushes %d, delivered %d", r.chosen, calls, flushes, f.delivered)
	}
	for i := 2; i < 70; i++ {
		r.handlePacket(wireAudio(uint16(i)))
	}
	if r.chosen != -1 || calls > 12 || strings.Count(f.logs.String(), "audio: decode:") != 1 {
		t.Fatalf("probe or logging budget exceeded: calls %d, chosen %d, log %s", calls, r.chosen, f.logs.String())
	}
}
