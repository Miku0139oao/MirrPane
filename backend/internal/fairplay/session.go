// Package fairplay implements the legacy FairPlay key exchange (protocol
// version 3) that AirPlay screen mirroring uses to wrap the stream's AES key.
//
// The mathematics lives in C, ported from UxPlay's lib/playfair (GPLv3), because
// it is a large table-driven cipher whose tables are best kept byte-identical to
// the reference. This file owns the wire protocol: the three /fp-setup
// exchanges and the derived key.
package fairplay

/*
#include <stdint.h>

void playfair_decrypt(unsigned char* message3, unsigned char* cipherText, unsigned char* keyOut);
*/
import "C"

import (
	"errors"
	"unsafe"
)

const (
	// SetupRequestLen is the size of the first /fp-setup request body.
	SetupRequestLen = 16
	// SetupReplyLen is the size of the first /fp-setup reply.
	SetupReplyLen = 142
	// HandshakeRequestLen is the size of the second /fp-setup request body.
	HandshakeRequestLen = 164
	// HandshakeReplyLen is the size of the second /fp-setup reply.
	HandshakeReplyLen = 32
	// EKeyLen is the size of the EKEY blob found in the /setup plist.
	EKeyLen = 72
	// AESKeyLen is the AES key length the receiver derives.
	AESKeyLen = 16
)

var (
	// ErrMalformed reports a request that cannot be a FairPlay message.
	ErrMalformed = errors.New("fairplay: malformed request")
	// ErrUnsupportedVersion reports FairPlay v2, which this implementation does
	// not speak yet.
	ErrUnsupportedVersion = errors.New("fairplay: unsupported protocol version")
	// ErrOutOfOrder reports a decryption attempt before the handshake finished.
	ErrOutOfOrder = errors.New("fairplay: handshake not completed")
)

// Version identifies the FairPlay protocol revision of a connection.
type Version int

// The versions a client can announce in byte 4 of /fp-setup.
const (
	VersionUnknown Version = 0
	Version2       Version = 2
	Version3       Version = 3
)

// Session holds the FairPlay state of one client connection. A receiver that
// serves several devices keeps one Session per connection; the state is small
// and must not be shared.
type Session struct {
	version Version
	keyMsg  [HandshakeRequestLen]byte
}

// NewSession returns a session ready for the first /fp-setup request.
func NewSession() *Session { return &Session{} }

// Version reports the protocol revision negotiated so far.
func (s *Session) Version() Version { return s.version }

// Setup answers the first /fp-setup request (16 bytes) and returns the 142-byte
// reply. The reply is one of four fixed messages chosen by the client's mode
// byte, which is why it can be a table lookup.
func (s *Session) Setup(req []byte) ([]byte, error) {
	if len(req) != SetupRequestLen {
		return nil, ErrMalformed
	}
	switch req[4] {
	case 0x02:
		// FairPlay v2 needs the separate fairplay_v2 exchange.
		s.version = Version2
		return nil, ErrUnsupportedVersion
	case 0x03:
		if req[14] >= 4 {
			return nil, ErrMalformed
		}
		s.version = Version3
		reply := make([]byte, SetupReplyLen)
		copy(reply, setupReplies[req[14]][:])
		return reply, nil
	default:
		return nil, ErrUnsupportedVersion
	}
}

// Handshake answers the second /fp-setup request (164 bytes) and returns the
// 32-byte reply. The request itself is the "key message" that later decrypts
// the stream key, so it is kept until the connection ends.
func (s *Session) Handshake(req []byte) ([]byte, error) {
	if len(req) != HandshakeRequestLen {
		return nil, ErrMalformed
	}
	if req[4] != 0x03 || req[12] >= 4 {
		s.reset()
		return nil, ErrMalformed
	}
	s.reset()
	s.version = Version3
	copy(s.keyMsg[:], req)

	reply := make([]byte, HandshakeReplyLen)
	copy(reply, fpHeader[:])
	copy(reply[len(fpHeader):], req[144:164])
	return reply, nil
}

// DecryptKey unwraps the 72-byte EKEY blob from the /setup plist into the
// 16-byte AES key. The key feeds AES-CTR for both audio and video.
func (s *Session) DecryptKey(ekey []byte) ([]byte, error) {
	if len(ekey) != EKeyLen {
		return nil, ErrMalformed
	}
	if s.version != Version3 {
		return nil, ErrOutOfOrder
	}
	out := make([]byte, AESKeyLen)
	C.playfair_decrypt(
		(*C.uchar)(unsafe.Pointer(&s.keyMsg[0])),
		(*C.uchar)(unsafe.Pointer(&ekey[0])),
		(*C.uchar)(unsafe.Pointer(&out[0])),
	)
	return out, nil
}

// Reset clears the session so the next /fp-setup starts fresh.
func (s *Session) Reset() { s.reset() }

func (s *Session) reset() {
	s.version = VersionUnknown
	for i := range s.keyMsg {
		s.keyMsg[i] = 0
	}
}
