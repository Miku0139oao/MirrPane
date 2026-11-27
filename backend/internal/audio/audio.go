// Package audio receives the AirPlay mirrored-audio UDP streams, decrypts them
// and decodes AAC-ELD.
//
// The wire format follows UxPlay (GPLv3), lib/raop_rtp.c and lib/raop_buffer.c.
// Two sockets are involved:
//
//	data    RTP audio packets, 12-byte header + AES-128-CBC encrypted payload
//	control sync packets (type 0x54) and retransmissions (type 0x56)
//
// A sender repeats every AAC-ELD packet three times, so duplicate sequence
// numbers are normal and must be dropped, not decoded.
package audio

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Miku0139oao/MirrPane/backend/internal/decode"
)

// HeaderLen is the RTP header every audio packet starts with.
const HeaderLen = 12

// Payload types seen on the control channel.
const (
	typeSync      = 0x54
	typeResend    = 0x56
	typeResendReq = 0x55
)

// Key is one candidate AES key. When a sender paired, the setup key is
// hashed with the ECDH secret. The decoder confirms the candidate by producing PCM.
type Key struct {
	Name string
	Key  []byte
}

// Handlers receive what the audio path produces.
type Handlers struct {
	// Output writes PCM to the playback queue in session order. It is serialized
	// with reset and must not call back into the receiver.
	Output func(*decode.AudioFrame)
	// OnFrame is called for each decoded access unit, in stream order.
	OnFrame func(*decode.AudioFrame)
	// OnSync is called when the sender reports its RTP/NTP mapping.
	OnSync func(rtpTimestamp uint32, ntp uint64, nextRTP uint32)
	// OnStats collects packet counts for the UI.
	OnStats func(received, dropped, decoded uint64)
}

type audioDecoder interface {
	Decode([]byte) (*decode.AudioFrame, error)
	Configure([]byte) error
	Flush()
	Close()
}

// diagCounters are the socket-level audio counters. They are read without the
// receiver lock (atomics), so /api/state never waits behind decoding.
type diagCounters struct {
	dataRx          atomic.Uint64
	controlRx       atomic.Uint64
	peerDrops       atomic.Uint64 // source address is not the authenticated peer
	shortDrops      atomic.Uint64 // runt or non-RTP packet
	sourceDrops     atomic.Uint64 // data from a port other than the chosen source
	controlRTPDrops atomic.Uint64 // RTP audio sent to the control socket
}

// Diagnostics is one snapshot of the socket counters.
type Diagnostics struct {
	DataRx          uint64
	ControlRx       uint64
	PeerDrops       uint64
	ShortDrops      uint64
	SourceDrops     uint64
	ControlRTPDrops uint64
}

// Diagnostics reports the audio UDP socket counters.
func (r *Receiver) Diagnostics() Diagnostics {
	return Diagnostics{
		DataRx:          r.diag.dataRx.Load(),
		ControlRx:       r.diag.controlRx.Load(),
		PeerDrops:       r.diag.peerDrops.Load(),
		ShortDrops:      r.diag.shortDrops.Load(),
		SourceDrops:     r.diag.sourceDrops.Load(),
		ControlRTPDrops: r.diag.controlRTPDrops.Load(),
	}
}

// packetType reports an RTP/control payload type for logging, 0 for runts.
func packetType(pkt []byte) byte {
	if len(pkt) < 2 {
		return 0
	}
	return pkt[1] & 0x7f
}

// Receiver owns the two UDP sockets the sender streams to.
type Receiver struct {
	log      *log.Logger
	handlers Handlers

	data        *net.UDPConn
	control     *net.UDPConn
	dataPort    int
	controlPort int

	// Peer binding, pending repairs and key probing are protected by decMu.
	peer                          netip.Addr
	peerLogged                    bool
	remoteControl                 netip.AddrPort
	controlSource                 netip.AddrPort
	dataSource                    netip.AddrPort
	repair                        repairQueue
	chosenFailures, probeFailures int
	trialKey                      int
	haveTrial                     bool
	nextProbe, lastDecodeLog      time.Time

	mu      sync.Mutex
	keys    []Key
	iv      []byte
	chosen  int // index of the key that decoded a frame, -1 until confirmed
	packets uint64
	decoded uint64
	dropped uint64
	// Only decoded frames refresh liveness; arbitrary UDP packets do not.
	configuredAt time.Time
	lastFrame    time.Time

	lastSeq     uint16
	haveLast    bool
	syncClock   rtpSync
	lateDropped uint64 // cumulative subset of dropped, logged at a sampled rate
	lastLateLog time.Time
	now         func() time.Time // nil uses the monotonic local clock; test seam

	// debug enables MIRRPANE_AUDIO_DEBUG datagram and drop logging.
	debug bool
	// diag counts every datagram each socket carried and why the data path
	// rejected it, so "nothing arrived" can be told apart from "arrived and
	// was dropped" from /api/state alone.
	diag diagCounters

	// decMu serializes packet processing and all decoder calls/replacement.
	// Lock order is decMu before mu; never hold mu while calling the decoder.
	// Frame callbacks run after both locks are released.
	decMu  sync.Mutex
	dec    audioDecoder
	closed bool
}

