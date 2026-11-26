// Package mirror receives the AirPlay screen-mirroring TCP stream, decrypts it
// and hands out H.264/H.265 access units in Annex-B form.
//
// The wire format and the key derivation follow UxPlay (GPLv3),
// lib/raop_rtp_mirror.c, lib/mirror_buffer.c and lib/byteutils.c. Byte order is
// mixed and matters:
//
//	payload length (header 0:3)   little endian
//	NTP timestamp (header 8:15)   big endian
//	duration     (header 64:71)   big endian
//	NAL length prefixes           big endian
//	parameter-set lengths         big endian
//	geometry floats               host order (little endian on x86)
package mirror

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/netip"
	"sync"
	"time"
)

// HeaderLen is the fixed header every mirror payload is prefixed with.
const HeaderLen = 128

// maxPayload bounds one packet. A 4K IDR frame is well under this; anything
// larger is a desynchronised stream, not a frame.
const maxPayload = 8 << 20

const (
	pendingLifetime = 8 * time.Second
	streamIdleTime  = 4 * time.Second
	maxPendingPeers = 16
)

// reportTrailer is the size of the opaque trailer some streaming reports carry
// when the sender's screen is locked. The plist occupies the front of the body.
const reportTrailer = 25000

// AnnexBStartCode begins every NAL unit handed to the decoder.
var AnnexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// Codec identifies the video codec the sender chose.
type Codec int

// The two codecs AirPlay mirroring uses.
const (
	CodecUnknown Codec = iota
	CodecH264
	CodecH265
)

func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	default:
		return "unknown"
	}
}

// Frame is one decodable access unit.
type Frame struct {
	// Data is the access unit in Annex-B byte-stream form, including the
	// parameter sets when they were prepended.
	Data []byte
	// NTP is the sender's timestamp: naive little-endian read of header 8:15,
	// exactly as UxPlay decodes it, so it compares equal to the value the
	// parameter-set packet stored.
	NTP uint64
	// Duration is the frame duration from the header, in nanoseconds.
	Duration uint64
	// Codec is the codec the sender is using.
	Codec Codec
	// Valid is false when the payload failed its integrity checks, which in
	// practice means the AES key was wrong.
	Valid bool
	// Session is the connection generation that produced this access unit.
	// Consumers must ignore pictures whose generation no longer owns the
	// stream, so a stopped session cannot publish stale media.
	Session uint64
}

// Size is the source display geometry announced in unencrypted packets.
// Offsets describe what the sender cropped away on the device.
type Size struct {
	Width, Height             int
	XOffset, YOffset          int
	SourceWidth, SourceHeight int
}

// Handlers receive what the stream produces.
type Handlers struct {
	// OnFrame is called for every decodable access unit.
	OnFrame func(*Frame)
	// OnGeometry is called when the sender announces or changes the display size.
	OnGeometry func(Size)
	// OnPause is called when the sender suspends the video stream (device asleep).
	OnPause func()
	// OnResume is called when the stream comes back after a pause.
	OnResume func()
	// OnReport is called with each streaming-report plist payload.
	OnReport func([]byte)
	// OnConnected is called when a sender opens the stream connection. The
	// session generation identifies the connection; later connect, frame and
	// disconnect callbacks carry the same value so an out-of-order callback
	// can never change state owned by a newer connection.
	OnConnected func(session uint64, remote string)
	// OnDisconnected is called when the sender closes the session that still
	// owns the stream. A superseded connection closing is not reported.
	OnDisconnected func(session uint64)
	// OnStats collects byte counts for the UI.
	OnStats func(received uint64, frames uint64)
}

// KeyCandidate is one possible FairPlay audio key. UxPlay hashes that key with
// the pairing ECDH secret when pairing took place; a sender that expects the
// unhashed key would otherwise produce undecryptable frames, so the stream tries
// each candidate on its first encrypted packet and keeps the one that yields
// well-formed NAL framing. The stream key and IV are derived from Key.
type KeyCandidate struct {
	Name string
	Key  []byte
}

// resolvedKey is a stream key and IV already derived from a KeyCandidate.
type resolvedKey struct {
	Name string
	Key  []byte
	IV   []byte
}

