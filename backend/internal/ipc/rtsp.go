package ipc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Request is one parsed AirPlay control message.
//
// AirPlay speaks RTSP/1.0 for the control channel and HTTP/1.1 for the handful
// of endpoints that belong to its "reverse" web API. Both have the same shape,
// so one parser covers them. Go's net/http cannot be used here: it rejects the
// RTSP/1.0 request line outright, which is why an iOS sender's very first
// request never reached the handler.
type Request struct {
	Method   string // GET, POST, SETUP, OPTIONS, SET_PARAMETER, ...
	Target   string // raw request target, e.g. "/info?txtAirPlay"
	Protocol string // "RTSP/1.0" or "HTTP/1.1"
	Headers  map[string]string
	Body     []byte

	// Close is set when the sender asked to end the connection.
	Close bool
}

// Path is the request target without its query string.
func (r *Request) Path() string {
	if i := strings.IndexByte(r.Target, '?'); i >= 0 {
		return r.Target[:i]
	}
	return r.Target
}

// Query is the request target's query string, without the '?'.
func (r *Request) Query() string {
	if i := strings.IndexByte(r.Target, '?'); i >= 0 {
		return r.Target[i+1:]
	}
	return ""
}

// Header looks a header up case-insensitively.
func (r *Request) Header(name string) string {
	if v, ok := r.Headers[strings.ToLower(name)]; ok {
		return v
	}
	return ""
}

const (
	// maxHeaderBytes bounds one control message header block.
	maxHeaderBytes = 32 << 10
	// maxBodyBytes bounds one control message body.
	maxBodyBytes = 4 << 20
	// readTimeout bounds how long a sender may stall mid-request.
	readTimeout = 30 * time.Second
	// idleTimeout closes connections that have gone quiet.
	idleTimeout = 5 * time.Minute
)

// errBadRequest reports a syntactically invalid control message.
var errBadRequest = errors.New("ipc: malformed request")

// readRequest parses one control message off conn.
func readRequest(r *bufio.Reader) (*Request, error) {
	line, err := readLine(r, maxHeaderBytes)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errBadRequest
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: request line %q", errBadRequest, line)
	}
	req := &Request{
		Method:   parts[0],
		Target:   parts[1],
		Protocol: parts[2],
		Headers:  make(map[string]string, 16),
	}

	var total int
	for {
		hdr, err := readLine(r, maxHeaderBytes-total)
		if err != nil {
			return nil, err
		}
		if hdr == "" {
			break
		}
		total += len(hdr)
		name, value, ok := strings.Cut(hdr, ":")
		if !ok {
			return nil, fmt.Errorf("%w: header %q", errBadRequest, hdr)
		}
		req.Headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}

	if strings.EqualFold(req.Header("Connection"), "close") {
		req.Close = true
	}

	if cl := req.Header("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, fmt.Errorf("%w: Content-Length %q", errBadRequest, cl)
		}
		if n > 0 {
			req.Body = make([]byte, n)
			if _, err := io.ReadFull(r, req.Body); err != nil {
				return nil, err
			}
		}
	}

	// A body that was not announced by Content-Length would desynchronise the
	// stream, so a sender that streams plists always declares it.
	return req, nil
}

// readLine reads one CRLF-terminated line, tolerating a bare LF.
func readLine(r *bufio.Reader, limit int) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimSuffix(string(buf), "\r"), nil
		}
		buf = append(buf, b)
		if len(buf) > limit {
			return "", fmt.Errorf("%w: header line too long", errBadRequest)
		}
	}
}

// Response is a control message reply.
type Response struct {
	Status      int
	ContentType string
	Headers     []header
	Body        []byte
	// Disconnect ends the connection after this reply.
	Disconnect bool
}

type header struct{ name, value string }

// status resolves the reply's status code. A handler that leaves Status at zero
// means 200, which is the AirPlay default; the value is filled in on write.
func (r *Response) status() int {
	if r.Status == 0 {
		return 200
	}
	return r.Status
}

// AddHeader appends a header.
func (r *Response) AddHeader(name, value string) {
	r.Headers = append(r.Headers, header{name, value})
}

// logTarget bounds the request target echoed to the log so a crafted long
// target cannot inflate the log file.
func logTarget(target string) string {
	const max = 120
	if len(target) <= max {
		return target
	}
	return target[:max] + "…"
}

// statusText renders the reason phrase AirPlay senders expect to see.
func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 101:
		return "Switching Protocols"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 429:
		return "Too Many Requests"
	case 431:
		return "Request Header Fields Too Large"
	case 451:
		return "Parameter not understood"
	case 453:
		return "Not Enough Bandwidth"
	case 470:
		return "Connection Authorization Required"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	default:
		return "OK"
	}
}

