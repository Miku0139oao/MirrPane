//go:build !windows

package config

import (
	"encoding/base64"
	"errors"
)

const dpapiProtectAtRest = false

func protectKeySeed(seed []byte) (string, error) {
	return base64.StdEncoding.EncodeToString(seed), nil
}

func unprotectKeySeed(string) ([]byte, error) {
	return nil, errors.New("config: dpapi keySeed cannot be decrypted on this platform")
}
