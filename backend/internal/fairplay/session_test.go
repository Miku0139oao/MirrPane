package fairplay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
)

type referenceVector struct {
	Message, EKey, Key, Block, Seed, Sap, MD5 string
}

type referenceData struct {
	Count   int
	SHA256  string
	Vectors []referenceVector
}

func readReference(t *testing.T) referenceData {
	t.Helper()
	b, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var data referenceData
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The oracles were generated with GCC from the C sources in commit 0690192,
// before the port. They cover all four modes and both intermediate hashes.
func TestPlayfairReferenceVectors(t *testing.T) {
	for i, v := range readReference(t).Vectors {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s := NewSession()
			message := unhex(t, v.Message)
			reply, err := s.Handshake(message)
			if err != nil || len(reply) != HandshakeReplyLen {
				t.Fatalf("handshake: reply=%x err=%v", reply, err)
			}
			key, err := s.DecryptKey(unhex(t, v.EKey))
			if err != nil || hex.EncodeToString(key) != v.Key {
				t.Fatalf("key=%x err=%v, want %s", key, err, v.Key)
			}
			block := unhex(t, v.Block)
			sap := sapHash(block)
			md5 := modifiedMD5(block, unhex(t, v.Seed))
			if hex.EncodeToString(sap[:]) != v.Sap || hex.EncodeToString(md5[:]) != v.MD5 {
				t.Fatalf("hashes sap=%x md5=%x, want %s %s", sap, md5, v.Sap, v.MD5)
			}
		})
	}
}

func TestPlayfairReferenceCorpus(t *testing.T) {
	data := readReference(t)
	state := uint32(0x6d697272)
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			b[i] = byte(state)
		}
		return b
	}
	h := sha256.New()
	for i := range data.Count {
		message, encrypted := random(164), random(72)
		copy(message, "FPLY\x03")
		message[12] = byte(i % 4)
		key := playfairDecrypt(message, encrypted)
		block, seed := random(64), random(16)
		sap, md5 := sapHash(block), modifiedMD5(block, seed)
		h.Write(key)
		h.Write(sap[:])
		h.Write(md5[:])
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != data.SHA256 {
		t.Fatalf("reference corpus SHA256=%s, want %s", got, data.SHA256)
	}
}

func TestSessionRequiresCompletedHandshake(t *testing.T) {
	s := NewSession()
	setup := make([]byte, SetupRequestLen)
	setup[4] = 3
	for mode := range 4 {
		setup[14] = byte(mode)
		reply, err := s.Setup(setup)
		if err != nil || len(reply) != SetupReplyLen {
			t.Fatalf("mode %d: setup reply=%x err=%v", mode, reply, err)
		}
		if _, err := s.DecryptKey(make([]byte, EKeyLen)); !errors.Is(err, ErrOutOfOrder) {
			t.Fatalf("setup alone allowed decryption: %v", err)
		}
	}
	message := unhex(t, readReference(t).Vectors[0].Message)
	if _, err := s.Handshake(message); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup(setup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecryptKey(make([]byte, EKeyLen)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("setup reused stale handshake: %v", err)
	}
	s.Reset()
	if s.Version() != VersionUnknown || s.handshake || s.keyMsg != [HandshakeRequestLen]byte{} {
		t.Fatal("reset retained session state")
	}
}

func FuzzSessionMessages(f *testing.F) {
	f.Add([]byte("FPLY\x03"))
	for mode := range 4 {
		message := make([]byte, HandshakeRequestLen)
		copy(message, "FPLY\x03")
		message[12] = byte(mode)
		f.Add(message)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		s := NewSession()
		_, _ = s.Setup(b)
		if _, err := s.Handshake(b); err == nil {
			_, _ = s.DecryptKey(b[:EKeyLen])
		}
		_, _ = s.DecryptKey(b)
	})
}