// New binds the data and control sockets and starts reading.
func New(logger *log.Logger, handlers Handlers) (*Receiver, error) {
	if logger == nil {
		logger = log.Default()
	}
	// Dual-stack: an iPhone frequently reaches the receiver over IPv6 and sends
	// RTP to this port family, so an IPv4-only socket would never see audio.
	data, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("audio: data socket: %w", err)
	}
	control, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		_ = data.Close()
		return nil, fmt.Errorf("audio: control socket: %w", err)
	}

	r := &Receiver{
		log:         logger,
		handlers:    handlers,
		data:        data,
		control:     control,
		dataPort:    data.LocalAddr().(*net.UDPAddr).Port,
		controlPort: control.LocalAddr().(*net.UDPAddr).Port,
		chosen:      -1,
		debug:       os.Getenv("MIRRPANE_AUDIO_DEBUG") == "1",
	}

	dec, err := decode.NewAudioDecoder(nil)
	if err != nil {
		// A missing decoder must not take the receiver down: video mirroring
		// still works, and the log names the reason.
		logger.Printf("audio: decoder unavailable: %v", err)
	} else {
		r.dec = dec
	}

	go r.readData()
	go r.readControl()
	return r, nil
}

// Ports reports the socket the sender must stream to and the control port it
// must advertise, in the order /setup wants them.
func (r *Receiver) Ports() (data, control int) { return r.dataPort, r.controlPort }

// Configure sets decoder key material without admitting any network peer.
// ConfigurePeer is used for authenticated network streams.
func (r *Receiver) Configure(keys []Key, iv []byte, config ...[]byte) {
	r.ConfigurePeer(netip.Addr{}, 0, keys, iv, config...)
}

// ConfigurePeer binds the incoming UDP stream to the peer authenticated by
// SETUP. The sender's negotiated control port is also the resend destination.
func (r *Receiver) ConfigurePeer(peer netip.Addr, controlPort int, keys []Key, iv []byte, config ...[]byte) {
	r.decMu.Lock()
	if r.closed {
		r.decMu.Unlock()
		return
	}
	r.clearIngress()
	r.peer = peer.Unmap()
	r.peerLogged = false
	if r.peer.IsValid() && controlPort > 0 && controlPort <= 65535 {
		r.remoteControl = netip.AddrPortFrom(r.peer, uint16(controlPort))
	}
	r.mu.Lock()
	r.keys = append([]Key(nil), keys...)
	r.iv = append([]byte(nil), iv...)
	r.chosen = -1
	r.haveLast = false
	r.syncClock = rtpSync{}
	r.configuredAt, r.lastFrame = time.Now(), time.Time{}
	r.mu.Unlock()
	var asc []byte
	if len(config) > 0 {
		asc = config[0]
	}
	var err error
	if r.dec != nil {
		err = r.dec.Configure(asc)
	} else {
		var d *decode.AudioDecoder
		d, err = decode.NewAudioDecoder(asc)
		if err == nil {
			r.dec = d
		}
	}
	r.decMu.Unlock()
	if err != nil {
		r.log.Printf("audio: configure decoder: %v", err)
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.Name
	}
	r.log.Printf("audio: %d key candidate(s): %v", len(keys), names)
}

// ResetStream clears sequence-number tracking for a new audio stream (for
// example after TEARDOWN + SETUP without new keys) so a fresh RTP sequence
// base is not mistaken for old duplicate packets.
func (r *Receiver) ResetStream() {
	r.resetStream(false)
}

// ResetKey releases the sender's audio keys as well as its decoder buffers.
func (r *Receiver) ResetKey() {
	r.resetStream(true)
}

func (r *Receiver) resetStream(clearKeys bool) {
	r.decMu.Lock()
	defer r.decMu.Unlock()
	if r.closed {
		return
	}
	r.clearIngress()
	r.mu.Lock()
	r.haveLast = false
	r.syncClock = rtpSync{}
	r.configuredAt, r.lastFrame = time.Time{}, time.Time{}
	if clearKeys {
		r.keys, r.iv = nil, nil
		r.chosen = -1
	}
	r.mu.Unlock()
	if r.dec != nil {
		r.dec.Flush()
	}
}

// Busy protects a live audio-only sender too. A setup gets a short start-up
// window, after which only successfully decoded audio keeps the reservation.
func (r *Receiver) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.lastFrame
	if last.IsZero() {
		last = r.configuredAt
	}
	return len(r.keys) > 0 && len(r.iv) == aes.BlockSize && time.Since(last) < 4*time.Second
}

