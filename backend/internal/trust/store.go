// Package trust persists the pairing identities allowed to start a stream.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Miku0139oao/MirrPane/backend/internal/config"
)

// FileName is stored beside config.json, independently of config settings.
const FileName = "trusted_senders.json"

// Fingerprint identifies a public pairing key without disclosing the key.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type record struct {
	Receiver       string   `json:"receiver"`
	TrustedSenders []string `json:"trustedSenders,omitempty"`
}

// Store serializes trust decisions with persistence. A failed save never
// changes the in-memory policy. One sidecar owns the data directory.
type Store struct {
	mu   sync.Mutex
	path string
	data record
}

// Open loads trust for this receiver identity. The UI resets pairing by
// deleting config.json and restarting; the new receiver key invalidates the
// old trust list without requiring changes to the config or frontend writers.
// Malformed or unreadable files fail closed rather than reopening enrollment.
func Open(dir string, receiverPublicKey []byte) (*Store, error) {
	if len(receiverPublicKey) != 32 {
		return nil, errors.New("trust: receiver public key must be 32 bytes")
	}
	s := &Store{path: filepath.Join(dir, FileName), data: record{Receiver: Fingerprint(receiverPublicKey)}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			if err := config.RestrictDirACL(dir); err != nil {
				return nil, fmt.Errorf("trust: permissions: %w", err)
			}
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: read: %w", err)
	}
	var saved record
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("trust: parse: %w", err)
	}
	if !validFingerprint(saved.Receiver) {
		return nil, errors.New("trust: invalid receiver fingerprint")
	}
	for _, sender := range saved.TrustedSenders {
		if !validFingerprint(sender) {
			return nil, errors.New("trust: invalid sender fingerprint")
		}
	}
	if saved.Receiver != s.data.Receiver {
		if err := s.save(s.data); err != nil {
			return nil, err
		}
	} else {
		if err := config.RestrictDirACL(filepath.Dir(s.path)); err != nil {
			return nil, fmt.Errorf("trust: permissions: %w", err)
		}
		if err := os.Chmod(s.path, 0o600); err != nil {
			return nil, fmt.Errorf("trust: permissions: %w", err)
		}
		if err := config.RestrictFileACL(s.path); err != nil {
			return nil, fmt.Errorf("trust: permissions: %w", err)
		}
		s.data = saved
	}
	return s, nil
}

// IsTrustedSender checks the current policy, including revocations since pairing.
func (s *Store) IsTrustedSender(fingerprint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contains(fingerprint)
}

func (s *Store) contains(fingerprint string) bool {
	for _, sender := range s.data.TrustedSenders {
		if sender == fingerprint {
			return true
		}
	}
	return false
}

// TrustSender enrolls a sender identity: automatically on first use, or after
// the owner approves it in manual approval mode.
func (s *Store) TrustSender(fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validFingerprint(fingerprint) {
		return errors.New("trust: invalid sender fingerprint")
	}
	if s.contains(fingerprint) {
		return nil
	}
	next := record{Receiver: s.data.Receiver, TrustedSenders: append(append([]string(nil), s.data.TrustedSenders...), fingerprint)}
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// ClearTrustedSenders durably clears every trusted identity.
func (s *Store) ClearTrustedSenders() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := record{Receiver: s.data.Receiver}
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func validFingerprint(fingerprint string) bool {
	hexValue, ok := strings.CutPrefix(fingerprint, "sha256:")
	if !ok || len(hexValue) != 64 || hexValue != strings.ToLower(hexValue) {
		return false
	}
	_, err := hex.DecodeString(hexValue)
	return err == nil
}

// save uses an owner-only temporary file in the destination directory and an
// atomic rename, applying 0600 permissions and the platform protected DACL.
func (s *Store) save(next record) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("trust: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("trust: directory: %w", err)
	}
	if err := config.RestrictDirACL(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("trust: permissions: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".trusted-senders-*")
	if err != nil {
		return fmt.Errorf("trust: create: %w", err)
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("trust: permissions: %w", err)
	}
	if err := config.RestrictFileACL(temp); err != nil {
		return fmt.Errorf("trust: permissions: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("trust: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("trust: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("trust: close: %w", err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("trust: replace: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("trust: permissions: %w", err)
	}
	if err := config.RestrictFileACL(s.path); err != nil {
		return fmt.Errorf("trust: permissions: %w", err)
	}
	return nil
}
