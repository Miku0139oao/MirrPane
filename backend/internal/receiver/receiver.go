// Package receiver is the AirPlay receiver state machine: it ties the control
// endpoints, FairPlay, pairing and the mirror stream together.
package receiver

import (
	"bytes"
	"crypto/sha512"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
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
	// OnVideoReset is called before the first picture of a new mirror session
	// and when the owning session ends, so decoder state and any published
	// picture from the previous session are discarded. It never runs
	// concurrently with OnFrame.
	OnVideoReset func()
	// OnAudioReset is called when a new audio track becomes active and when
	// the current one ends, so buffered PCM from the previous session cannot
	// play into the new one. It never runs concurrently with OnAudio.
	OnAudioReset func()
}

type mirrorServer interface {
	Port() int
	Prepare(string, uint64, ...mirror.KeyCandidate) (uint64, bool)
	Busy() bool
	DropClient()
	ResetKey()
	Stats() (uint64, uint64)
	Close() error
}

type audioReceiver interface {
	Ports() (int, int)
	ConfigurePeer(peer netip.Addr, controlPort int, keys []audio.Key, iv []byte, config ...[]byte)
	Busy() bool
	ResetStream()
	ResetKey()
	Stats() (uint64, uint64, uint64)
	LateDropped() uint64
	Diagnostics() audio.Diagnostics
	Close()
}

// TrustStore persists the sender identities admitted by the owner or by
// trust-on-first-use, and checks current trust on every stream setup,
// including sessions verified before revocation.
type TrustStore interface {
	TrustSender(string) error
	IsTrustedSender(string) bool
	ClearTrustedSenders() error
}

// maxSessions caps concurrent clientSession entries so unowned TCP probes
// cannot grow the session map unboundedly.
const maxSessions = 64

// Receiver serves one sender at a time, isolating pairing/FairPlay state per
// control connection key while tracking IP-level media/approval identity.
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
	mu                sync.Mutex
	sessions          map[string]*clientSession
	sessionSeq        uint64
	activeConn        string
	activeRemote      string
	activeFingerprint string
	requireApproval   bool
	cleanStop         atomic.Bool
	controlGrace      time.Duration
	afterFunc         func(time.Duration, func()) disconnectTimer
	now               func() time.Time
	pending           map[string]pendingApproval
	rejected          map[string]time.Time
	approvalSources   map[netip.Prefix][]time.Time
	pinnedSender      string

	// Media session ownership. preparedSession is the mirror generation the
	// latest accepted /setup authorized; videoSession is the generation that
	// actually committed and owns video. liveSource says which track keeps the
	// UI in StatusLive: "video", "audio" or "".
	preparedSession uint64
	videoSession    uint64
	liveSource      string
	audioBound      bool
	audioBind       *audioBinding

	// The admitted sender currently named to the UI.
	clientDeviceID string
	clientName     string
	clientModel    string

	// stateMu serializes OnState delivery without holding mu. Lock order is
	// strictly stateMu -> mu so callbacks can inspect mu safely and stale
	// connecting transitions cannot overwrite newer live or idle state.
	stateMu     sync.Mutex
	status      Status
	stateReason string
	pendingEmit bool

	// Callback lanes keep a reset from overlapping media of the same kind.
	// Lock order is videoCbMu/audioCbMu -> mu; no reset runs with mu held.
	videoCbMu sync.Mutex
	audioCbMu sync.Mutex
	// Serialize ownership changes through the delivery of their reset and
	// client notifications. An old teardown cannot flush a newer stream.
	transitionMu sync.Mutex
}

// mediaEffects are reset/notification side effects computed under mu and
// delivered after the lock is released.
type mediaEffects struct {
	videoReset    bool
	audioReset    bool
	clientCleared bool
}

// audioBinding is the peer, owner and key material currently configured in the
// audio receiver. It is compared against each /setup so a duplicate setup from
// the same owner cannot reset audio that is already streaming.
type audioBinding struct {
	owner       string
	peer        netip.Addr
	controlPort int
	keys        []audio.Key
	iv          []byte
}

