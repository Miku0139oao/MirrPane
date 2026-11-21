// Package pairing implements the AirPlay control-channel pairing the way UxPlay
// does (lib/pairing.c, GPLv3): a /pair-setup that hands out this receiver's
// Ed25519 public key, then a two-step /pair-verify in which both sides prove
// possession of their keys over an X25519 shared secret.
//
// Nothing here is TLV8: the bodies are fixed-size binary records. That is what
// the reference implementation accepts from real iOS and macOS senders.
package pairing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
)

// Sizes fixed by the protocol.
const (
	// Ed25519KeyLen is the length of an Ed25519 public key.
	Ed25519KeyLen = 32
	// X25519KeyLen is the length of an X25519 public key and of a shared secret.
	X25519KeyLen = 32
	// SignatureLen is the length of an Ed25519 signature.
	SignatureLen = 64
	// SetupRequestLen is the size of the /pair-setup body.
	SetupRequestLen = 32
	// SetupReplyLen is the size of the /pair-setup reply: our public key.
	SetupReplyLen = Ed25519KeyLen
	// VerifyHeaderLen is the four-byte type prefix on each /pair-verify body.
	VerifyHeaderLen = 4
	// VerifyStep1Len is header + X25519 key + Ed25519 key.
	VerifyStep1Len = VerifyHeaderLen + X25519KeyLen + Ed25519KeyLen
	// VerifyStep2Len is header + signature.
	VerifyStep2Len = VerifyHeaderLen + SignatureLen
)

// Salt strings are hashed with the shared secret to derive the signature keys.
const (
	saltKey = "Pair-Verify-AES-Key"
	saltIV  = "Pair-Verify-AES-IV"
)

var (
	// ErrMalformed reports a body that cannot be a pairing record.
	ErrMalformed = errors.New("pairing: malformed request")
	// ErrBadSignature reports a signature that did not verify.
	ErrBadSignature = errors.New("pairing: signature verification failed")
	// ErrOutOfOrder reports a step that does not follow the session state.
	ErrOutOfOrder = errors.New("pairing: unexpected pairing step")
	// ErrMissingKey reports a session without a pairing session ready.
	ErrMissingKey = errors.New("pairing: no pairing session; run pair-setup first")
)

// Keys is the receiver's long-lived Ed25519 identity. It is generated once and
// stored so that a device that has paired keeps recognising this receiver.
type Keys struct {
	private ed25519.PrivateKey
}

// NewKeys creates a fresh identity.
func NewKeys() (*Keys, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Keys{private: priv}, nil
}

// KeysFromSeed rebuilds an identity from a 32-byte seed.
func KeysFromSeed(seed []byte) (*Keys, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("pairing: seed must be %d bytes", ed25519.SeedSize)
	}
	return &Keys{private: ed25519.NewKeyFromSeed(seed)}, nil
}

// Seed returns the 32-byte seed, used to persist the identity.
func (k *Keys) Seed() []byte { return append([]byte(nil), k.private.Seed()...) }

// PublicKey returns the 32-byte Ed25519 public key.
func (k *Keys) PublicKey() []byte {
	return append([]byte(nil), k.private.Public().(ed25519.PublicKey)...)
}

// PublicKeyHex renders the public key as the mDNS `pk` TXT value wants it.
func (k *Keys) PublicKeyHex() string { return hex.EncodeToString(k.PublicKey()) }

// sessionStatus tracks how far a pairing session has progressed.
type sessionStatus int

const (
	statusInitial sessionStatus = iota
	statusSetup
	statusHandshake
	statusFinished
)

// Session is the pairing state of one sender.
type Session struct {
	keys   *Keys
	status sessionStatus

	edTheirs   ed25519.PublicKey
	ecdhOurs   *ecdh.PrivateKey
	ecdhTheirs []byte
	secret     []byte
}

// NewSession starts pairing for one sender.
func NewSession(keys *Keys) *Session {
	return &Session{keys: keys, status: statusInitial}
}

// Setup answers /pair-setup. The request is ignored, exactly as in the
// reference: it only tells the sender who we are.
func (s *Session) Setup(req []byte) ([]byte, error) {
	if len(req) != SetupRequestLen {
		return nil, ErrMalformed
	}
	s.status = statusSetup
	return s.keys.PublicKey(), nil
}

