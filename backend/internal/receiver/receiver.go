// Package receiver is the AirPlay receiver state machine: it ties the control
// endpoints, FairPlay, pairing and the mirror stream together.
package receiver

import (
	"crypto/sha512"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/audio"
	"github.com/miku0139/airplay-go/backend/internal/decode"
	"github.com/miku0139/airplay-go/backend/internal/fairplay"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/trust"
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

type mirrorServer interface {
	Port() int
	Prepare(string, uint64, ...mirror.KeyCandidate) bool
	Busy() bool
	DropClient()
	ResetKey()
	Stats() (uint64, uint64)
	Close() error
}

type audioReceiver interface {
	Ports() (int, int)
	Configure([]audio.Key, []byte, ...[]byte)
	Busy() bool
	ResetStream()
	ResetKey()
	Stats() (uint64, uint64, uint64)
	Close()
}

// TrustStore persists only owner-directed approvals and checks current trust
// on every stream setup, including sessions verified before revocation.
type TrustStore interface {
	TrustSender(string) error
	IsTrustedSender(string) bool
	ClearTrustedSenders() error
}

// Receiver serves one sender at a time, keyed by client address.
type Receiver struct {
	info   protocol.DeviceInfo
	log    *log.Logger
	events Events
	keys   *pairing.Keys
	trust  TrustStore

	mirror  mirrorServer
	timing  *udpStub
	audioRx audioReceiver
	display struct{ width, height int }

	// mu serializes control requests, session state and shared-media ownership.
	mu              sync.Mutex
	sessions        map[string]*clientSession
	activeRemote    string
	cleanStop       atomic.Bool
	controlGrace    time.Duration
	afterFunc       func(time.Duration, func()) disconnectTimer
	now             func() time.Time
	pending         map[string]pendingApproval
	rejected        map[string]time.Time
	approvalSources map[netip.Prefix][]time.Time
	pinnedSender    string
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
	closeTimer   disconnectTimer
	closeVersion uint64
}

// New starts the mirror, timing and audio listeners and returns the receiver.
// The listeners bind before mDNS starts so the advertised ports are real.
func New(info protocol.DeviceInfo, keys *pairing.Keys, trusted TrustStore, logger *log.Logger, events Events) (*Receiver, error) {
	if trusted == nil {
		return nil, errors.New("receiver: a sender trust store is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	r := &Receiver{
		info:     info,
		log:      logger,
		events:   events,
		keys:     keys,
		trust:    trusted,
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
	r.mu.Lock()
	r.cancelAllDisconnectsLocked()
	r.mu.Unlock()
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
	audioOnly := false
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
					audioOnly = true
				}
			}
		}
	}
	r.mu.Lock()
	r.cancelDisconnectLocked(remote)
	owner := r.activeRemote == "" || r.activeRemote == remote
	if audioOnly {
		if owner && r.audioRx != nil {
			r.audioRx.ResetStream()
		}
		r.mu.Unlock()
		return
	}
	delete(r.sessions, remote)
	if owner {
		r.cleanStop.Store(true)
		r.dropMediaLocked()
	}
	r.mu.Unlock()
	if owner {
		r.setState(StatusIdle, "sender stopped mirroring")
	}
}

// Disconnect drops the current mirroring session from the receiver side and
// returns cleanly to StatusIdle.
func (r *Receiver) Disconnect() {
	r.mu.Lock()
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	r.dropMediaLocked()
	clear(r.sessions)
	r.mu.Unlock()
	r.setState(StatusIdle, "disconnected by user")
}

// dropMediaLocked releases both the active client and any unused setup keys.
func (r *Receiver) dropMediaLocked() {
	r.cancelDisconnectLocked(r.activeRemote)
	if r.mirror != nil {
		r.mirror.DropClient()
		r.mirror.ResetKey()
	}
	if r.audioRx != nil {
		r.audioRx.ResetKey()
	}
	r.activeRemote = ""
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
	return r.clientSessionLocked(remote)
}

// sessionLocked is for pairing/setup requests that resume a control session.
func (r *Receiver) sessionLocked(remote string) *clientSession {
	r.cancelDisconnectLocked(remote)
	return r.clientSessionLocked(remote)
}

