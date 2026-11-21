// Package receiver is the AirPlay receiver state machine: it ties the control
// endpoints, FairPlay, pairing and the mirror stream together.
package receiver

import (
	"crypto/sha512"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/audio"
	"github.com/miku0139/airplay-go/backend/internal/decode"
	"github.com/miku0139/airplay-go/backend/internal/fairplay"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// Status is what the receiver reports to the UI.
type Status int

// The states the UI renders.
const (
	StatusIdle Status = iota
	StatusConnecting
	StatusLive
	StatusInterrupted
)

// Events receives state changes for the UI and the log.
type Events struct {
	OnFrame    func(*mirror.Frame)
	OnAudio    func(*decode.AudioFrame)
	OnGeometry func(mirror.Size)
	OnState    func(Status, string)
	OnClient   func(deviceID, name, model string)
}

// Receiver serves one sender at a time, keyed by client address.
type Receiver struct {
	info   protocol.DeviceInfo
	log    *log.Logger
	events Events
	keys   *pairing.Keys

	mirror  *mirror.Server
	timing  *udpStub
	audioRx *audio.Receiver
	display struct{ width, height int }

	mu        sync.Mutex
	sessions  map[string]*clientSession
	cleanStop atomic.Bool
}

// clientSession is the state of one sender, keyed by its IP address.
type clientSession struct {
	fp   *fairplay.Session
	pair *pairing.Session
	aes  []byte // FairPlay output, before any pairing hash
	iv   []byte
	// aesCandidates are the stream keys the mirror connection tests, in
	// preference order.
	aesCandidates []mirror.KeyCandidate
	// ecdh is the shared secret from pair-verify, if the sender paired.
	ecdh []byte
	// lastStreamID is the mirror stream connection ID from /setup.
	lastStreamID uint64
	haveEKey     bool
}

// New starts the mirror, timing and audio listeners and returns the receiver.
// The listeners bind before mDNS starts so the advertised ports are real.
func New(info protocol.DeviceInfo, keys *pairing.Keys, logger *log.Logger, events Events) (*Receiver, error) {
	if logger == nil {
		logger = log.Default()
	}
	r := &Receiver{
		info:     info,
		log:      logger,
		events:   events,
		keys:     keys,
		sessions: make(map[string]*clientSession),
	}

	mr := mirror.NewServer(logger, mirror.Handlers{
		OnFrame: func(f *mirror.Frame) {
			if events.OnFrame != nil {
				events.OnFrame(f)
			}
		},
		OnGeometry: func(s mirror.Size) {
			r.log.Printf("mirror: geometry %dx%d (source %dx%d, offset %d,%d)",
				s.Width, s.Height, s.SourceWidth, s.SourceHeight, s.XOffset, s.YOffset)
			if events.OnGeometry != nil {
				events.OnGeometry(s)
			}
		},
		OnConnected: func(remote string) {
			r.cleanStop.Store(false)
			r.setState(StatusLive, "mirroring")
		},
		OnDisconnected: func() {
			if r.cleanStop.Swap(false) {
				r.setState(StatusIdle, "disconnected")
			} else {
				r.setState(StatusInterrupted, "sender closed the stream")
			}
		},
		OnPause: func() {
			r.log.Printf("mirror: sender suspended video")
		},
	})
	r.mirror = mr
	if _, err := mr.Start(); err != nil {
		return nil, err
	}
	logger.Printf("mirror: listening on port %d", mr.Port())

	timing, err := newUDPStub()
	if err != nil {
		return nil, fmt.Errorf("timing port: %w", err)
	}
	r.timing = timing

	audioRx, err := audio.New(logger, audio.Handlers{
		OnFrame: func(f *decode.AudioFrame) {
			if events.OnAudio != nil {
				events.OnAudio(f)
			}
		},
		OnSync: func(rtp uint32, ntp uint64, next uint32) {
			r.log.Printf("audio: sync rtp=%d ntp=%d next=%d", rtp, ntp, next)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("audio ports: %w", err)
	}
	r.audioRx = audioRx
	audioData, audioControl := audioRx.Ports()
	logger.Printf("timing: port %d, audio: control %d / data %d",
		r.timing.port, audioControl, audioData)
	return r, nil
}

// Stats reports the byte and frame counts of the current stream.
func (r *Receiver) Stats() (bytes, frames uint64) { return r.mirror.Stats() }

// AudioStats reports the audio packet and decoded-frame counts, which is how a
// silent session is told apart from one where nothing arrived.
func (r *Receiver) AudioStats() (received, dropped, decoded uint64) {
	if r.audioRx == nil {
		return 0, 0, 0
	}
	return r.audioRx.Stats()
}

// Close releases every listener.
func (r *Receiver) Close() {
	if r.mirror != nil {
		_ = r.mirror.Close()
	}
	if r.timing != nil {
		r.timing.close()
	}
	if r.audioRx != nil {
		r.audioRx.Close()
	}
}

// MirrorPort reports the mirror data port.
func (r *Receiver) MirrorPort() int { return r.mirror.Port() }

// Teardown handles an RTSP TEARDOWN from the sender. A teardown naming only the
// audio stream (type 96) resets audio without interrupting video; a video or
// whole-session teardown returns the receiver cleanly to StatusIdle.
func (r *Receiver) Teardown(remote string, body []byte) {
	if len(body) > 0 {
		var req map[string]any
		if _, err := plist.Unmarshal(body, &req); err == nil {
			if streams, ok := req["streams"].([]any); ok && len(streams) > 0 {
				hasVideo := false
				hasAudio := false
				for _, raw := range streams {
					if m, ok := raw.(map[string]any); ok {
						switch streamType(m) {
						case 110:
							hasVideo = true
						case 96:
							hasAudio = true
						}
					}
				}
				if hasAudio && !hasVideo {
					r.audioRx.ResetStream()
					return
				}
			}
		}
	}
	r.cleanStop.Store(true)
	r.audioRx.ResetStream()
	r.setState(StatusIdle, "sender stopped mirroring")
}

// Disconnect drops the current mirroring session from the receiver side and
// returns cleanly to StatusIdle.
func (r *Receiver) Disconnect() {
	r.cleanStop.Store(true)
	if r.audioRx != nil {
		r.audioRx.ResetStream()
	}
	if r.mirror != nil {
		r.mirror.DropClient()
	}
	r.setState(StatusIdle, "disconnected by user")
}

func (r *Receiver) setState(s Status, reason string) {
	if r.events.OnState != nil {
		r.events.OnState(s, reason)
	}
}

// session returns the per-client state, creating it on first contact.
func (r *Receiver) session(remote string) *clientSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.sessions[remote]
	if !ok {
		cs = &clientSession{
			fp:   fairplay.NewSession(),
			pair: pairing.NewSession(r.keys),
		}
		r.sessions[remote] = cs
	}
	return cs
}

// DropSession forgets a client, e.g. when it disconnects.
func (r *Receiver) DropSession(remote string) {
	r.mu.Lock()
	delete(r.sessions, remote)
	r.mu.Unlock()
}

// FPSetup answers /fp-setup. A 16-byte body is the first exchange, a 164-byte
// body the second; the reply lengths follow from that.
func (r *Receiver) FPSetup(remote string, body []byte) ([]byte, error) {
	cs := r.session(remote)
	switch len(body) {
	case fairplay.SetupRequestLen:
		reply, err := cs.fp.Setup(body)
		if err != nil {
			return nil, wrapProtocol(err)
		}
		r.setState(StatusConnecting, "fairplay setup")
		return reply, nil
	case fairplay.HandshakeRequestLen:
		reply, err := cs.fp.Handshake(body)
		return reply, wrapProtocol(err)
	default:
		return nil, &protocolError{status: 400, err: fairplay.ErrMalformed}
	}
}

// PairSetup answers /pair-setup with this receiver's Ed25519 public key.
func (r *Receiver) PairSetup(remote string, body []byte) ([]byte, error) {
	cs := r.session(remote)
	reply, err := cs.pair.Setup(body)
	if err != nil {
		return nil, &protocolError{status: 400, err: err}
	}
	r.setState(StatusConnecting, "pair-setup")
	return reply, nil
}

// PairVerify answers /pair-verify. After the first step the ECDH secret exists,
// which changes how /setup derives the stream key.
//
// A request the state machine cannot process — a sender probing before
// pair-setup, a retry, a bad signature — is answered with an empty 200, exactly
// as UxPlay does. Senders treat a 4xx here as "this receiver cannot pair" and
// abandon the session; an empty reply lets them pick a different path.
func (r *Receiver) PairVerify(remote string, body []byte) ([]byte, error) {
	cs := r.session(remote)
	reply, err := cs.pair.Verify(body)
	if err != nil {
		r.log.Printf("pair: verify from %s: %v", remote, err)
		return nil, nil
	}
	if secret := cs.pair.Secret(); len(secret) > 0 {
		cs.ecdh = secret
	}
	return reply, nil
}

// ErrNoKey reports that /setup asked for a stream before FairPlay produced a key.
var ErrNoKey = errors.New("receiver: no stream key yet")

// protocolError carries the HTTP status the control endpoint should answer with.
type protocolError struct {
	status int
	err    error
}

func (e *protocolError) Error() string   { return e.err.Error() }
func (e *protocolError) Unwrap() error   { return e.err }
func (e *protocolError) HTTPStatus() int { return e.status }

// wrapProtocol turns a FairPlay failure into the status a sender understands:
// 400 for a malformed exchange, 501 for a version we do not speak.
func wrapProtocol(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fairplay.ErrUnsupportedVersion):
		return &protocolError{status: 501, err: err}
	case errors.Is(err, fairplay.ErrMalformed), errors.Is(err, fairplay.ErrOutOfOrder):
		return &protocolError{status: 400, err: err}
	default:
		return err
	}
}