// pendingStream binds the keys and stream ID to one authenticated /setup.
// A TCP peer cannot supply its own stream ID; it must use these derived keys.
// generation is the token /setup handed to the receiver; the connection that
// commits this setup reports that token in its callbacks.
type pendingStream struct {
	sender     netip.Addr
	streamID   uint64
	generation uint64
	expires    time.Time
	candidates []resolvedKey
}

type streamAttempt struct {
	pending *pendingStream
	at      time.Time
	sender  netip.Addr
}

// Server listens for the mirror stream.
type Server struct {
	log      *log.Logger
	handlers Handlers

	ln   net.Listener
	port int

	mu          sync.Mutex
	pending     *pendingStream
	attempts    map[net.Conn]streamAttempt
	conn        net.Conn
	sender      netip.Addr
	setupSender netip.Addr
	streamID    uint64
	session     uint64 // generation owning conn; 0 when idle
	nextSession uint64
	connectedAt time.Time
	lastFrame   time.Time

	received uint64
	frames   uint64

	closeOnce sync.Once
	closed    chan struct{}
}

// NewServer creates a server that has not bound a port yet.
func NewServer(logger *log.Logger, handlers Handlers) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{log: logger, handlers: handlers, closed: make(chan struct{})}
}

// Start binds an ephemeral TCP port on every interface and begins accepting.
// The port is what /setup reports as the mirror data port.
//
// The address is left empty rather than pinned to 0.0.0.0 so the socket is
// dual-stack: mDNS publishes the interface's AAAA record alongside its A record,
// and a sender that resolved the IPv6 address would otherwise find nothing
// listening. The control port has the same property, so a sender that reached it
// over IPv6 expects the mirror port to answer there too.
func (s *Server) Start() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("mirror: listen: %w", err)
	}
	s.ln = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	go s.acceptLoop()
	return s.port, nil
}

// Port reports the bound port.
func (s *Server) Port() int { return s.port }

// logf logs through the server's logger, tolerating a nil one so that tests can
// build a Server without a logger.
func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

// DropClient closes the active mirror connection without shutting down the
// listener, allowing a new sender to connect immediately.
func (s *Server) DropClient() {
	s.mu.Lock()
	c := s.conn
	attempts := s.attempts
	s.conn = nil
	s.pending = nil
	s.attempts = nil
	s.sender = netip.Addr{}
	s.setupSender = netip.Addr{}
	s.streamID = 0
	s.session = 0
	s.connectedAt, s.lastFrame = time.Time{}, time.Time{}
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	for peer := range attempts {
		_ = peer.Close()
	}
}

// Close stops listening and drops the active stream.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.ln != nil {
			err = s.ln.Close()
		}
		s.DropClient()
	})
	return err
}

// DeriveStreamKey computes the AES-128 key and IV that protect the mirror
// stream. The audio key comes from the FairPlay exchange; the stream connection
// ID is chosen by the sender in /setup.
//
// UxPlay, lib/mirror_buffer.c mirror_buffer_init_aes, hashes with sha_init(),
// which is SHA-512 (lib/crypto.c), and keeps the first 16 bytes:
//
//	SHA512("AirPlayStreamKey" + decimal(id) || audioKey)[:16]
//	SHA512("AirPlayStreamIV"  + decimal(id) || audioKey)[:16]
//
// Using SHA-256 here produces a plausible-looking key that decrypts to noise.
func DeriveStreamKey(streamConnectionID uint64, audioKey []byte) (key, iv [16]byte) {
	k := sha512.New()
	fmt.Fprintf(k, "AirPlayStreamKey%d", streamConnectionID)
	k.Write(audioKey)
	copy(key[:], k.Sum(nil))

	ivh := sha512.New()
	fmt.Fprintf(ivh, "AirPlayStreamIV%d", streamConnectionID)
	ivh.Write(audioKey)
	copy(iv[:], ivh.Sum(nil))
	return key, iv
}

