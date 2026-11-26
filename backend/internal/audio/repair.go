package audio

import (
	"encoding/binary"
	"net/netip"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
)

const maxRepairGap = 8

// Only packets beyond a small gap enter the queue. In-order audio passes
// straight to the decoder. A missing last packet cannot leave the queue stuck.
type repairQueue struct {
	packets    map[uint16][]byte
	timer      *time.Timer
	timerID    uint64
	requestSeq uint16
	furthest   uint16
}

func (r *Receiver) cancelRepair() {
	if r.repair.timer != nil {
		r.repair.timer.Stop()
		r.repair.timer = nil
	}
	r.repair.timerID++
}

func (r *Receiver) clearIngress() {
	r.cancelRepair()
	clear(r.repair.packets)
	r.peer = netip.Addr{}
	r.remoteControl, r.dataSource = netip.AddrPort{}, netip.AddrPort{}
	r.controlSource = netip.AddrPort{}
	r.chosenFailures, r.probeFailures = 0, 0
	r.haveTrial = false
	r.nextProbe, r.lastDecodeLog = time.Time{}, time.Time{}
}

// Validate under the same lock as decode and SETUP. A packet waiting in one
// socket goroutine cannot change the clock or sequence of a subsequent owner.
func (r *Receiver) receiveDatagram(from netip.AddrPort, pkt []byte, control bool) {
	r.decMu.Lock()
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	if r.closed {
		r.decMu.Unlock()
		return
	}
	if !r.peer.IsValid() || from.Addr() != r.peer {
		if c := r.diag.peerDrops.Add(1); r.debug && (c <= 5 || c%100 == 0) {
			r.log.Printf("audio: debug drop peer #%d from=%v peer=%v len=%d type=%#x", c, from, r.peer, len(pkt), packetType(pkt))
		}
		r.decMu.Unlock()
		return
	}
	if len(pkt) < 2 {
		r.diag.shortDrops.Add(1)
		r.decMu.Unlock()
		return
	}
	if control {
		if r.remoteControl.IsValid() && from != r.remoteControl {
			r.decMu.Unlock()
			return
		}
		switch pkt[1] & 0x7f {
		case typeSync:
			// When SETUP omits a control port, learn the sync source only
			// after this sender has produced audio with the negotiated key.
			if len(pkt) < 20 || (!r.remoteControl.IsValid() && (!r.dataSource.IsValid() ||
				(r.controlSource.IsValid() && r.controlSource != from))) {
				r.decMu.Unlock()
				return
			}
			r.controlSource = from
			rtp, ntp, next := r.recordSync(pkt)
			r.decMu.Unlock()
			if r.handlers.OnSync != nil {
				r.handlers.OnSync(rtp, ntp, next)
			}
			return
		case typeResend:
			if len(pkt) < 4+HeaderLen {
				r.decMu.Unlock()
				return
			}
			pkt = pkt[4:]
		default:
			if pkt[0]>>6 == 2 {
				if c := r.diag.controlRTPDrops.Add(1); r.debug && (c <= 5 || c%100 == 0) {
					r.log.Printf("audio: debug RTP on control socket #%d from=%v len=%d type=%#x", c, from, len(pkt), packetType(pkt))
				}
			}
			r.decMu.Unlock()
			return
		}
	} else if r.dataSource.IsValid() && from != r.dataSource {
		if c := r.diag.sourceDrops.Add(1); r.debug && (c <= 5 || c%100 == 0) {
			r.log.Printf("audio: debug drop source #%d from=%v chosen=%v", c, from, r.dataSource)
		}
		r.decMu.Unlock()
		return
	}
	if len(pkt) <= HeaderLen || pkt[0]>>6 != 2 {
		if c := r.diag.shortDrops.Add(1); r.debug && (c <= 5 || c%100 == 0) {
			r.log.Printf("audio: debug drop runt #%d from=%v len=%d first=%#x", c, from, len(pkt), pkt[0])
		}
		r.decMu.Unlock()
		return
	}
	if control {
		// Only requested gaps in the current stream can accept a retransmit.
		seq := binary.BigEndian.Uint16(pkt[2:4])
		if len(r.repair.packets) == 0 || int16(seq-r.lastSeq) <= 0 || int16(seq-r.repair.furthest) >= 0 {
			r.decMu.Unlock()
			return
		}
	}
	var frames []*decode.AudioFrame
	if !control && !r.dataSource.IsValid() {
		if f := r.consumePacket(pkt); f != nil {
			r.dataSource = from
			frames = append(frames, f)
		} else {
			// A candidate source port that produced no audio cannot choose
			// the sequence baseline for the legitimate first packet.
			r.haveLast = false
		}
	} else {
		frames = r.orderPacket(pkt)
	}
	r.decMu.Unlock()
	r.observe(frames)
}

