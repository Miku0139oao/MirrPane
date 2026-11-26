// Package ipc serves the AirPlay control endpoints over RTSP/TCP.
//
// The control channel is not HTTP: senders open a TCP connection, send
// "GET /info RTSP/1.0" style request lines and keep the connection open for the
// whole session. Requests and replies therefore go through the parser in
// rtsp.go, not net/http. See the routing table in UxPlay's lib/raop.c, which
// this file mirrors.
package ipc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// serverVersion is advertised in the Server header and the /info plist.
const serverVersion = protocol.SourceVersion

const maxControlConnections = 64

// Server answers the AirPlay control endpoints that iPhones and iPads call when
// they discover and connect to a receiver.
type Server struct {
	info    protocol.DeviceInfo
	display Display
	mu      sync.RWMutex

	// Handler hooks, set by the protocol layer as it comes online. The remote
	// argument is a per-control-connection key (such as "ip:port" or a unique
	// synthetic key) that isolates pairing and FairPlay proof per TCP socket.
	OnFPSetup    func(remote string, body []byte) ([]byte, error)
	OnPairSetup  func(remote string, body []byte) ([]byte, error)
	OnPairVerify func(remote string, body []byte) ([]byte, error)
	OnSetup      func(remote string, req map[string]any) (map[string]any, error)
	OnTeardown   func(remote string, body []byte)
	OnFeedback   func(remote string, body []byte)
	OnEvent      func(remote string, body []byte)
	// OnDisconnect forgets a specific control connection when it closes. It
	// may follow a full OnTeardown, so forgetting the connection must be idempotent.
	OnDisconnect func(remote string)
	// OnStop releases all sender sessions for the HTTP /stop endpoint.
	OnStop func()

	// connMu guards per-connection identity assignment so concurrent sockets
	// (including portless net.Pipe peers) never collide on a session key.
	connMu      sync.Mutex
	connSeq     uint64
	activeConns map[string]struct{}

	// limMu/limiters bound per-sender request rates so a pairing or handshake
	// flood cannot hold the receiver-wide control lock for an unbounded time.
	limMu    sync.Mutex
	limiters map[string]*requestBudget
	prefixes map[string]*prefixWindow

	limitLogAt    time.Time
	limitLogCount int

	// TXTRecord returns the DNS-SD TXT record in wire form for /info?txtAirPlay
	// (raop false) or /info?txtRAOP (raop true). Set before serving.
	TXTRecord func(raop bool) []byte

	ln       net.Listener
	closing  chan struct{}
	sockets  map[net.Conn]struct{}
	handlers sync.WaitGroup

	started time.Time
}

// Display describes the virtual screen we present to clients.
type Display struct {
	UUID        string
	Width       int
	Height      int
	MaxFPS      int
	RefreshHz   float64
	Overscanned bool
}

// New creates the control server.
func New(info protocol.DeviceInfo, display Display) *Server {
	if display.UUID == "" {
		display.UUID = "e0ff8a27-6738-3d56-8a16-cc53aacee925"
	}
	if display.MaxFPS == 0 {
		display.MaxFPS = 60
	}
	if display.RefreshHz == 0 {
		display.RefreshHz = 60
	}
	return &Server{
		info:    info,
		display: display,
		closing: make(chan struct{}),
		sockets: make(map[net.Conn]struct{}),
		started: time.Now(),
	}
}

// Listen binds addr and starts serving.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve answers control requests on an already bound listener. Binding outside
// lets the caller report the real port before advertising it.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	select {
	case <-s.closing:
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	default:
	}
	s.ln = ln
	s.mu.Unlock()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// A closed listener returns the same error forever; without this
				// check the loop floods the log during shutdown and starves it.
				if errors.Is(err, net.ErrClosed) {
					return
				}
				select {
				case <-s.closing:
					return
				default:
				}
				log.Printf("ipc: accept: %v", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if !s.admit(conn) {
				conn.Close()
				continue
			}
			go func() {
				defer s.finish(conn)
				s.serveConn(conn)
			}()
		}
	}()
	return nil
}

func (s *Server) admit(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closing:
		return false
	default:
	}
	if len(s.sockets) >= maxControlConnections {
		return false
	}
	s.sockets[conn] = struct{}{}
	s.handlers.Add(1)
	return true
}

func (s *Server) finish(conn net.Conn) {
	s.mu.Lock()
	delete(s.sockets, conn)
	s.mu.Unlock()
	s.handlers.Done()
}

// Addr reports the bound TCP address.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops accepting and waits for the existing control handlers to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	var err error
	select {
	case <-s.closing:
	default:
		close(s.closing)
		if s.ln != nil {
			err = s.ln.Close()
		}
		for socket := range s.sockets {
			socket.Close()
		}
	}
	s.mu.Unlock()
	s.handlers.Wait()
	return err
}