// HandshakeStatus reports whether pair-verify may proceed.
func (s *Session) HandshakeStatus() error {
	if s.status == statusSetup || s.status == statusHandshake {
		return nil
	}
	return ErrMissingKey
}

// Verify processes one /pair-verify body and returns the reply.
//
// Step 1 (header 0x01) carries the sender's X25519 and Ed25519 public keys and
// is answered with our X25519 public key followed by our encrypted signature.
// Step 2 (header 0x00) carries the sender's signature and is answered with an
// empty body.
func (s *Session) Verify(req []byte) ([]byte, error) {
	if err := s.HandshakeStatus(); err != nil {
		return nil, err
	}
	if len(req) < VerifyHeaderLen {
		return nil, ErrMalformed
	}
	switch req[0] {
	case 1:
		if len(req) != VerifyStep1Len {
			return nil, ErrMalformed
		}
		return s.verifyStep1(req[VerifyHeaderLen:])
	case 0:
		if len(req) != VerifyStep2Len {
			return nil, ErrMalformed
		}
		return nil, s.verifyStep2(req[VerifyHeaderLen:])
	default:
		return nil, ErrMalformed
	}
}

func (s *Session) verifyStep1(body []byte) ([]byte, error) {
	theirECDH := body[:X25519KeyLen]
	theirEd := body[X25519KeyLen : X25519KeyLen+Ed25519KeyLen]

	curve := ecdh.X25519()
	ourKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	peerKey, err := curve.NewPublicKey(theirECDH)
	if err != nil {
		return nil, fmt.Errorf("pairing: bad X25519 key: %w", err)
	}
	secret, err := ourKey.ECDH(peerKey)
	if err != nil {
		return nil, fmt.Errorf("pairing: ECDH failed: %w", err)
	}

	s.ecdhOurs = ourKey
	s.ecdhTheirs = append([]byte(nil), theirECDH...)
	s.edTheirs = append(ed25519.PublicKey(nil), theirEd...)
	s.secret = secret
	s.status = statusHandshake

	// The signature covers our key followed by theirs.
	message := append(append([]byte(nil), ourKey.PublicKey().Bytes()...), theirECDH...)
	signature := ed25519.Sign(s.keys.private, message)

	stream := s.signatureStream()
	enc := make([]byte, SignatureLen)
	stream.XORKeyStream(enc, signature)

	reply := make([]byte, 0, X25519KeyLen+SignatureLen)
	reply = append(reply, ourKey.PublicKey().Bytes()...)
	reply = append(reply, enc...)
	return reply, nil
}

func (s *Session) verifyStep2(body []byte) error {
	if s.status != statusHandshake {
		return ErrOutOfOrder
	}
	stream := s.signatureStream()
	// Our own signature occupies the first 64 bytes of the keystream.
	filler := make([]byte, SignatureLen)
	stream.XORKeyStream(filler, filler)

	plain := make([]byte, SignatureLen)
	stream.XORKeyStream(plain, body)

	// Their signature covers their key followed by ours.
	message := append(append([]byte(nil), s.ecdhTheirs...), s.ecdhOurs.PublicKey().Bytes()...)
	if !ed25519.Verify(s.edTheirs, message, plain) {
		return ErrBadSignature
	}
	s.status = statusFinished
	return nil
}

// Secret returns the X25519 shared secret once step 1 has run. /setup hashes the
// FairPlay key with it when pairing is in use.
func (s *Session) Secret() []byte {
	if s.status == statusInitial || len(s.secret) == 0 {
		return nil
	}
	return append([]byte(nil), s.secret...)
}

// signatureStream builds the AES-CTR stream both signatures share, keyed from
// the ECDH secret as described in UxPlay's derive_key_internal.
func (s *Session) signatureStream() cipher.Stream {
	key := derive(append([]byte(saltKey), s.secret...), 16)
	iv := derive(append([]byte(saltIV), s.secret...), 16)
	block, _ := aes.NewCipher(key) // 16-byte key cannot fail
	return cipher.NewCTR(block, iv)
}

// derive is SHA-512 over the material, truncated to n bytes.
func derive(material []byte, n int) []byte {
	sum := sha512.Sum512(material)
	return sum[:n]
}