// Setup answers /setup. The first call carries ekey/eiv and is followed by calls
// that create the mirror and audio streams.
func (r *Receiver) Setup(remote string, req map[string]any) (map[string]any, error) {
	cs := r.session(remote)
	res := map[string]any{}

	if ekey, eiv, ok := keyMaterial(req); ok {
		cs.iv = eiv
		key, err := cs.fp.DecryptKey(ekey)
		if err != nil {
			return nil, &protocolError{status: 400, err: fmt.Errorf("decrypt ekey: %w", err)}
		}
		cs.aes = key
		cs.aesCandidates = []mirror.KeyCandidate{{Name: "fairplay", Key: key}}
		if len(cs.ecdh) > 0 {
			// With pairing, UxPlay hashes the FairPlay key together with the
			// shared ECDH secret using its SHA-512 helper, truncated to 16 bytes.
			// lib/raop_handlers.h raop_handler_setup.
			// Both forms are offered: the mirror stream tests them against its
			// first encrypted packet and keeps whichever decodes.
			h := sha512.New()
			h.Write(key)
			h.Write(cs.ecdh)
			hashed := h.Sum(nil)[:fairplay.AESKeyLen]
			cs.aesCandidates = []mirror.KeyCandidate{
				{Name: "fairplay+ecdh", Key: hashed},
				{Name: "fairplay", Key: key},
			}
		}
		cs.haveEKey = true
		// The audio stream uses the same key candidates, but AES-128-CBC with
		// eiv as the IV rather than a derived CTR stream.
		audioKeys := make([]audio.Key, len(cs.aesCandidates))
		for i, c := range cs.aesCandidates {
			audioKeys[i] = audio.Key{Name: c.Name, Key: c.Key}
		}
		r.audioRx.Configure(audioKeys, eiv)
		r.log.Printf("setup: stream key derived for %s (%d candidate(s))", remote, len(cs.aesCandidates))
		for _, c := range cs.aesCandidates {
			r.log.Printf("setup:   candidate %q key % x", c.Name, c.Key)
		}
	}

	res["timingPort"] = uint64(r.timing.port)
	res["eventPort"] = uint64(0)

	streams, _ := req["streams"].([]any)
	if len(streams) == 0 {
		return res, nil
	}

	// Logging the requested types makes a silent sender diagnosable: no audio
	// stream here means the phone never asked for one, which is a different
	// problem from audio that arrives and cannot be decrypted.
	requested := make([]uint64, 0, len(streams))
	for _, raw := range streams {
		if stream, ok := raw.(map[string]any); ok {
			requested = append(requested, streamType(stream))
		}
	}
	r.log.Printf("setup: streams requested %v", requested)

	out := make([]any, 0, len(streams))
	for _, raw := range streams {
		stream, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch streamType(stream) {
		case 110:
			if !cs.haveEKey {
				return nil, &protocolError{status: 400, err: ErrNoKey}
			}
			id := uint64Value(stream["streamConnectionID"])
			cs.lastStreamID = id
			r.mirror.Prepare(id, cs.aesCandidates...)
			r.log.Printf("setup: mirror stream %d on port %d", id, r.mirror.Port())
			out = append(out, map[string]any{
				"type":     uint64(110),
				"dataPort": uint64(r.mirror.Port()),
			})
		case 96:
			// ct 8 is AAC-ELD, which is the only mirrored-audio codec iOS uses.
			// controlPort must be non-zero or the sender disables resends, so it
			// is always advertised.
			r.audioRx.ResetStream()
			dataPort, controlPort := r.audioRx.Ports()
			r.log.Printf("setup: audio stream ct=%v spf=%v on data %d / control %d",
				stream["ct"], stream["spf"], dataPort, controlPort)
			out = append(out, map[string]any{
				"type":        uint64(96),
				"dataPort":    uint64(dataPort),
				"controlPort": uint64(controlPort),
			})
		default:
			r.log.Printf("setup: unknown stream type %d", streamType(stream))
		}
	}
	res["streams"] = out
	return res, nil
}

