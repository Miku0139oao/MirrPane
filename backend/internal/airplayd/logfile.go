package airplayd

import (
	"io"
	"os"
	"sync"
)

// receiverLog starts a new log at a line boundary when the current one fills.
// Both loggers share this writer, including on systems without a console.
type receiverLog struct {
	mu          sync.Mutex
	file        *os.File
	limit, size int64
}

func openReceiverLog(path string, limit int64) (*receiverLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &receiverLog{file: f, limit: limit}, nil
}

func (l *receiverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	want := len(p)
	if capLine := min(l.limit, 8<<10); int64(want) > capLine {
		// Preserve a record boundary even when we abbreviate an oversized line.
		p = append(append([]byte(nil), p[:capLine-1]...), '\n')
	}
	if l.size+int64(len(p)) > l.limit {
		if _, err := l.file.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if err := l.file.Truncate(0); err != nil {
			return 0, err
		}
		l.size = 0
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	if err != nil {
		return n, err
	}
	return want, nil
}

func (l *receiverLog) Close() error { return l.file.Close() }