// Prepare sets the key material for the next stream connection and returns the
// session generation that connection will report. It must be called before
// /setup replies with this server's port, because the sender connects as soon
// as it has the reply.
//
// The first candidate is the one the protocol calls for; the rest are fallbacks
// the stream tests against the first encrypted packet.
// It refuses a different sender while a live stream or fresh setup owns the
// receiver. The same sender can always prepare a legitimate reconnect.
func (s *Server) Prepare(remote string, streamConnectionID uint64, audioKeys ...KeyCandidate) (uint64, bool) {
	sender, err := netip.ParseAddr(remote)
	if err != nil || len(audioKeys) == 0 {
		return 0, false
	}
	pending := &pendingStream{sender: sender.Unmap(), streamID: streamConnectionID}
	for _, c := range audioKeys {
		key, iv := DeriveStreamKey(streamConnectionID, c.Key)
		name := c.Name
		if name == "" {
			name = "stream"
		}
		pending.candidates = append(pending.candidates, resolvedKey{Name: name, Key: key[:], IV: iv[:]})
	}
	s.mu.Lock()
	now := time.Now()
	if (s.conn != nil && s.setupSender != pending.sender && !s.staleLocked(now)) ||
		(s.pending != nil && s.pending.sender != pending.sender && now.Before(s.pending.expires)) {
		s.mu.Unlock()
		return 0, false
	}
	s.nextSession++
	pending.generation = s.nextSession
	oldAttempts := s.attempts
	s.attempts = nil
	pending.expires = now.Add(pendingLifetime)
	s.pending = pending
	s.mu.Unlock()
	for peer := range oldAttempts {
		_ = peer.Close()
	}
	return pending.generation, true
}

// ResetKey forgets the key so a stale stream cannot be decrypted with it.
func (s *Server) ResetKey() {
	s.mu.Lock()
	s.pending = nil
	attempts := s.attempts
	s.attempts = nil
	s.mu.Unlock()
	for peer := range attempts {
		_ = peer.Close()
	}
}

// Busy reports a live stream or a fresh pending setup. A raw TCP connection
// never refreshes liveness: only a successfully decrypted video frame does.
func (s *Server) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	return (s.conn != nil && !s.staleLocked(now)) ||
		(s.pending != nil && now.Before(s.pending.expires))
}

func (s *Server) staleLocked(now time.Time) bool {
	last := s.lastFrame
	if last.IsZero() {
		last = s.connectedAt
	}
	return now.Sub(last) >= streamIdleTime
}

func sourceIP(conn net.Conn) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	sender, err := netip.ParseAddr(host)
	return sender.Unmap(), err
}

// admit grants only a bounded decryption attempt, never ownership. A different
// data address is allowed while no healthy stream owns the slot; a same-address
// reconnect may also prove its new setup key while the old stream stays live.
func (s *Server) admit(conn net.Conn) *pendingStream {
	sender, err := sourceIP(conn)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	now := time.Now()
	pending := s.pending
	if pending != nil && !now.Before(pending.expires) {
		s.pending = nil
		pending = nil
	}
	if pending == nil ||
		(s.conn != nil && s.sender != sender && pending.sender != sender && !s.staleLocked(now)) {
		s.mu.Unlock()
		return nil
	}
	if s.attempts == nil {
		s.attempts = make(map[net.Conn]streamAttempt)
	}
	// Replace the same source immediately. At capacity, prefer the oldest
	// foreign source so alternate-family legitimate data peers also get a turn.
	var replaced net.Conn
	for peer, attempt := range s.attempts {
		if attempt.sender == sender {
			replaced = peer
			break
		}
	}
	if replaced == nil && len(s.attempts) >= maxPendingPeers {
		var oldest time.Time
		for peer, attempt := range s.attempts {
			if attempt.sender != pending.sender && (replaced == nil || attempt.at.Before(oldest)) {
				replaced, oldest = peer, attempt.at
			}
		}
		if replaced == nil {
			for peer, attempt := range s.attempts {
				if replaced == nil || attempt.at.Before(oldest) {
					replaced, oldest = peer, attempt.at
				}
			}
		}
	}
	delete(s.attempts, replaced)
	s.attempts[conn] = streamAttempt{pending: pending, at: now, sender: sender}
	_ = conn.SetReadDeadline(now.Add(streamIdleTime))
	s.mu.Unlock()
	if replaced != nil {
		_ = replaced.Close()
	}
	return pending
}