// Stats reports packet and frame counts. Dropped includes duplicates,
// out-of-order packets and packets past their RTP playout deadline.
func (r *Receiver) Stats() (received, dropped, decoded uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.packets, r.dropped, r.decoded
}

// LateDropped reports how many of the dropped packets arrived after their RTP
// playout deadline, which is what a sender's catch-up burst looks like.
func (r *Receiver) LateDropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lateDropped
}

// Close releases the sockets and the decoder.
func (r *Receiver) Close() {
	_ = r.data.Close()
	_ = r.control.Close()
	r.decMu.Lock()
	r.clearIngress()
	r.closed = true
	if r.dec != nil {
		r.dec.Close()
		r.dec = nil
	}
	r.decMu.Unlock()
}

func (r *Receiver) readData() {
	buf := make([]byte, 2048)
	for {
		n, from, err := r.data.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if c := r.diag.dataRx.Add(1); r.debug && (c <= 5 || c%100 == 0) {
			r.log.Printf("audio: debug data rx #%d from=%v len=%d type=%#x", c, from, n, packetType(buf[:n]))
		}
		r.receiveDatagram(from, buf[:n], false)
	}
}

func (r *Receiver) readControl() {
	buf := make([]byte, 2048)
	for {
		n, from, err := r.control.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if c := r.diag.controlRx.Add(1); r.debug && (c <= 5 || c%100 == 0) {
			r.log.Printf("audio: debug control rx #%d from=%v len=%d type=%#x", c, from, n, packetType(buf[:n]))
		}
		r.receiveDatagram(from, buf[:n], true)
	}
}

// handleSync records the sender's RTP-to-NTP mapping, which is what A/V sync
// is built on. Layout from raop_rtp.c:508-515.
func (r *Receiver) handleSync(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	rtp, ntp, next := r.recordSync(pkt)
	if r.handlers.OnSync != nil {
		r.handlers.OnSync(rtp, ntp, next)
	}
}

func (r *Receiver) recordSync(pkt []byte) (uint32, uint64, uint32) {
	rtp := binary.BigEndian.Uint32(pkt[4:8])
	ntp := binary.BigEndian.Uint64(pkt[8:16])
	next := binary.BigEndian.Uint32(pkt[16:20])
	receivedAt := r.clockNow()
	r.mu.Lock()
	r.syncClock = rtpSync{rtp: rtp, ntp: ntp, localTime: receivedAt, valid: true}
	r.mu.Unlock()
	return rtp, ntp, next
}

// handlePacket decrypts and decodes one RTP audio packet.
func (r *Receiver) handlePacket(pkt []byte) {
	r.decMu.Lock()
	frame := r.consumePacket(pkt)
	r.decMu.Unlock()
	if frame != nil && r.handlers.OnFrame != nil {
		r.handlers.OnFrame(frame)
	}
}

func (r *Receiver) consumePacket(pkt []byte) *decode.AudioFrame {
	if len(pkt) < HeaderLen || r.closed {
		return nil
	}
	r.mu.Lock()
	r.packets++
	first := r.packets == 1
	r.mu.Unlock()
	if first {
		// The decisive line for "is there no sound because nothing arrived, or
		// because it could not be decrypted".
		r.log.Printf("audio: first RTP packet (%d bytes)", len(pkt))
	}

	payload := pkt[HeaderLen:]

	// At stream start the sender sends a 4-byte no-data marker; it is not audio.
	if len(payload) == 4 && payload[0] == 0x00 && payload[1] == 0x68 && payload[2] == 0x34 && payload[3] == 0x00 {
		return nil
	}
	if len(payload) == 0 {
		return nil
	}

	seq := binary.BigEndian.Uint16(pkt[2:4])
	rtp := binary.BigEndian.Uint32(pkt[4:8])
	frame, err := r.decodePacketLocked(seq, rtp, payload)
	if err != nil {
		now := r.clockNow()
		if !errors.Is(err, decode.ErrNoFrame) && (r.lastDecodeLog.IsZero() || now.Sub(r.lastDecodeLog) >= lateDropLogInterval) {
			r.lastDecodeLog = now
			r.log.Printf("audio: decode: %v", err)
		}
		return nil
	}
	if frame != nil && r.handlers.Output != nil {
		r.handlers.Output(frame)
	}
	return frame
}

// decodePacket keeps stream state, decryption and decoding together so a reset
// cannot move a packet decrypted with the old keys into the new decoder state.
func (r *Receiver) decodePacket(seq uint16, rtp uint32, payload []byte) (*decode.AudioFrame, error) {
	r.decMu.Lock()
	defer r.decMu.Unlock()
	return r.decodePacketLocked(seq, rtp, payload)
}