// dispatch answers one control message. The routing table matches UxPlay's
// lib/raop.c: RTSP/1.0 methods carry the protocol, HTTP/1.1 carries the web API
// endpoints (reverse, scrub, rate, stop, fp-setup2, server-info).
func (s *Server) dispatch(req *Request, remote string) *Response {
	peerIP := ipOf(remote)
	if !s.allowRequest(peerIP) {
		s.noteRateLimited(peerIP)
		return &Response{Status: 429}
	}
	path := req.Path()

	switch {
	case req.Protocol == "RTSP/1.0" || isRTSPMethod(req.Method):
		return s.dispatchRTSP(req, remote, path)

	case req.Protocol == "HTTP/1.1":
		return s.dispatchHTTP(req, remote, path)
	}

	return &Response{Status: 200}
}

// isRTSPMethod reports the methods AirPlay defines that HTTP does not.
func isRTSPMethod(m string) bool {
	switch m {
	case "SETUP", "RECORD", "FLUSH", "TEARDOWN", "GET_PARAMETER", "SET_PARAMETER", "ANNOUNCE", "SETUP2":
		return true
	}
	return false
}

func (s *Server) dispatchRTSP(req *Request, remote string, path string) *Response {
	switch req.Method {
	case "POST":
		switch path {
		case "/feedback":
			if s.OnFeedback != nil {
				s.OnFeedback(remote, req.Body)
			}
			return &Response{}
		case "/pair-pin-start":
			// UxPlay answers with an empty reply and pushes the PIN to the UI.
			return &Response{}
		case "/pair-setup-pin":
			// The PIN flow is the AirPlay 2 pairing variant; we speak the
			// legacy /pair-setup + /pair-verify pair instead.
			return &Response{Status: 470}
		case "/pair-setup":
			return s.binary(remote, req.Body, s.OnPairSetup)
		case "/pair-verify":
			return s.binary(remote, req.Body, s.OnPairVerify)
		case "/fp-setup":
			return s.binary(remote, req.Body, s.OnFPSetup)
		case "/audioMode":
			// UxPlay answers an empty 200 without looking at the body; iOS uses
			// it to probe whether audio-only playback is possible.
			return &Response{}
		}
		// A known method on an unknown URL is not an error: UxPlay leaves its
		// pre-initialised 200 in place. Endpoints such as /event, /volume and
		// /pair-remove land here.
		log.Printf("ipc: tolerated %q %q", req.Method, logTarget(req.Target))
		return &Response{}

	case "GET":
		if strings.HasPrefix(req.Target, "/info") {
			return s.handleInfo(req)
		}
		log.Printf("ipc: tolerated %q %q", req.Method, logTarget(req.Target))
		return &Response{}

	case "OPTIONS":
		return &Response{Headers: []header{{"Public",
			"SETUP, RECORD, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER"}}}

	case "SETUP":
		return s.binaryPlist(remote, req.Body, s.OnSetup)

	case "GET_PARAMETER":
		return s.handleGetParameter(req)

	case "SET_PARAMETER":
		return s.handleSetParameter(req)

	case "RECORD":
		// Advertises the audio pipeline latency the sender should account for.
		return &Response{Headers: []header{
			{"Audio-Latency", "0"},
			{"Audio-Jack-Status", "connected; type=analog"},
		}}

	case "FLUSH":
		return &Response{}

	case "TEARDOWN":
		if s.OnTeardown != nil {
			s.OnTeardown(remote, req.Body)
		}
		return &Response{}

	case "ANNOUNCE":
		return &Response{Status: 453}
	}

	log.Printf("ipc: unhandled %q %q %q", req.Method, logTarget(req.Target), req.Protocol)
	return &Response{Status: 501}
}

func isLoopbackRemote(remote string) bool {
	addr, err := netip.ParseAddr(ipOf(remote))
	return err == nil && addr.Unmap().IsLoopback()
}

func (s *Server) dispatchHTTP(req *Request, remote, path string) *Response {
	switch req.Method {
	case "POST":
		switch {
		case path == "/reverse":
			// The sender asks us to open a reverse connection back to it. We
			// answer OK; the mirror stream itself is inbound, so nothing more
			// is needed.
			return &Response{}
		case path == "/stop":
			if !isLoopbackRemote(remote) {
				return &Response{Status: 403}
			}
			if s.OnStop != nil {
				s.OnStop()
			}
			return &Response{}
		case path == "/fp-setup2":
			// AirPlay 2's second FairPlay exchange; not implemented.
			return &Response{Status: 501}
		case strings.HasPrefix(path, "/scrub") || strings.HasPrefix(path, "/rate"):
			return &Response{}
		case path == "/play" || path == "/action":
			return &Response{}
		}
	case "GET":
		switch path {
		case "/server-info":
			return s.handleServerInfo()
		case "/playback-info":
			return &Response{Status: 404}
		}
	}
	log.Printf("ipc: unhandled %q %q %q", req.Method, logTarget(req.Target), req.Protocol)
	return &Response{Status: 200}
}