// acceptFrame commits a speculative connection only after a nonempty frame
// decrypts with this setup's stream-ID-derived key. Raw sockets cannot evict a
// healthy sender or consume its pending authorization.
func (s *Server) acceptFrame(st *streamState, valid bool) (accepted, connected bool) {
	s.mu.Lock()
	if s.conn == st.conn {
		s.mu.Unlock()
		return true, false
	}
	now := time.Now()
	sender, err := sourceIP(st.conn)
	if !valid || err != nil || s.attempts[st.conn].pending != st.pending || s.pending != st.pending ||
		!now.Before(st.pending.expires) ||
		(s.conn != nil && s.sender != sender && st.pending.sender != sender && !s.staleLocked(now)) {
		s.mu.Unlock()
		return false, false
	}
	old := s.conn
	attempts := s.attempts
	s.attempts, s.pending = nil, nil
	s.conn, s.sender, s.setupSender, s.streamID = st.conn, sender, st.pending.sender, st.pending.streamID
	s.session = st.pending.generation
	st.session = s.session
	s.connectedAt, s.lastFrame = now, now
	s.received, s.frames = st.received, 0
	// Established streams may suspend video when the phone locks its screen.
	_ = st.conn.SetReadDeadline(time.Time{})
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	for peer := range attempts {
		if peer != st.conn {
			_ = peer.Close()
		}
	}
	return true, true
}

// Stats reports bytes and access units seen since the stream opened.
func (s *Server) Stats() (bytes, frames uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received, s.frames
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(10 * time.Second)
		_ = tc.SetNoDelay(true)
	}

	remote := conn.RemoteAddr().String()
	pending := s.admit(conn)
	if pending == nil {
		s.logf("mirror: rejected connection from %s: no matching fresh setup", remote)
		return
	}
	s.logf("mirror: checking stream key from %s", remote)
	st := &streamState{srv: s, conn: conn, pending: pending, active: -1}
	defer func() {
		s.mu.Lock()
		current := s.conn == conn
		session := st.session
		delete(s.attempts, conn)
		if current {
			s.conn = nil
			s.sender = netip.Addr{}
			s.setupSender = netip.Addr{}
			s.streamID = 0
			s.session = 0
			s.connectedAt, s.lastFrame = time.Time{}, time.Time{}
			if s.pending == pending {
				s.pending = nil
			}
		}
		s.mu.Unlock()
		s.logf("mirror: sender disconnected (%s)", remote)
		if current && s.handlers.OnDisconnected != nil {
			// Only the generation that still owned the stream reports its end.
			// A superseded connection must not interrupt the new session.
			s.handlers.OnDisconnected(session)
		}
	}()

	for _, c := range pending.candidates {
		block, err := aes.NewCipher(c.Key)
		if err != nil {
			s.logf("mirror: candidate %q: %v", c.Name, err)
			continue
		}
		st.cands = append(st.cands, cipher.NewCTR(block, c.IV))
		st.names = append(st.names, c.Name)
	}
	if len(st.cands) == 0 {
		s.logf("mirror: no stream key prepared; encrypted packets cannot be read")
	}

	st.read(bufio.NewReaderSize(conn, 1<<20))
}

// streamState is the per-connection decode state.
type streamState struct {
	srv          *Server
	conn         net.Conn
	pending      *pendingStream
	received     uint64
	session      uint64 // generation of this connection once committed; guarded by srv.mu
	paramsHeader []byte

	// cands are the key candidates in preference order. active is the index of
	// the one that produced a well-formed first frame, or -1 until one is known.
	cands  []cipher.Stream
	names  []string
	active int

	codec     Codec
	codecSet  bool
	h265      bool
	spsPPS    []byte
	spsNTP    uint64
	haveSPS   bool
	suspended bool
}