func (r *Receiver) decodePacketLocked(seq uint16, rtp uint32, payload []byte) (*decode.AudioFrame, error) {
	if r.closed {
		return nil, nil
	}
	r.mu.Lock()
	// Each packet is sent three times. Sequence numbers only move forward, so a
	// repeat or a late arrival is dropped.
	if r.haveLast && int16(seq-r.lastSeq) <= 0 {
		r.dropped++
		r.mu.Unlock()
		return nil, nil
	}
	r.lastSeq = seq
	r.haveLast = true
	now := r.clockNow()
	if expected, fresh := r.syncClock.expected(now); fresh {
		delta := int32(rtp - expected)
		if delta < -latePacketMarginTicks {
			r.dropped++
			r.lateDropped++
			count := r.lateDropped
			logDrop := count == 1 || now.Sub(r.lastLateLog) >= lateDropLogInterval
			if logDrop {
				r.lastLateLog = now
			}
			syncAge := now.Sub(r.syncClock.localTime)
			r.mu.Unlock()
			if logDrop {
				r.log.Printf("audio: late RTP dropped (lateDropped=%d, behind=%.1f ms, syncAge=%v)",
					count, float64(-int64(delta))*1000/rtpClockRate, syncAge)
			}
			return nil, nil
		}
	}
	r.mu.Unlock()

	frame, err := r.decodeWithKey(payload, now)
	if err != nil || frame == nil {
		return nil, err
	}
	// Decode's PCM buffer is reused. Own the delivered frame before releasing
	// decMu so another packet, flush or close cannot change a callback's data.
	owned := *frame
	owned.Samples = append([]int16(nil), frame.Samples...)
	r.mu.Lock()
	r.decoded++
	r.lastFrame = time.Now()
	r.mu.Unlock()
	return &owned, nil
}

var errNoAudioKey = errors.New("no configured key decoded this audio frame")

// Only decoded PCM confirms a candidate. A guessed prefix is a cheap filter,
// and ambiguous packets must not permanently choose the wrong compatibility key.
func (r *Receiver) decodeWithKey(payload []byte, now time.Time) (*decode.AudioFrame, error) {
	if r.dec == nil || len(r.keys) == 0 || len(r.iv) != aes.BlockSize || now.Before(r.nextProbe) {
		return nil, nil
	}
	try := func(i int) (*decode.AudioFrame, error) {
		plain := cbcDecrypt(r.keys[i].Key, r.iv, payload)
		if !plausibleAAC(plain) {
			return nil, errNoAudioKey
		}
		if r.haveTrial && r.trialKey != i {
			r.dec.Flush()
		}
		r.trialKey, r.haveTrial = i, true
		f, err := r.dec.Decode(plain)
		if err == nil && (f == nil || f.Frames() == 0) {
			err = decode.ErrNoFrame
		}
		return f, err
	}
	if r.chosen >= 0 {
		frame, err := try(r.chosen)
		if err == nil {
			r.chosenFailures = 0
			return frame, nil
		}
		r.chosenFailures++
		limit := 3
		if errors.Is(err, decode.ErrNoFrame) {
			limit = 8 // tolerate codec priming before reconsidering this choice
		}
		if r.chosenFailures >= limit {
			r.chosen, r.chosenFailures = -1, 0
			r.haveTrial = false
			r.dec.Flush()
		}
		return nil, err
	}
	var lastErr error
	for i := range r.keys {
		frame, err := try(i)
		if err == nil {
			r.chosen, r.probeFailures = i, 0
			r.log.Printf("audio: decoder confirmed key candidate %q", r.keys[i].Name)
			return frame, nil
		}
		lastErr = err
	}
	if !errors.Is(lastErr, decode.ErrNoFrame) {
		r.probeFailures++
		if r.probeFailures >= 3 {
			r.nextProbe = now.Add(100 * time.Millisecond)
			r.probeFailures = 0
		}
	}
	return nil, lastErr
}

func cbcDecrypt(key, iv, payload []byte) []byte {
	encLen := len(payload) / aes.BlockSize * aes.BlockSize
	out := make([]byte, len(payload))
	if encLen > 0 {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil
		}
		// A fresh decrypter per packet: the CBC state must not carry across
		// packets, matching aes_cbc_reset in UxPlay.
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out[:encLen], payload[:encLen])
	}
	copy(out[encLen:], payload[encLen:])
	return out
}

// plausibleAAC reports whether b could begin an AAC-ELD or AAC-LC frame.
// UxPlay checks the same first bytes (renderers/audio_renderer.c:345-366).
func plausibleAAC(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	switch b[0] {
	case 0x8c, 0x8d, 0x8e, // AAC-ELD, modern iOS
		0x80, 0x81, 0x82, // AAC-ELD, iOS 9/10
		0x20, 0x21, 0x22: // ALAC
		return true
	}
	return false
}