func (r *Receiver) observe(frames []*decode.AudioFrame) {
	if r.handlers.OnFrame != nil {
		for _, f := range frames {
			r.handlers.OnFrame(f)
		}
	}
}

func (r *Receiver) orderPacket(pkt []byte) []*decode.AudioFrame {
	seq := binary.BigEndian.Uint16(pkt[2:4])
	ahead := int16(seq - r.lastSeq)
	if r.haveLast && ahead > 1 && ahead <= maxRepairGap && r.remoteControl.IsValid() {
		if r.repair.packets == nil {
			r.repair.packets = make(map[uint16][]byte, maxRepairGap)
		}
		if _, duplicate := r.repair.packets[seq]; duplicate {
			r.mu.Lock()
			r.packets++
			r.dropped++
			r.mu.Unlock()
			return nil
		}
		r.repair.packets[seq] = append([]byte(nil), pkt...)
		if r.repair.timer == nil {
			r.requestRepair(r.lastSeq+1, uint16(ahead-1))
			r.repair.furthest = seq
			id := r.repair.timerID
			r.repair.timer = time.AfterFunc(latePacketMargin/2, func() { r.expireRepair(id) })
		} else if extra := int16(seq - r.repair.furthest); extra > 0 {
			if extra > 1 {
				r.requestRepair(r.repair.furthest+1, uint16(extra-1))
			}
			r.repair.furthest = seq
		}
		return nil
	}
	var frames []*decode.AudioFrame
	if r.haveLast && ahead > maxRepairGap {
		frames = r.drainRepair(frames, true)
	}
	if f := r.consumePacket(pkt); f != nil {
		frames = append(frames, f)
	}
	return r.drainRepair(frames, false)
}

func (r *Receiver) drainRepair(frames []*decode.AudioFrame, skipGaps bool) []*decode.AudioFrame {
	for len(r.repair.packets) > 0 {
		next := r.lastSeq + 1
		if _, found := r.repair.packets[next]; !found {
			if !skipGaps {
				break
			}
			var distance uint16
			for seq := range r.repair.packets {
				if d := seq - r.lastSeq; distance == 0 || d < distance {
					next, distance = seq, d
				}
			}
		}
		pkt := r.repair.packets[next]
		delete(r.repair.packets, next)
		if f := r.consumePacket(pkt); f != nil {
			frames = append(frames, f)
		}
	}
	if len(r.repair.packets) == 0 && r.repair.timer != nil {
		r.cancelRepair()
	}
	return frames
}

func (r *Receiver) expireRepair(id uint64) {
	r.decMu.Lock()
	if id != r.repair.timerID || r.closed {
		r.decMu.Unlock()
		return
	}
	frames := r.drainRepair(nil, true)
	r.decMu.Unlock()
	r.observe(frames)
}

func (r *Receiver) requestRepair(first, count uint16) {
	if r.control == nil {
		return
	}
	var req [8]byte
	req[0], req[1] = 0x80, 0x80|typeResendReq
	r.repair.requestSeq++
	binary.BigEndian.PutUint16(req[2:4], r.repair.requestSeq)
	binary.BigEndian.PutUint16(req[4:6], first)
	binary.BigEndian.PutUint16(req[6:8], count)
	_, _ = r.control.WriteToUDPAddrPort(req[:], r.remoteControl)
}