func (b *audioBinding) matches(owner string, peer netip.Addr, controlPort int, keys []audio.Key, iv []byte) bool {
	if b == nil || b.owner != owner || b.peer != peer || b.controlPort != controlPort || len(b.keys) != len(keys) || !bytes.Equal(b.iv, iv) {
		return false
	}
	for i := range keys {
		if b.keys[i].Name != keys[i].Name || !bytes.Equal(b.keys[i].Key, keys[i].Key) {
			return false
		}
	}
	return true
}

// clientSession is the state of one control connection, keyed by connKey.
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
	lastSeq      uint64
	closeTimer   disconnectTimer
	closeVersion uint64
}

// New starts the mirror, timing and audio listeners and returns the receiver.
// The listeners bind before mDNS starts so the advertised ports are real.
// requireApproval selects the owner-approval gate for unknown senders; with
// false (the product default) a verified sender is trusted on first use.
func New(info protocol.DeviceInfo, keys *pairing.Keys, trusted TrustStore, logger *log.Logger, events Events, requireApproval bool) (*Receiver, error) {
	if trusted == nil {
		return nil, errors.New("receiver: a sender trust store is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	r := &Receiver{
		info:            info,
		log:             logger,
		events:          events,
		keys:            keys,
		trust:           trusted,
		requireApproval: requireApproval,
		sessions:        make(map[string]*clientSession),
	}

	mr := mirror.NewServer(logger, mirror.Handlers{
		OnFrame: r.handleMirrorFrame,
		OnGeometry: func(s mirror.Size) {
			r.log.Printf("mirror: geometry %dx%d (source %dx%d, offset %d,%d)",
				s.Width, s.Height, s.SourceWidth, s.SourceHeight, s.XOffset, s.YOffset)
			if events.OnGeometry != nil {
				events.OnGeometry(s)
			}
		},
		OnConnected:    r.handleMirrorConnected,
		OnDisconnected: r.handleMirrorDisconnected,
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
		Output: func(f *decode.AudioFrame) {
			// A reset (new track or track end) cannot interleave with frames
			// already being written to the speaker: it waits for this lane and
			// then flushes everything delivered so far.
			r.audioCbMu.Lock()
			defer r.audioCbMu.Unlock()
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

// AudioLateDropped reports the part of AudioStats' dropped count that was
// rejected for missing its playout deadline.
func (r *Receiver) AudioLateDropped() uint64 {
	if r.audioRx == nil {
		return 0
	}
	return r.audioRx.LateDropped()
}

// AudioDiagnostics reports the audio UDP socket counters, which tell "the
// sender streamed nothing" apart from "packets arrived and were rejected".
func (r *Receiver) AudioDiagnostics() audio.Diagnostics {
	if r.audioRx == nil {
		return audio.Diagnostics{}
	}
	return r.audioRx.Diagnostics()
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
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	r.cancelDisconnectLocked(remote)
	cs := r.sessions[remote]
	sameTrustedActive := false
	if cs != nil && cs.pair != nil && cs.pair.Verified() && r.activeFingerprint != "" && r.trust != nil {
		fp := trust.Fingerprint(cs.pair.PeerPublicKey())
		if fp == r.activeFingerprint && r.trust.IsTrustedSender(fp) {
			sameTrustedActive = true
		}
	}
	owner := (r.activeConn == "" && r.activeRemote == "") || r.activeConn == remote || sameTrustedActive
	if audioOnly {
		var fx mediaEffects
		idle := false
		if owner && r.audioRx != nil {
			r.audioRx.ResetStream()
			r.audioBound, r.audioBind = false, nil
			fx.audioReset = true
			if r.liveSource == "audio" {
				r.liveSource = ""
				if r.clearClientLocked() {
					fx.clientCleared = true
				}
				r.recordStateLocked(StatusIdle, "audio ended")
				idle = true
			}
		}
		r.mu.Unlock()
		r.applyEffects(fx)
		if idle {
			r.flushState()
		}
		return
	}
	delete(r.sessions, remote)
	var fx mediaEffects
	if owner {
		r.cleanStop.Store(true)
		fx = r.dropMediaLocked()
		r.recordStateLocked(StatusIdle, "sender stopped mirroring")
	}
	r.mu.Unlock()
	if owner {
		r.applyEffects(fx)
		r.flushState()
	}
}

// Disconnect drops the current mirroring session from the receiver side and
// returns cleanly to StatusIdle.
func (r *Receiver) Disconnect() {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	fx := r.dropMediaLocked()
	clear(r.sessions)
	r.recordStateLocked(StatusIdle, "disconnected by user")
	r.mu.Unlock()
	r.applyEffects(fx)
	r.flushState()
}

// dropMediaLocked releases both the active client and any unused setup keys.
// It returns the resets the consumer needs so the caller can deliver them
// after releasing mu.
func (r *Receiver) dropMediaLocked() mediaEffects {
	var fx mediaEffects
	r.cancelDisconnectLocked(r.activeConn)
	if r.activeConn != "" {
		delete(r.sessions, r.activeConn)
	}
	if r.mirror != nil {
		r.mirror.DropClient()
		r.mirror.ResetKey()
	}
	if r.audioRx != nil {
		r.audioRx.ResetKey()
	}
	if r.videoSession != 0 {
		fx.videoReset = true
	}
	if r.audioBound {
		fx.audioReset = true
	}
	if r.clearClientLocked() {
		fx.clientCleared = true
	}
	r.activeConn = ""
	r.activeRemote = ""
	r.activeFingerprint = ""
	r.preparedSession, r.videoSession = 0, 0
	r.liveSource = ""
	r.audioBound, r.audioBind = false, nil
	return fx
}

func (r *Receiver) recordStateLocked(s Status, reason string) {
	r.status = s
	r.stateReason = reason
	r.pendingEmit = true
}

func (r *Receiver) flushState() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	for {
		r.mu.Lock()
		if !r.pendingEmit {
			r.mu.Unlock()
			return
		}
		s := r.status
		reason := r.stateReason
		r.pendingEmit = false
		r.mu.Unlock()

		if r.events.OnState != nil {
			r.events.OnState(s, reason)
		}
	}
}

// handleMirrorFrame forwards a picture only while the generation that produced
// it still owns video. The callback lane also orders frames against
// OnVideoReset, so a stop can never deliver a picture after the consumer was
// told to discard the previous session.
func (r *Receiver) handleMirrorFrame(f *mirror.Frame) {
	if f == nil {
		return
	}
	r.videoCbMu.Lock()
	defer r.videoCbMu.Unlock()
	r.mu.Lock()
	live := f.Session != 0 && f.Session == r.videoSession
	onFrame := r.events.OnFrame
	r.mu.Unlock()
	if !live || onFrame == nil {
		return
	}
	onFrame(f)
}

// handleMirrorConnected accepts a connect callback only for the generation the
// latest authorized /setup prepared. A callback delayed past a stop or a newer
// setup therefore cannot mark a stopped stream live.
func (r *Receiver) handleMirrorConnected(session uint64, remote string) {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.videoCbMu.Lock()
	r.mu.Lock()
	if session == 0 || session != r.preparedSession || r.activeConn == "" {
		r.mu.Unlock()
		r.videoCbMu.Unlock()
		return
	}
	r.videoSession = session
	r.cleanStop.Store(false)
	r.liveSource = "video"
	r.recordStateLocked(StatusLive, "mirroring")
	r.mu.Unlock()
	if r.events.OnVideoReset != nil {
		r.events.OnVideoReset()
	}
	r.videoCbMu.Unlock()
	r.flushState()
}

// handleMirrorDisconnected ends only the session that still owns video and
// discards that session's pictures and decoder state. A superseded connection's
// late close is ignored.
func (r *Receiver) handleMirrorDisconnected(session uint64) {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	if session == 0 || session != r.videoSession {
		r.mu.Unlock()
		return
	}
	r.videoSession = 0
	clean := r.cleanStop.Swap(false)
	fx := mediaEffects{videoReset: true}
	if r.liveSource == "video" {
		r.liveSource = ""
	}
	if r.clearClientLocked() {
		fx.clientCleared = true
	}
	if clean {
		r.recordStateLocked(StatusIdle, "disconnected")
	} else {
		r.recordStateLocked(StatusInterrupted, "sender closed the stream")
	}
	r.mu.Unlock()
	r.applyEffects(fx)
	r.flushState()
}

// resetVideo runs the consumer's video reset in the frame callback lane.
func (r *Receiver) resetVideo() {
	r.videoCbMu.Lock()
	defer r.videoCbMu.Unlock()
	if r.events.OnVideoReset != nil {
		r.events.OnVideoReset()
	}
}

// resetAudio runs the consumer's audio reset in the audio callback lane.
func (r *Receiver) resetAudio() {
	r.audioCbMu.Lock()
	defer r.audioCbMu.Unlock()
	if r.events.OnAudioReset != nil {
		r.events.OnAudioReset()
	}
}

// applyEffects delivers collected reset/notification effects. The caller must
// have released mu.
func (r *Receiver) applyEffects(fx mediaEffects) {
	if fx.videoReset {
		r.resetVideo()
	}
	if fx.audioReset {
		r.resetAudio()
	}
	if fx.clientCleared && r.events.OnClient != nil {
		r.events.OnClient("", "", "")
	}
}

// clearClientLocked reports whether the UI still held a sender name.
func (r *Receiver) clearClientLocked() bool {
	if r.clientDeviceID == "" && r.clientName == "" && r.clientModel == "" {
		return false
	}
	r.clientDeviceID, r.clientName, r.clientModel = "", "", ""
	return true
}

// recordClientLocked stores a newly admitted sender's identity for the UI,
// keeping earlier fields when a later setup omits them.
func (r *Receiver) recordClientLocked(deviceID, name, model string) (string, string, string, bool) {
	if deviceID == "" && name == "" && model == "" {
		return "", "", "", false
	}
	if deviceID == "" {
		deviceID = r.clientDeviceID
	}
	if name == "" {
		name = r.clientName
	}
	if model == "" {
		model = r.clientModel
	}
	if deviceID == r.clientDeviceID && name == r.clientName && model == r.clientModel {
		return "", "", "", false
	}
	r.clientDeviceID, r.clientName, r.clientModel = deviceID, name, model
	return deviceID, name, model, true
}

// hostIP extracts the IP address from a control connection key ("ip:port",
// "[ipv6]:port", optional "#N" synthetic suffix, or bare IP in unit tests).
func hostIP(connKey string) string {
	if i := strings.LastIndexByte(connKey, '#'); i >= 0 && i < len(connKey)-1 {
		digits := true
		for j := i + 1; j < len(connKey); j++ {
			if connKey[j] < '0' || connKey[j] > '9' {
				digits = false
				break
			}
		}
		if digits {
			connKey = connKey[:i]
		}
	}
	if host, _, err := net.SplitHostPort(connKey); err == nil {
		return host
	}
	return connKey
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
	if r.sessions == nil {
		r.sessions = make(map[string]*clientSession)
	}
	r.sessionSeq++
	if cs, ok := r.sessions[remote]; ok {
		cs.lastSeq = r.sessionSeq
		return cs
	}
	if len(r.sessions) >= maxSessions {
		r.evictSessionLocked()
	}
	cs := &clientSession{
		fp:      fairplay.NewSession(),
		pair:    pairing.NewSession(r.keys),
		lastSeq: r.sessionSeq,
	}
	r.sessions[remote] = cs
	return cs
}

func (r *Receiver) evictSessionLocked() {
	var victim string
	var victimSeq uint64
	found := false
	for k, cs := range r.sessions {
		if k == r.activeConn || cs.closeTimer != nil {
			continue
		}
		if !found || cs.lastSeq < victimSeq {
			victim, victimSeq, found = k, cs.lastSeq, true
		}
	}
	if !found {
		for k, cs := range r.sessions {
			if k == r.activeConn {
				continue
			}
			if !found || cs.lastSeq < victimSeq {
				victim, victimSeq, found = k, cs.lastSeq, true
			}
		}
	}
	if found {
		r.cancelDisconnectLocked(victim)
		delete(r.sessions, victim)
	}
}

// DropSession invalidates pairing/stream-key proof on control connection close.
// The owning connection's media keeps a short grace period; reconnects must
// verify afresh on a new connection. Unowned connections are forgotten at once.
func (r *Receiver) DropSession(remote string) {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	if r.activeConn != remote || remote == "" || r.sessions[remote] == nil {
		_, hadSession := r.sessions[remote]
		r.cancelDisconnectLocked(remote)
		delete(r.sessions, remote)
		idle := hadSession && r.activeConn == "" && len(r.sessions) == 0
		var fx mediaEffects
		if idle {
			if r.clearClientLocked() {
				fx.clientCleared = true
			}
			r.recordStateLocked(StatusIdle, "control connection closed")
		}
		r.mu.Unlock()
		if idle {
			r.applyEffects(fx)
			r.flushState()
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
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	fx := r.dropMediaLocked()
	clear(r.sessions)
	r.recordStateLocked(StatusIdle, "receiver stopped")
	r.mu.Unlock()
	r.applyEffects(fx)
	r.flushState()
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
	emit := err == nil && len(body) == fairplay.SetupRequestLen && connecting
	if emit {
		r.recordStateLocked(StatusConnecting, "fairplay setup")
	}
	r.mu.Unlock()
	if emit {
		r.flushState()
	}
	return reply, wrapProtocol(err)
}

// PairSetup answers /pair-setup with this receiver's Ed25519 public key.
func (r *Receiver) PairSetup(remote string, body []byte) ([]byte, error) {
	r.mu.Lock()
	cs := r.sessionLocked(remote)
	connecting := r.activeRemote == ""
	reply, err := cs.pair.Setup(body)
	emit := err == nil && connecting
	if emit {
		r.recordStateLocked(StatusConnecting, "pair-setup")
	}
	r.mu.Unlock()
	if err != nil {
		return nil, &protocolError{status: 400, err: err}
	}
	if emit {
		r.flushState()
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

// authorizeSetupLocked admits a verified sender. A new identity is trusted on
// first use by default; with manual approval enabled it is instead recorded
// for the local owner and the setup is refused until approved.
func (r *Receiver) authorizeSetupLocked(fingerprint, remote, label string) error {
	if r.trust == nil {
		return &protocolError{status: 503, err: errors.New("receiver: sender trust store unavailable")}
	}
	if r.trust.IsTrustedSender(fingerprint) {
		return nil
	}
	if r.requireApproval {
		r.recordPendingLocked(fingerprint, remote, label)
		return &protocolError{status: 403, err: ErrUntrusted}
	}
	if err := r.trust.TrustSender(fingerprint); err != nil {
		// Fail closed: without a persisted identity the admission would not
		// survive a restart, and silently streaming would hide the failure.
		r.log.Printf("pair: auto-trust sender %s from %s: %v", fingerprint, remote, err)
		return &protocolError{status: 503, err: fmt.Errorf("receiver: enroll sender: %w", err)}
	}
	r.log.Printf("pair: auto-trusted sender %s from %s", fingerprint, remote)
	return nil
}

// ClearTrustedSenders durably clears trusted senders and immediately revokes
// all pairing/media sessions and pending decisions. Identity resets also clear
// trust.
func (r *Receiver) ClearTrustedSenders() error {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	if r.trust == nil {
		r.mu.Unlock()
		return errors.New("receiver: sender trust store unavailable")
	}
	if err := r.trust.ClearTrustedSenders(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.cleanStop.Store(true)
	r.cancelAllDisconnectsLocked()
	fx := r.dropMediaLocked()
	clear(r.sessions)
	clear(r.pending)
	clear(r.rejected)
	clear(r.approvalSources)
	r.pinnedSender = ""
	// A successful clear revokes the session no matter which state the UI was
	// showing, so record idle explicitly and notify after the lock is released.
	r.recordStateLocked(StatusIdle, "trust cleared")
	r.mu.Unlock()
	r.applyEffects(fx)
	r.flushState()
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

// setupEffects are the notifications a successful /setup collects while mu is
// held; they are delivered after the lock is released.
type setupEffects struct {
	media        mediaEffects
	clientChange bool
	clientDevice string
	clientName   string
	clientModel  string
	stateChanged bool
}

// Setup answers /setup. The first call carries ekey/eiv and is followed by calls
// that create the mirror and audio streams.
func (r *Receiver) Setup(remote string, req map[string]any) (map[string]any, error) {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	r.mu.Lock()
	res, err, fx := r.setupLocked(remote, req)
	r.mu.Unlock()
	r.applyEffects(fx.media)
	if fx.clientChange && r.events.OnClient != nil {
		r.events.OnClient(fx.clientDevice, fx.clientName, fx.clientModel)
	}
	if fx.stateChanged {
		r.flushState()
	}
	return res, err
}

func (r *Receiver) setupLocked(remote string, req map[string]any) (map[string]any, error, setupEffects) {
	var fx setupEffects
	cs := r.sessionLocked(remote)
	remoteIP := hostIP(remote)
	streams, _ := req["streams"].([]any)
	_, hasEKey := req["ekey"]
	_, hasEIV := req["eiv"]
	if (hasEKey || hasEIV || len(streams) > 0) && !cs.pair.Verified() {
		return nil, &protocolError{status: 403, err: ErrNotPaired}, fx
	}
	var fingerprint string
	var clientDeviceID, clientName, clientModel string
	if cs.pair.Verified() {
		fingerprint = trust.Fingerprint(cs.pair.PeerPublicKey())
		clientName = senderLabel(req)
		clientDeviceID = senderDeviceID(req)
		clientModel = senderModel(req)
		if err := r.authorizeSetupLocked(fingerprint, remoteIP, clientName); err != nil {
			return nil, err, fx
		}
	}
	// Stage all per-sender changes until the complete request is accepted.
	next := *cs
	res := map[string]any{}

	if hasEKey || hasEIV {
		ekey, eiv, ok := keyMaterial(req)
		if !ok {
			return nil, &protocolError{status: 400, err: errors.New("receiver: malformed stream key material")}, fx
		}
		key, err := cs.fp.DecryptKey(ekey)
		if err != nil {
			return nil, &protocolError{status: 400, err: fmt.Errorf("decrypt ekey: %w", err)}, fx
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
		// A key-only setup is still the first accepted contact: name the sender
		// once its identity is authorized.
		if deviceID, name, model, changed := r.recordClientLocked(clientDeviceID, clientName, clientModel); changed {
			fx.clientChange, fx.clientDevice, fx.clientName, fx.clientModel = true, deviceID, name, model
		}
		return res, nil, fx
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
	audioControlPort := 0
	for _, raw := range streams {
		stream, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch streamType(stream) {
		case 110:
			if !next.haveEKey {
				return nil, &protocolError{status: 400, err: ErrNoKey}, fx
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
				return nil, &protocolError{status: 400, err: ErrNoKey}, fx
			}
			hasAudio = true
			// The sender names the UDP port it listens on for resend requests.
			// Absent or invalid means no resends (0); the advertised control
			// port below stays non-zero so the sender keeps a resend path.
			audioControlPort = senderControlPort(stream)
			// ct 8 is AAC-ELD, which is the only mirrored-audio codec iOS uses.
			// controlPort must be non-zero or the sender disables resends, so it
			// is always advertised.
			dataPort, controlPort := r.audioRx.Ports()
			// SEC-005: quote untrusted plist values so CR/LF or terminal escapes
			// cannot forge log lines.
			ct := fmt.Sprint(stream["ct"])
			spf := fmt.Sprint(stream["spf"])
			r.log.Printf("setup: audio stream ct=%q spf=%q on data %d / control %d (sender control %d)",
				ct, spf, dataPort, controlPort, audioControlPort)
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
		// Another connection on the same IP can take over a live/grace session
		// only after fresh pair-verify proves the same trusted sender identity.
		sameConn := r.activeConn == remote
		sameTrustedIdentity := r.activeFingerprint != "" && fingerprint == r.activeFingerprint && r.activeRemote == remoteIP
		if r.activeConn != "" && !sameConn && !sameTrustedIdentity && (r.mirror.Busy() || r.audioRx.Busy()) {
			return nil, &protocolError{status: 453, err: ErrBusy}, fx
		}
		if hasVideo {
			generation, ok := r.mirror.Prepare(remoteIP, next.lastStreamID, next.aesCandidates...)
			if !ok {
				return nil, &protocolError{status: 453, err: ErrBusy}, fx
			}
			// Only a connection that commits this exact setup may report live.
			r.preparedSession = generation
		}
		if !hasVideo && r.activeConn != "" && !sameConn && !sameTrustedIdentity {
			r.mirror.DropClient()
			r.mirror.ResetKey()
			if r.videoSession != 0 {
				fx.media.videoReset = true
			}
			r.preparedSession, r.videoSession = 0, 0
			if r.liveSource == "video" {
				r.liveSource = ""
			}
		}
		if r.activeConn != "" && r.activeConn != remote {
			r.cancelDisconnectLocked(r.activeConn)
			delete(r.sessions, r.activeConn)
		}
		r.activeConn = remote
		r.activeRemote = remoteIP
		r.activeFingerprint = fingerprint
		if hasAudio {
			// The audio receiver needs the authenticated peer and the sender's
			// own control port to verify sources and request resends. A repeat
			// setup from the same owner with identical material must not reset a
			// stream that is already playing, so only a changed binding is
			// configured (and a new track is announced to the consumer).
			peer := parsePeer(remoteIP)
			audioKeys := audioKeysFromCandidates(next.aesCandidates)
			if !r.audioBound || !r.audioBind.matches(fingerprint, peer, audioControlPort, audioKeys, next.iv) {
				r.audioRx.ConfigurePeer(peer, audioControlPort, audioKeys, append([]byte(nil), next.iv...))
				r.audioBound = true
				r.audioBind = &audioBinding{
					owner:       fingerprint,
					peer:        peer,
					controlPort: audioControlPort,
					keys:        audioKeys,
					iv:          append([]byte(nil), next.iv...),
				}
				fx.media.audioReset = true
			}
			// Audio-only senders have no video frame to announce playback.
			if !hasVideo && r.videoSession == 0 && r.liveSource == "" {
				r.liveSource = "audio"
				r.recordStateLocked(StatusLive, "audio")
				fx.stateChanged = true
			}
		}
	}
	*cs = next
	// The sender is now the accepted owner; only a completed setup may name it
	// to the UI, so a trusted but busy probe cannot mislabel a live session.
	if deviceID, name, model, changed := r.recordClientLocked(clientDeviceID, clientName, clientModel); changed {
		fx.clientChange, fx.clientDevice, fx.clientName, fx.clientModel = true, deviceID, name, model
	}
	res["streams"] = out
	return res, nil, fx
}

// senderControlPort reads the control port a sender listens on for resend
// requests. Absent or out of range means "no resend channel".
func senderControlPort(stream map[string]any) int {
	port := uint64Value(stream["controlPort"])
	if port == 0 || port > 65535 {
		return 0
	}
	return int(port)
}

func parsePeer(remoteIP string) netip.Addr {
	addr, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func audioKeysFromCandidates(candidates []mirror.KeyCandidate) []audio.Key {
	keys := make([]audio.Key, len(candidates))
	for i, c := range candidates {
		keys[i] = audio.Key{Name: c.Name, Key: append([]byte(nil), c.Key...)}
	}
	return keys
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
