package audio

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

// encryptPayload mirrors what a sender does: AES-128-CBC over the block-aligned
// prefix, with the remainder left in the clear.
func encryptPayload(t *testing.T, key, iv, plain []byte) []byte {
	t.Helper()
	encLen := len(plain) / aes.BlockSize * aes.BlockSize
	out := append([]byte(nil), plain...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[:encLen], plain[:encLen])
	return out
}

func TestCBCDecryptRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 16)
	iv := bytes.Repeat([]byte{0x22}, 16)

	// 0x8c starts a valid AAC-ELD frame. 37 bytes means 32 encrypted and 5 in
	// the clear, which is exactly the case the reference handles specially.
	plain := make([]byte, 37)
	plain[0] = 0x8c
	for i := 1; i < len(plain); i++ {
		plain[i] = byte(i * 7)
	}
	enc := encryptPayload(t, key, iv, plain)

	got := cbcDecrypt(key, iv, enc)
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip failed\n got % x\nwant % x", got, plain)
	}
	if !plausibleAAC(got) {
		t.Fatal("decrypted frame was not recognised")
	}
}

// Each packet restarts the CBC state at the fixed IV. If the state carried over,
// the second packet would decrypt to noise.
func TestCBCDecryptRestartsPerPacket(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 16)
	iv := bytes.Repeat([]byte{0x44}, 16)

	plain := make([]byte, 32)
	plain[0] = 0x8e
	for i := 1; i < len(plain); i++ {
		plain[i] = byte(i)
	}
	enc := encryptPayload(t, key, iv, plain)

	for i := 0; i < 3; i++ {
		if got := cbcDecrypt(key, iv, enc); !bytes.Equal(got, plain) {
			t.Fatalf("packet %d decrypted differently: % x", i, got)
		}
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 16)
	iv := bytes.Repeat([]byte{0x66}, 16)
	plain := make([]byte, 32)
	plain[0] = 0x8c
	enc := encryptPayload(t, key, iv, plain)

	wrong := bytes.Repeat([]byte{0x99}, 16)
	got := cbcDecrypt(wrong, iv, enc)
	if bytes.Equal(got, plain) {
		t.Fatal("the wrong key produced the plaintext")
	}
	if plausibleAAC(got) {
		t.Fatal("the wrong key produced a frame that looked valid")
	}
}

func TestPlausibleAAC(t *testing.T) {
	for _, head := range []byte{0x8c, 0x8d, 0x8e, 0x80, 0x81, 0x82, 0x20} {
		if !plausibleAAC([]byte{head, 1, 2, 3}) {
			t.Fatalf("0x%02x should be accepted", head)
		}
	}
	for _, head := range []byte{0x00, 0x41, 0xff, 0x13} {
		if plausibleAAC([]byte{head, 1, 2, 3}) {
			t.Fatalf("0x%02x should be rejected", head)
		}
	}
	if plausibleAAC(nil) {
		t.Fatal("empty input accepted")
	}
}
