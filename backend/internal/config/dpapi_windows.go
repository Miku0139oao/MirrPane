//go:build windows

package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	dpapiProtectAtRest = true
	// Fixed domain-separation entropy for DPAPI protection. Must never depend
	// on mutable config fields such as Name so renaming the receiver does not
	// invalidate the persisted pairing identity.
	dpapiEntropyText = "MirrPane.KeySeed.v1"
)

var (
	dpapiEntropy = []byte(dpapiEntropyText)
	dpapiDesc    = windows.StringToUTF16Ptr(dpapiEntropyText)
)

func protectKeySeed(seed []byte) (string, error) {
	blob, err := dpapiProtect(seed)
	if err != nil {
		return "", err
	}
	return dpapiPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

func unprotectKeySeed(encoded string) ([]byte, error) {
	payload, ok := strings.CutPrefix(encoded, dpapiPrefix)
	if !ok || payload == "" {
		return nil, errors.New("config: malformed dpapi keySeed")
	}
	blob, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("config: decode dpapi keySeed: %w", err)
	}
	seed, err := dpapiUnprotect(blob)
	if err != nil {
		return nil, fmt.Errorf("config: decrypt dpapi keySeed: %w", err)
	}
	return seed, nil
}

func dpapiProtect(plaintext []byte) ([]byte, error) {
	return dpapiProtectWithEntropy(plaintext, dpapiEntropy)
}

func dpapiProtectWithEntropy(plaintext, entropy []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("dpapi protect: empty plaintext")
	}
	inBlob := windows.DataBlob{
		Size: uint32(len(plaintext)),
		Data: &plaintext[0],
	}
	var entropyPtr *windows.DataBlob
	if len(entropy) > 0 {
		entropyPtr = &windows.DataBlob{
			Size: uint32(len(entropy)),
			Data: &entropy[0],
		}
	}
	var outBlob windows.DataBlob
	err := windows.CryptProtectData(
		&inBlob,
		dpapiDesc,
		entropyPtr,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&outBlob,
	)
	runtime.KeepAlive(plaintext)
	runtime.KeepAlive(entropy)
	if outBlob.Data != nil {
		defer func() {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
		}()
	}
	if err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	if outBlob.Size == 0 || outBlob.Data == nil {
		return nil, errors.New("CryptProtectData: empty output")
	}
	out := make([]byte, outBlob.Size)
	copy(out, unsafe.Slice(outBlob.Data, outBlob.Size))
	return out, nil
}

func dpapiUnprotect(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("dpapi unprotect: empty ciphertext")
	}
	inBlob := windows.DataBlob{
		Size: uint32(len(ciphertext)),
		Data: &ciphertext[0],
	}
	entropyBlob := windows.DataBlob{
		Size: uint32(len(dpapiEntropy)),
		Data: &dpapiEntropy[0],
	}
	var outBlob windows.DataBlob
	err := windows.CryptUnprotectData(
		&inBlob,
		nil,
		&entropyBlob,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&outBlob,
	)
	runtime.KeepAlive(ciphertext)
	runtime.KeepAlive(dpapiEntropy)
	if outBlob.Data != nil {
		defer func() {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
		}()
	}
	if err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	if outBlob.Size == 0 || outBlob.Data == nil {
		return nil, errors.New("CryptUnprotectData: empty output")
	}
	raw := unsafe.Slice(outBlob.Data, outBlob.Size)
	out := make([]byte, len(raw))
	copy(out, raw)
	clear(raw)
	return out, nil
}