// binary answers a body as a binary blob. An empty reply is written as a bare
// 200 with no Content-Type, which is how UxPlay answers a pair-verify request it
// cannot process.
func (s *Server) binary(remote string, body []byte, h func(string, []byte) ([]byte, error)) *Response {
	if h == nil {
		return &Response{Status: 501}
	}
	out, err := h(remote, body)
	if err != nil {
		return errorResponse(err)
	}
	if len(out) == 0 {
		return &Response{}
	}
	return &Response{ContentType: "application/octet-stream", Body: out}
}

// binaryPlist answers a plist body with a plist reply.
func (s *Server) binaryPlist(remote string, body []byte, h func(string, map[string]any) (map[string]any, error)) *Response {
	req := map[string]any{}
	if len(body) > 0 {
		if _, err := plist.Unmarshal(body, &req); err != nil {
			log.Printf("ipc: setup plist decode: %v", err)
			return &Response{Status: 400}
		}
	}
	if h == nil {
		return &Response{Status: 501}
	}
	res, err := h(remote, req)
	if err != nil {
		return errorResponse(err)
	}
	return plistResponse(res)
}

// errorResponse maps a protocol error onto the status a sender understands.
func errorResponse(err error) *Response {
	var code statusCoder
	if errors.As(err, &code) {
		log.Printf("ipc: protocol error: %v", err)
		return &Response{Status: code.HTTPStatus()}
	}
	log.Printf("ipc: error: %v", err)
	return &Response{Status: 500}
}

// statusCoder lets a protocol error choose its status without the control server
// having to know about protocol packages.
type statusCoder interface {
	error
	HTTPStatus() int
}

func (s *Server) handleInfo(req *Request) *Response {
	// A sender asks for the raw DNS-SD record in one of two ways: with a
	// qualifier array in a binary plist body (modern), or with ?txtAirPlay /
	// ?txtRAOP in the URL (the Bluetooth-LE path, which carries no CSeq).
	//
	// UxPlay answers both with a binary plist holding a single key whose value
	// is the TXT record as a DATA node in DNS-SD wire form, and no device keys.
	if key := txtQualifier(req); key != "" {
		if rec := s.txtRecord(key == "txtRAOP"); rec != nil {
			return plistResponse(map[string]any{key: rec})
		}
	}
	if key := queryTXTKey(req.Query()); key != "" {
		if rec := s.txtRecord(key == "txtRAOP"); rec != nil {
			return plistResponse(map[string]any{key: rec})
		}
	}

	s.mu.RLock()
	disp := s.display
	s.mu.RUnlock()

	features := featuresUint64()
	deviceID := protocol.AirplayDeviceID(s.info.MAC)
	props := map[string]any{
		"deviceID":                 deviceID,
		"macAddress":               deviceID,
		"features":                 features,
		"name":                     s.info.Name,
		"pi":                       s.info.PairingID,
		"vv":                       uint64(2),
		"statusFlags":              uint64(68),
		"keepAliveLowPower":        true,
		"sourceVersion":            s.info.SrcVersion,
		"keepAliveSendStatsAsBody": true,
		"model":                    s.info.Model,
		"initialVolume":            float64(0.0),
		"audiotxl":                 []any{},
		"audioLatencies": []any{
			map[string]any{
				"type": uint64(100), "audioType": "default",
				"inputLatencyMicros": uint64(0), "outputLatencyMicros": false,
			},
			map[string]any{
				"type": uint64(101), "audioType": "default",
				"inputLatencyMicros": uint64(0), "outputLatencyMicros": false,
			},
		},
		"audioFormats": []any{
			map[string]any{
				"type":               uint64(100),
				"audioOutputFormats": uint64(0x3fffffc),
				"audioInputFormats":  uint64(0x3fffffc),
			},
			map[string]any{
				"type":               uint64(101),
				"audioOutputFormats": uint64(0x3fffffc),
				"audioInputFormats":  uint64(0x3fffffc),
			},
		},
		"displays": []any{
			map[string]any{
				"uuid":           disp.UUID,
				"widthPhysical":  uint64(0),
				"heightPhysical": uint64(0),
				"width":          uint64(disp.Width),
				"height":         uint64(disp.Height),
				"widthPixels":    uint64(disp.Width),
				"heightPixels":   uint64(disp.Height),
				"rotation":       false,
				// UxPlay sends the frame interval, not the rate: 1/60 reads as
				// 0.0166667. Senders interpret the field that way.
				"refreshRate": 1.0 / disp.RefreshHz,
				"maxFPS":      uint64(disp.MaxFPS),
				"overscanned": disp.Overscanned,
				"features":    uint64(14),
			},
		},
	}

	// The pairing public key is only present once the pairing identity exists.
	if pk := publicKeyBytes(s.info.PublicKey); len(pk) > 0 {
		props["pk"] = pk
	}

	return plistResponse(props)
}