// clientSessionLocked looks up state without extending media/control grace.
func (r *Receiver) clientSessionLocked(remote string) *clientSession {
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

// DropSession invalidates pairing/stream-key proof on the last control close.
// The owner's media keeps a short grace period; reconnects must verify afresh.
// Unowned sessions can be forgotten immediately.
func (r *Receiver) DropSession(remote string) {
	r.mu.Lock()
	if r.activeRemote != remote || remote == "" || r.sessions[remote] == nil {
		_, hadSession := r.sessions[remote]
		r.cancelDisconnectLocked(remote)
		delete(r.sessions, remote)
		idle := hadSession && r.activeRemote == "" && len(r.sessions) == 0
		r.mu.Unlock()
		if idle {
			r.setState(StatusIdle, "control connection closed")
		}
		return
	}
	cs := r.sessions[remote]
	// Media retains its own key/cipher copies during grace. Control state must
	// not let the next connection at this IP inherit the former peer's proof.
	cs.pair = pairing.NewSession(r.keys)
	cs.fp = fairplay.NewSession()
	cs.ecdh, cs.aes, cs.iv, cs.aesCandidates = nil, nil, nil, nil
	cs.haveEKey, cs.lastStreamID = false, 0
	if cs.closeTimer == nil {
		r.scheduleDisconnectLocked(remote, cs)
	}
	r.mu.Unlock()
}

// DropAllSessions forgets every sender, including their FairPlay and pairing
// state, when the HTTP /stop endpoint stops the receiver session.
func (r *Receiver) DropAllSessions() {
	r.mu.Lock()
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	r.dropMediaLocked()
	clear(r.sessions)
	r.mu.Unlock()
}

// FPSetup answers /fp-setup. A 16-byte body is the first exchange, a 164-byte
// body the second; the reply lengths follow from that.
func (r *Receiver) FPSetup(remote string, body []byte) ([]byte, error) {
	r.mu.Lock()
	cs := r.clientSessionLocked(remote)
	connecting := r.activeRemote == ""
	var reply []byte
	var err error
	switch len(body) {
	case fairplay.SetupRequestLen:
		reply, err = cs.fp.Setup(body)
	case fairplay.HandshakeRequestLen:
		reply, err = cs.fp.Handshake(body)
	default:
		err = fairplay.ErrMalformed
	}
	r.mu.Unlock()
	if err == nil && len(body) == fairplay.SetupRequestLen && connecting {
		r.setState(StatusConnecting, "fairplay setup")
	}
	return reply, wrapProtocol(err)
}

// PairSetup answers /pair-setup with this receiver's Ed25519 public key.
func (r *Receiver) PairSetup(remote string, body []byte) ([]byte, error) {
	r.mu.Lock()
	cs := r.sessionLocked(remote)
	connecting := r.activeRemote == ""
	reply, err := cs.pair.Setup(body)
	r.mu.Unlock()
	if err != nil {
		return nil, &protocolError{status: 400, err: err}
	}
	if connecting {
		r.setState(StatusConnecting, "pair-setup")
	}
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
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.sessionLocked(remote)
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

// authorizeSetupLocked never enrolls a sender. A verified but unknown identity
// is recorded for local approval before any stream keys or media can change.
func (r *Receiver) authorizeSetupLocked(fingerprint, remote, label string) error {
	if r.trust == nil {
		return &protocolError{status: 503, err: errors.New("receiver: sender trust store unavailable")}
	}
	if r.trust.IsTrustedSender(fingerprint) {
		return nil
	}
	r.recordPendingLocked(fingerprint, remote, label)
	return &protocolError{status: 403, err: ErrUntrusted}
}

// ClearTrustedSenders durably clears approvals and immediately revokes all
// pairing/media sessions and pending decisions. Identity resets also clear trust.
func (r *Receiver) ClearTrustedSenders() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trust == nil {
		return errors.New("receiver: sender trust store unavailable")
	}
	if err := r.trust.ClearTrustedSenders(); err != nil {
		return err
	}
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	r.dropMediaLocked()
	clear(r.sessions)
	clear(r.pending)
	clear(r.rejected)
	clear(r.approvalSources)
	r.pinnedSender = ""
	return nil
}

// ErrNoKey reports that /setup asked for a stream before FairPlay produced a key.
var ErrNoKey = errors.New("receiver: no stream key yet")

// ErrNotPaired reports a stream setup before pair-verify finished.
var ErrNotPaired = errors.New("receiver: complete pair-verify before stream setup")

// ErrUntrusted reports a verified sender that has not been enrolled by policy.
var ErrUntrusted = errors.New("receiver: sender is not trusted")

// ErrBusy reports that another sender owns a live stream or pending setup.
var ErrBusy = errors.New("receiver: another sender is streaming")

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
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.sessionLocked(remote)
	streams, _ := req["streams"].([]any)
	_, hasEKey := req["ekey"]
	_, hasEIV := req["eiv"]
	if (hasEKey || hasEIV || len(streams) > 0) && !cs.pair.Verified() {
		return nil, &protocolError{status: 403, err: ErrNotPaired}
	}
	if cs.pair.Verified() {
		fingerprint := trust.Fingerprint(cs.pair.PeerPublicKey())
		if err := r.authorizeSetupLocked(fingerprint, remote, senderLabel(req)); err != nil {
			return nil, err
		}
	}
	// Stage all per-sender changes until the complete request is accepted.
	next := *cs
	res := map[string]any{}

	if hasEKey || hasEIV {
		ekey, eiv, ok := keyMaterial(req)
		if !ok {
			return nil, &protocolError{status: 400, err: errors.New("receiver: malformed stream key material")}
		}
		key, err := cs.fp.DecryptKey(ekey)
		if err != nil {
			return nil, &protocolError{status: 400, err: fmt.Errorf("decrypt ekey: %w", err)}
		}
		next.iv = append([]byte(nil), eiv...)
		next.aes = key
		next.aesCandidates = []mirror.KeyCandidate{{Name: "fairplay", Key: key}}
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
			next.aesCandidates = []mirror.KeyCandidate{
				{Name: "fairplay+ecdh", Key: hashed},
				{Name: "fairplay", Key: key},
			}
		}
		next.haveEKey = true
	}

	res["timingPort"] = uint64(r.timing.port)
	res["eventPort"] = uint64(0)

	if len(streams) == 0 {
		*cs = next
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
	hasVideo, hasAudio := false, false
	for _, raw := range streams {
		stream, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch streamType(stream) {
		case 110:
			if !next.haveEKey {
				return nil, &protocolError{status: 400, err: ErrNoKey}
			}
			id := uint64Value(stream["streamConnectionID"])
			next.lastStreamID = id
			hasVideo = true
			out = append(out, map[string]any{
				"type":     uint64(110),
				"dataPort": uint64(r.mirror.Port()),
			})
		case 96:
			if !next.haveEKey {
				return nil, &protocolError{status: 400, err: ErrNoKey}
			}
			hasAudio = true
			// ct 8 is AAC-ELD, which is the only mirrored-audio codec iOS uses.
			// controlPort must be non-zero or the sender disables resends, so it
			// is always advertised.
			dataPort, controlPort := r.audioRx.Ports()
			// SEC-005: quote untrusted plist values so CR/LF or terminal escapes
			// cannot forge log lines.
			ct := fmt.Sprint(stream["ct"])
			spf := fmt.Sprint(stream["spf"])
			r.log.Printf("setup: audio stream ct=%q spf=%q on data %d / control %d",
				ct, spf, dataPort, controlPort)
			out = append(out, map[string]any{
				"type":        uint64(96),
				"dataPort":    uint64(dataPort),
				"controlPort": uint64(controlPort),
			})
		default:
			r.log.Printf("setup: unknown stream type %d", streamType(stream))
		}
	}
	if hasVideo || hasAudio {
		// Check ownership before either mirror keys or audio buffers can change.
		// Audio liveness also protects senders using only the audio stream.
		if r.activeRemote != "" && r.activeRemote != remote && (r.mirror.Busy() || r.audioRx.Busy()) {
			return nil, &protocolError{status: 453, err: ErrBusy}
		}
		if hasVideo && !r.mirror.Prepare(remote, next.lastStreamID, next.aesCandidates...) {
			return nil, &protocolError{status: 453, err: ErrBusy}
		}
		if !hasVideo && r.activeRemote != "" && r.activeRemote != remote {
			r.mirror.DropClient()
			r.mirror.ResetKey()
		}
		r.activeRemote = remote
		// Audio uses the same per-sender candidates, with CBC and the setup IV.
		// Key-only setup never reconfigures this shared receiver.
		audioKeys := make([]audio.Key, len(next.aesCandidates))
		for i, c := range next.aesCandidates {
			audioKeys[i] = audio.Key{Name: c.Name, Key: c.Key}
		}
		r.audioRx.Configure(audioKeys, next.iv)
	}
	*cs = next
	res["streams"] = out
	return res, nil
}

// PairVerifySecret records the ECDH shared secret produced by pair-verify. The
// key derivation in /setup changes once it exists.
func (r *Receiver) PairVerifySecret(remote string, secret []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.clientSessionLocked(remote)
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
