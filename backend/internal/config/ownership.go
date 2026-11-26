package config

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrReceiverRunning = errors.New("這個設定資料夾已有接收器在使用，請先關閉原來的鏡屏視窗")

// LockReceiver grants exclusive use of the on-disk receiver identity until
// Close or process exit. Keep the lock file in place so all contenders lock the
// same file, including when config.json is atomically replaced or reset.
func LockReceiver(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "receiver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockIdentityFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