// txtRecord asks the mDNS advertiser for the published record. A missing hook
// means mDNS has not started, in which case the request falls through to the
// device dictionary rather than answering with an empty record.
func (s *Server) txtRecord(raop bool) []byte {
	if s.TXTRecord == nil {
		return nil
	}
	rec := s.TXTRecord(raop)
	if len(rec) == 0 {
		return nil
	}
	return rec
}

// txtQualifier reads the "txtAirPlay"/"txtRAOP" qualifier a modern sender puts
// in the search params of an /info request.
//
// UxPlay reads it from the search parameters (the part after '?' in the request
// line, still in plist form for this endpoint); the body is checked too because
// some versions of iOS send it as a plist dictionary instead.
func txtQualifier(req *Request) string {
	for _, src := range [][]byte{req.Body, []byte(req.Query())} {
		if len(src) == 0 {
			continue
		}
		probe := map[string]any{}
		if _, err := plist.Unmarshal(src, &probe); err != nil {
			continue
		}
		if key := firstQualifier(probe); key != "" {
			return key
		}
	}
	return ""
}

// firstQualifier pulls the first string out of a qualifier array or value.
func firstQualifier(m map[string]any) string {
	switch q := m["qualifier"].(type) {
	case string:
		if q == "txtAirPlay" || q == "txtRAOP" {
			return q
		}
	case []any:
		for _, item := range q {
			if s, ok := item.(string); ok && (s == "txtAirPlay" || s == "txtRAOP") {
				return s
			}
		}
	}
	return ""
}

// queryTXTKey recognises the ?txtAirPlay / ?txtRAOP URL form.
func queryTXTKey(query string) string {
	switch {
	case strings.Contains(query, "txtAirPlay"):
		return "txtAirPlay"
	case strings.Contains(query, "txtRAOP"):
		return "txtRAOP"
	}
	return ""
}

// handleServerInfo answers the reverse-API query for device identity.
func (s *Server) handleServerInfo() *Response {
	return plistResponse(map[string]any{
		"deviceID":      protocol.AirplayDeviceID(s.info.MAC),
		"macAddress":    protocol.AirplayDeviceID(s.info.MAC),
		"name":          s.info.Name,
		"model":         s.info.Model,
		"sourceVersion": s.info.SrcVersion,
	})
}

// handleGetParameter answers the status queries a sender makes while mirroring.
// UxPlay rejects only a missing Content-Type; a present type it does not know is
// accepted, because senders vary in what they send.
func (s *Server) handleGetParameter(req *Request) *Response {
	switch req.Header("Content-Type") {
	case "":
		return &Response{Status: 451}
	case "text/parameters":
		// A text body requests key/value lines; the only key we track is volume.
		return &Response{ContentType: "text/parameters", Body: []byte("volume: 0.000000\r\n")}
	}
	return &Response{}
}

// handleSetParameter accepts volume and scrub updates.
func (s *Server) handleSetParameter(req *Request) *Response {
	if req.Header("Content-Type") == "" {
		return &Response{Status: 451}
	}
	if req.Header("Content-Type") != "text/parameters" {
		return &Response{}
	}
	body := req.Body
	switch {
	case bytes.HasPrefix(body, []byte("volume:")):
		if len(body) <= 64 {
			v, err := strconv.ParseFloat(string(bytes.TrimSpace(body[len("volume:"):])), 64)
			if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
				log.Printf("ipc: sender set scene volume %g", v)
			}
		}
	case bytes.HasPrefix(body, []byte("progress:")):
		// Live mirroring has no scrubbing; accept and ignore.
	default:
		// Never echo the (up to 4 MiB) body to the log.
		log.Printf("ipc: unhandled set_parameter (%d bytes)", len(body))
	}
	return &Response{}
}

// plistResponse renders a binary plist reply.
func plistResponse(m map[string]any) *Response {
	b, err := plist.Marshal(m, plist.BinaryFormat)
	if err != nil {
		log.Printf("ipc: plist encode: %v", err)
		return &Response{Status: 500}
	}
	return &Response{ContentType: "application/x-apple-binary-plist", Body: b}
}

// publicKeyBytes decodes the hex public key advertised over mDNS.
func publicKeyBytes(hexKey string) []byte {
	if hexKey == "" {
		return nil
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil
	}
	return raw
}

func featuresUint64() uint64 {
	lo, hi := protocol.Features()
	return uint64(hi)<<32 | uint64(lo)
}

var _ = strconv.Itoa
