//go:build unix

package config

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockIdentityFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrReceiverRunning
	}
	return err
}