// PairVerifySecret records the ECDH shared secret produced by pair-verify. The
// key derivation in /setup changes once it exists.
func (r *Receiver) PairVerifySecret(remote string, secret []byte) {
	cs := r.session(remote)
	cs.ecdh = append([]byte(nil), secret...)
}

// keyMaterial pulls ekey and eiv out of a /setup plist.
func keyMaterial(req map[string]any) (ekey, eiv []byte, ok bool) {
	ek, ok1 := req["ekey"].([]byte)
	iv, ok2 := req["eiv"].([]byte)
	if !ok1 || !ok2 || len(ek) != fairplay.EKeyLen || len(iv) != 16 {
		return nil, nil, false
	}
	return ek, iv, true
}

func streamType(stream map[string]any) uint64 {
	return uint64Value(stream["type"])
}

// uint64Value accepts the numeric types howett.net/plist produces.
func uint64Value(v any) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case uint32:
		return uint64(n)
	case uint16:
		return uint64(n)
	case int64:
		return uint64(n)
	case int:
		return uint64(n)
	case float64:
		return uint64(n)
	default:
		return 0
	}
}

// udpStub is a bound UDP socket whose port is advertised but whose contents are
// not yet interpreted. The real NTP responder replaces it in a later phase.
type udpStub struct {
	conn *net.UDPConn
	port int
}

func newUDPStub() (*udpStub, error) {
	// Dual-stack, matching the audio sockets: senders commonly use IPv6.
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, err
	}
	s := &udpStub{conn: conn, port: conn.LocalAddr().(*net.UDPAddr).Port}
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := conn.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	return s, nil
}

func (s *udpStub) close() { _ = s.conn.Close() }
