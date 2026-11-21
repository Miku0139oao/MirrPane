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
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/miku0139/airplay-go/backend/internal/decode"
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
// hashed with the ECDH secret; several candidates are offered and the first
// that decrypts to a plausible AAC frame wins.
type Key struct {
	Name string
	Key  []byte
}

// Handlers receive what the audio path produces.
type Handlers struct {
	// OnFrame is called for each decoded access unit, in stream order.
	OnFrame func(*decode.AudioFrame)
	// OnSync is called when the sender reports its RTP/NTP mapping.
	OnSync func(rtpTimestamp uint32, ntp uint64, nextRTP uint32)
	// OnStats collects packet counts for the UI.
	OnStats func(received, dropped, decoded uint64)
}

// Receiver owns the two UDP sockets the sender streams to.
type Receiver struct {
	log      *log.Logger
	handlers Handlers

	data        *net.UDPConn
	control     *net.UDPConn
	dataPort    int
	controlPort int

	mu      sync.Mutex
	keys    []Key
	iv      []byte
	chosen  int // index of the key that produced a plausible frame, -1 until known
	packets uint64
	decoded uint64
	dropped uint64

	lastSeq  uint16
	haveLast bool

	dec *decode.AudioDecoder
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

// Configure sets the key material for the next stream. A fresh stream resets
// the sequence history and the key choice.
func (r *Receiver) Configure(keys []Key, iv []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append([]Key(nil), keys...)
	r.iv = append([]byte(nil), iv...)
	r.chosen = -1
	r.haveLast = false
	if r.dec != nil {
		r.dec.Close()
		if d, err := decode.NewAudioDecoder(nil); err == nil {
			r.dec = d
		}
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.haveLast = false
	if r.dec != nil {
		r.dec.Close()
		if d, err := decode.NewAudioDecoder(nil); err == nil {
			r.dec = d
		} else {
			r.dec = nil
		}
	}
}

// Stats reports packet and frame counts.
func (r *Receiver) Stats() (received, dropped, decoded uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.packets, r.dropped, r.decoded
}

// Close releases the sockets and the decoder.
func (r *Receiver) Close() {
	_ = r.data.Close()
	_ = r.control.Close()
	r.mu.Lock()
	if r.dec != nil {
		r.dec.Close()
		r.dec = nil
	}
	r.mu.Unlock()
}

func (r *Receiver) readData() {
	buf := make([]byte, 2048)
	for {
		n, _, err := r.data.ReadFromUDP(buf)
		if err != nil {
			return
		}
		r.handlePacket(buf[:n])
	}
}

func (r *Receiver) readControl() {
	buf := make([]byte, 2048)
	for {
		n, _, err := r.control.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 2 {
			continue
		}
		kind := buf[1] &^ 0x80
		switch kind {
		case typeSync:
			r.handleSync(buf[:n])
		case typeResend:
			// The resent packet is a complete RTP packet at offset 4.
			if n > 4 {
				r.handlePacket(buf[4:n])
			}
		}
	}
}

// handleSync records the sender's RTP-to-NTP mapping, which is what A/V sync
// is built on. Layout from raop_rtp.c:508-515.
func (r *Receiver) handleSync(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	rtp := binary.BigEndian.Uint32(pkt[4:8])
	ntp := binary.BigEndian.Uint64(pkt[8:16])
	next := binary.BigEndian.Uint32(pkt[16:20])
	if r.handlers.OnSync != nil {
		r.handlers.OnSync(rtp, ntp, next)
	}
}

// handlePacket decrypts and decodes one RTP audio packet.
func (r *Receiver) handlePacket(pkt []byte) {
	if len(pkt) < HeaderLen {
		return
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
		return
	}
	if len(payload) == 0 {
		return
	}

	seq := binary.BigEndian.Uint16(pkt[2:4])
	r.mu.Lock()
	// Each packet is sent three times. Sequence numbers only move forward, so a
	// repeat or a late arrival is dropped.
	if r.haveLast && int16(seq-r.lastSeq) <= 0 {
		r.dropped++
		r.mu.Unlock()
		return
	}
	r.lastSeq = seq
	r.haveLast = true
	r.mu.Unlock()

	plain, ok := r.decrypt(payload)
	if !ok {
		return
	}

	r.mu.Lock()
	dec := r.dec
	r.mu.Unlock()
	if dec == nil {
		return
	}

	frame, err := dec.Decode(plain)
	if err != nil {
		if err != decode.ErrNoFrame {
			r.log.Printf("audio: decode: %v", err)
		}
		return
	}
	r.mu.Lock()
	r.decoded++
	r.mu.Unlock()
	if r.handlers.OnFrame != nil {
		r.handlers.OnFrame(frame)
	}
}

// decrypt applies AES-128-CBC to one payload the way raop_buffer.c:130-136
// does: only the block-aligned prefix is encrypted, any trailing remainder is
// passed through, and the CBC state restarts from the fixed IV every packet.
func (r *Receiver) decrypt(payload []byte) ([]byte, bool) {
	r.mu.Lock()
	keys := r.keys
	iv := r.iv
	chosen := r.chosen
	r.mu.Unlock()

	if len(keys) == 0 || len(iv) != aes.BlockSize {
		return nil, false
	}

	if chosen >= 0 {
		out := cbcDecrypt(keys[chosen].Key, iv, payload)
		if plausibleAAC(out) {
			return out, true
		}
		return nil, false
	}

	// First packet: try each candidate and keep the one that yields a plausible
	// AAC frame. A wrong key produces bytes that cannot start a frame.
	for i, k := range keys {
		out := cbcDecrypt(k.Key, iv, payload)
		if plausibleAAC(out) {
			r.mu.Lock()
			r.chosen = i
			r.mu.Unlock()
			r.log.Printf("audio: key candidate %q decrypts to % x", k.Name, firstBytes(out, 4))
			return out, true
		}
	}
	r.log.Printf("audio: no key candidate produced a plausible frame (% x)", firstBytes(payload, 4))
	return nil, false
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

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