// write serialises the reply on conn.
//
// UxPlay echoes the sender's CSeq on every RTSP reply and identifies itself as
// iTunes/AirTunes; iOS treats a reply without the echoed CSeq as a protocol
// error and abandons the connection.
func (r *Response) write(w *bufio.Writer, req *Request) error {
	protocol := req.Protocol
	if protocol == "" {
		protocol = "RTSP/1.0"
	}
	if r.Status == 0 {
		r.Status = 200
	}
	if _, err := fmt.Fprintf(w, "%s %d %s\r\n", protocol, r.Status, statusText(r.Status)); err != nil {
		return err
	}
	for _, h := range r.Headers {
		if _, err := fmt.Fprintf(w, "%s: %s\r\n", h.name, h.value); err != nil {
			return err
		}
	}
	if r.ContentType != "" {
		if _, err := fmt.Fprintf(w, "Content-Type: %s\r\n", r.ContentType); err != nil {
			return err
		}
	}
	if cseq := req.Header("CSeq"); cseq != "" {
		if _, err := fmt.Fprintf(w, "CSeq: %s\r\n", cseq); err != nil {
			return err
		}
		// UxPlay reports a connected digital audio jack on every CSeq-bearing
		// reply except RECORD, which sets its own. Senders use it to decide
		// whether audio will be played through a real output.
		if req.Method != "RECORD" {
			if _, err := io.WriteString(w, "Audio-Jack-Status: connected; type=digital\r\n"); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w, "Server: AirTunes/%s\r\n", serverVersion); err != nil {
		return err
	}
	if len(r.Body) > 0 || r.ContentType != "" {
		if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n", len(r.Body)); err != nil {
			return err
		}
	}
	if r.Disconnect {
		if _, err := io.WriteString(w, "Connection: close\r\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\r\n"); err != nil {
		return err
	}
	if len(r.Body) > 0 {
		if _, err := w.Write(r.Body); err != nil {
			return err
		}
	}
	return w.Flush()
}

// allocConnKey assigns a distinct key for one control connection and returns
// both the connection key and the stripped peer IP. TCP connections retain
// their "ip:port" / "[ipv6]:port" address unless an identical address is still
// active; portless transports such as net.Pipe receive a monotonic "#N" suffix.
func (s *Server) allocConnKey(raw string) (connKey, remoteIP string) {
	remoteIP = ipOf(raw)
	if host, port, err := net.SplitHostPort(raw); err == nil && port != "" && port != "0" {
		remoteIP = host
		s.connMu.Lock()
		if s.activeConns == nil {
			s.activeConns = make(map[string]struct{})
		}
		if _, exists := s.activeConns[raw]; !exists {
			s.activeConns[raw] = struct{}{}
			s.connMu.Unlock()
			return raw, remoteIP
		}
		s.connMu.Unlock()
	}

	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.activeConns == nil {
		s.activeConns = make(map[string]struct{})
	}
	for {
		s.connSeq++
		candidate := fmt.Sprintf("%s#%d", raw, s.connSeq)
		if _, exists := s.activeConns[candidate]; !exists {
			s.activeConns[candidate] = struct{}{}
			return candidate, remoteIP
		}
	}
}

func (s *Server) releaseConnKey(connKey string) {
	s.connMu.Lock()
	delete(s.activeConns, connKey)
	s.connMu.Unlock()
}

// serveConn answers every request on one connection until the sender closes it.
func (s *Server) serveConn(conn net.Conn) {
	var rawAddr string
	if addr := conn.RemoteAddr(); addr != nil {
		rawAddr = addr.String()
	}
	connKey, remoteIP := s.allocConnKey(rawAddr)
	addr, err := netip.ParseAddr(remoteIP)
	// LAN and link-local senders are valid; loopback, unspecified and in-memory
	// local connections do not own a remote receiver session.
	notifyDisconnect := err == nil && !addr.Unmap().IsLoopback() && !addr.Unmap().IsUnspecified()
	defer func() {
		_ = conn.Close()
		if notifyDisconnect && s.OnDisconnect != nil {
			s.OnDisconnect(connKey)
		}
		s.releaseConnKey(connKey)
	}()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
	}

	r := bufio.NewReaderSize(conn, 32<<10)
	w := bufio.NewWriterSize(conn, 32<<10)
	readTimeout := 10 * time.Second
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		req, err := readRequest(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// A sender that speaks something we cannot parse gets one clear
				// rejection instead of a silent hang.
				if errors.Is(err, errBadRequest) {
					resp := &Response{Status: 400}
					_ = resp.write(w, &Request{Protocol: "RTSP/1.0"})
				}
			}
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		readTimeout = idleTimeout
		start := time.Now()

		resp := s.dispatch(req, connKey)
		if err := resp.write(w, req); err != nil {
			return
		}
		log.Printf("ipc: %q %q from %s -> %d (%d ms, %d in, %d out)",
			logTarget(req.Method), logTarget(req.Target), conn.RemoteAddr(), resp.Status,
			time.Since(start).Milliseconds(), len(req.Body), len(resp.Body))
		if resp.Disconnect || req.Close {
			return
		}
	}
}

// ipOf strips any synthetic "#N" connection suffix and port from an address.
func ipOf(addr string) string {
	addr = stripConnSuffix(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func stripConnSuffix(addr string) string {
	i := strings.LastIndexByte(addr, '#')
	if i < 0 || i == len(addr)-1 {
		return addr
	}
	for j := i + 1; j < len(addr); j++ {
		if addr[j] < '0' || addr[j] > '9' {
			return addr
		}
	}
	return addr[:i]
}