func (st *streamState) read(r io.Reader) {
	header := make([]byte, HeaderLen)
	var scratch []byte
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				st.srv.logf("mirror: header read: %v", err)
			}
			return
		}
		// UxPlay reads the payload length little-endian (byteutils_get_int).
		n := binary.LittleEndian.Uint32(header[0:4])
		if n > maxPayload {
			st.srv.logf("mirror: payload length %d exceeds limit; dropping connection", n)
			return
		}
		if cap(scratch) < int(n) {
			scratch = make([]byte, n)
		}
		payload := scratch[:n]
		if n > 0 {
			if _, err := io.ReadFull(r, payload); err != nil {
				st.srv.logf("mirror: payload read: %v", err)
				return
			}
		}
		st.srv.mu.Lock()
		if st.conn != nil && st.srv.conn != st.conn && st.srv.attempts[st.conn].pending != st.pending {
			st.srv.mu.Unlock()
			return
		}
		st.received += uint64(HeaderLen + int(n))
		if st.conn == nil || st.srv.conn == st.conn {
			st.srv.received += uint64(HeaderLen + int(n))
		}
		st.srv.mu.Unlock()

		st.handle(header, payload)
	}
}

func (st *streamState) handle(header, payload []byte) {
	// UxPlay reads the timestamp with byteutils_get_long, which is a little
	// endian read (lib/byteutils.c). The duration field is big endian.
	ntp := binary.LittleEndian.Uint64(header[8:16])
	duration := binary.BigEndian.Uint64(header[64:72])

	switch header[4] {
	case 0x00:
		st.handleVideo(payload, ntp, duration)
	case 0x01:
		st.handleParams(header, payload, ntp)
	case 0x05:
		st.handleReport(payload)
	case 0x02:
		// Legacy once-per-second heartbeat with no payload.
	default:
		st.srv.logf("mirror: unexpected packet type %02x size %d", header[4], len(payload))
	}
}

// handleReport forwards the sender's performance report. When the device is
// locked it appends a 25 kB trailer that is not part of the plist.
func (st *streamState) handleReport(payload []byte) {
	if len(payload) == 0 || st.srv.handlers.OnReport == nil || !st.current() {
		return
	}
	plist := payload
	if len(payload) > reportTrailer {
		plist = payload[:len(payload)-reportTrailer]
	}
	st.srv.handlers.OnReport(plist)
}

// handleVideo decrypts a VCL packet, converts its length-prefixed NAL units to
// Annex-B and reports the result.
func (st *streamState) handleVideo(payload []byte, ntp, duration uint64) {
	// A pending SPS/PPS only belongs to this frame if the sender stamped them
	// with the same time; otherwise they are stale and must be dropped.
	if st.haveSPS && st.spsNTP != ntp {
		st.spsPPS = nil
		st.haveSPS = false
	}

	var body []byte
	valid := true
	switch {
	case len(st.cands) == 0:
		body = append([]byte(nil), payload...)
		valid = false
	case st.active < 0:
		// The first encrypted packet decides which key derivation this sender
		// uses. A wrong key cannot produce well-formed length prefixes, so the
		// test is reliable and costs one extra decrypt per candidate.
		body, valid = st.chooseKey(payload)
	default:
		body = make([]byte, len(payload))
		// The keystream runs continuously across packets: packet boundaries do
		// not reset the counter.
		st.cands[st.active].XORKeyStream(body, payload)
		valid = st.toAnnexB(body)
	}

	data := make([]byte, 0, len(st.spsPPS)+len(body))
	data = append(data, st.spsPPS...)
	data = append(data, body...)

	frame := &Frame{Data: data, NTP: ntp, Duration: duration, Codec: st.codec, Valid: valid}
	if st.haveSPS {
		st.spsPPS = nil
		st.haveSPS = false
	}
	connected := false
	if st.conn != nil {
		accepted, came := st.srv.acceptFrame(st, valid)
		if !accepted {
			return
		}
		connected = came
	}
	st.srv.deliver(st, frame, valid, connected)
}

// deliver reports a committed access unit to the handlers. The generation is
// re-checked immediately before each callback, so a callback delayed past a
// stop or a newer session is dropped instead of reviving a stopped connection.
// Connect always precedes the first frame of its generation because both are
// delivered from this one goroutine; consumers pair the generation with their
// own reset/ownership checks to order media against resets.
func (s *Server) deliver(st *streamState, frame *Frame, valid, connected bool) {
	if st.conn != nil {
		s.mu.Lock()
		current := s.conn == st.conn && st.session != 0 && st.session == s.session
		frame.Session = st.session
		s.mu.Unlock()
		if !current {
			return
		}
	}
	if connected {
		remote := st.conn.RemoteAddr().String()
		s.logf("mirror: sender connected from %s", remote)
		if s.handlers.OnConnected != nil {
			s.handlers.OnConnected(frame.Session, remote)
		}
		st.publishIfCurrent()
	}

	s.mu.Lock()
	if st.conn != nil && s.conn != st.conn {
		s.mu.Unlock()
		return
	}
	if valid {
		s.lastFrame = time.Now()
		if s.pending == st.pending {
			s.pending = nil
		}
		if st.conn != nil {
			_ = st.conn.SetReadDeadline(time.Time{})
		}
	}
	s.frames++
	s.mu.Unlock()
	if s.handlers.OnFrame != nil {
		s.handlers.OnFrame(frame)
	}
}

// chooseKey tries each key candidate against the first encrypted packet and
// locks in the first that yields well-formed NAL framing. If none do, the
// preferred candidate is kept so the stream continues with the protocol's
// documented behaviour and the failure is visible in the log.
func (st *streamState) chooseKey(payload []byte) (body []byte, valid bool) {
	var first []byte
	for i, stream := range st.cands {
		out := make([]byte, len(payload))
		stream.XORKeyStream(out, payload)
		if i == 0 {
			first = out
		}
		if len(out) >= 8 {
			st.srv.logf("mirror: candidate %q decrypts to % x", st.names[i], out[:8])
		}
		if st.toAnnexB(out) {
			st.active = i
			st.srv.logf("mirror: key candidate %q decoded the first frame", st.names[i])
			return out, true
		}
	}
	st.active = 0
	st.srv.logf("mirror: no key candidate produced valid framing (tried %v)", st.names)
	return first, false
}

// toAnnexB rewrites the length prefixes in place. It mirrors the validation in
// UxPlay: the unit must fit, and the forbidden bit must be clear.
//
// The prefix is big endian (byteutils_get_int_be); the sender's own encoder
// writes it as a little-endian length, which the reference reads through ntohl.
func (st *streamState) toAnnexB(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	off := 0
	for off < len(b) {
		if off+4 > len(b) {
			return false
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		if size <= 0 || size > len(b)-off-4 {
			return false
		}
		copy(b[off:off+4], AnnexBStartCode)
		head := off + 4
		if b[head]&0x80 != 0 {
			return false
		}
		off = head + size
	}
	return true
}

// handleParams stores SPS/PPS (or VPS/SPS/PPS) to prepend to the next frame.
func (st *streamState) handleParams(header, payload []byte, ntp uint64) {
	if len(payload) == 0 {
		return
	}

	if len(payload) >= 8 && string(payload[4:8]) == "hvc1" {
		st.setCodec(CodecH265)
		vps, sps, pps, ok := splitHEVC(payload)
		if !ok {
			st.srv.logf("mirror: malformed HEVC parameter-set packet")
			return
		}
		st.spsPPS = packParameterSets(vps, sps, pps)
	} else {
		st.setCodec(CodecH264)
		sps, pps, ok := splitAVC(payload)
		if !ok {
			st.srv.logf("mirror: malformed H.264 parameter-set packet")
			return
		}
		st.spsPPS = packParameterSets(sps, pps)
	}

	st.spsNTP = ntp
	st.haveSPS = true
	st.paramsHeader = append(st.paramsHeader[:0], header...)
	st.publishIfCurrent()
}

func (st *streamState) current() bool {
	st.srv.mu.Lock()
	defer st.srv.mu.Unlock()
	return st.conn == nil || (st.srv.conn == st.conn && st.session != 0 && st.session == st.srv.session)
}

// publishIfCurrent reports geometry and pause/resume only for the generation
// that still owns the stream.
func (st *streamState) publishIfCurrent() {
	if st.current() {
		st.publishParams()
	}
}

// Speculative peers may provide codec parameters, but cannot publish geometry
// or pause/resume events before a valid encrypted video frame proves the key.
func (st *streamState) publishParams() {
	header := st.paramsHeader
	if len(header) != HeaderLen {
		return
	}

	// The header carries the geometry; report it alongside the codec.
	if st.srv.handlers.OnGeometry != nil {
		st.srv.handlers.OnGeometry(Size{
			Width:        int(geometryFloat(header[56:60])),
			Height:       int(geometryFloat(header[60:64])),
			XOffset:      int(geometryFloat(header[48:52])),
			YOffset:      int(geometryFloat(header[52:56])),
			SourceWidth:  int(geometryFloat(header[16:20])),
			SourceHeight: int(geometryFloat(header[20:24])),
		})
	}

	// 0x56/0x5e mean the sender suspended video (screen off); 0x16/0x1e resume.
	switch header[6] {
	case 0x56, 0x5e:
		if !st.suspended {
			st.suspended = true
			if st.srv.handlers.OnPause != nil {
				st.srv.handlers.OnPause()
			}
		}
	case 0x16, 0x1e:
		if st.suspended {
			st.suspended = false
			if st.srv.handlers.OnResume != nil {
				st.srv.handlers.OnResume()
			}
		}
	}
}

func (st *streamState) setCodec(c Codec) {
	if st.codecSet && st.codec != c {
		st.srv.logf("mirror: sender switched codec %v -> %v; dropping", st.codec, c)
	}
	if st.codecSet {
		return
	}
	st.codec = c
	st.codecSet = true
	st.h265 = c == CodecH265
	st.srv.logf("mirror: sender uses %s", c)
}

// splitAVC extracts SPS and PPS from an unencrypted parameter-set payload.
//
// Layout, from UxPlay lib/raop_rtp_mirror.c:
//
//	[0:6]   header
//	[6:8]   SPS length, big endian
//	[8:8+n] SPS
//	[..+2]  PPS length, big endian at spsLen+9
//	[...]   PPS at spsLen+11
func splitAVC(payload []byte) (sps, pps []byte, ok bool) {
	if len(payload) < 11 {
		return nil, nil, false
	}
	spsLen := int(binary.BigEndian.Uint16(payload[6:8]))
	if spsLen <= 0 || 8+spsLen+3 > len(payload) {
		return nil, nil, false
	}
	ppsLen := int(binary.BigEndian.Uint16(payload[spsLen+9 : spsLen+11]))
	if ppsLen <= 0 || spsLen+11+ppsLen > len(payload) {
		return nil, nil, false
	}
	return payload[8 : 8+spsLen], payload[spsLen+11 : spsLen+11+ppsLen], true
}

// splitHEVC extracts VPS, SPS and PPS. Each unit is introduced by a four-byte
// tag followed by a big-endian length at offset 3.
func splitHEVC(payload []byte) (vps, sps, pps []byte, ok bool) {
	// SEC-004: the parameter-set block starts at offset 0x75. Slicing below
	// that must not panic on a short, attacker-supplied payload.
	if len(payload) < 0x75+5 {
		return nil, nil, nil, false
	}
	read := func(ptr []byte, tag []byte) ([]byte, int, bool) {
		if len(ptr) < 5 || !bytes.Equal(ptr[:4], tag) {
			return nil, 0, false
		}
		n := int(binary.BigEndian.Uint16(ptr[3:5]))
		if n <= 0 || 5+n > len(ptr) {
			return nil, 0, false
		}
		return ptr[5 : 5+n], 5 + n, true
	}
	vps, off, ok := read(payload[0x75:], []byte{0xa0, 0x00, 0x01, 0x00})
	if !ok {
		return nil, nil, nil, false
	}
	sps, off2, ok := read(payload[0x75+off:], []byte{0xa1, 0x00, 0x01, 0x00})
	if !ok {
		return nil, nil, nil, false
	}
	pps, _, ok = read(payload[0x75+off+off2:], []byte{0xa2, 0x00, 0x01, 0x00})
	if !ok {
		return nil, nil, nil, false
	}
	return vps, sps, pps, true
}

// packParameterSets joins NAL units with Annex-B start codes.
func packParameterSets(units ...[]byte) []byte {
	var n int
	for _, u := range units {
		n += len(AnnexBStartCode) + len(u)
	}
	out := make([]byte, 0, n)
	for _, u := range units {
		out = append(out, AnnexBStartCode...)
		out = append(out, u...)
	}
	return out
}

// geometryFloat reads one of the header's geometry fields. byteutils_get_float
// is a plain memcpy, so the value is host order; every platform we ship is
// little endian.
func geometryFloat(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}
