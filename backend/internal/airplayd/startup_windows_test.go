package airplayd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

func TestParentMonitorRejectsInvalidOrMissingParent(t *testing.T) {
	if err := watchParent(0); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{-1, 0x7fffffff} {
		if err := watchParent(pid); err == nil {
			t.Fatalf("started without requested parent %d", pid)
		}
	}
}

func TestEffectiveDisplayMatchesAPI(t *testing.T) {
	cfg := &config.Config{Width: 1280, Height: 720, MaxFPS: 30}
	display := effectiveDisplay(cfg, 2560, 1178, 60)
	handler := apiHandler(protocol.NewDeviceInfo("Test", "00:00:00:00:00:01"), 7000, cfg, newRuntimeState(), nil, nil, nil, "")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8888/api/state", nil)
	request.RemoteAddr = "127.0.0.1:55555"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var state apiState
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.TargetWidth != 2560 || state.TargetHeight != 1178 || state.MaxFPS != 60 ||
		display.Width != state.TargetWidth || display.Height != state.TargetHeight || display.MaxFPS != state.MaxFPS {
		t.Fatalf("advertised %#v, api %dx%d@%d", display, state.TargetWidth, state.TargetHeight, state.MaxFPS)
	}
}

func TestReceiverLogBoundsFilesAndLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receiver.log")
	l, err := openReceiverLog(path, 12<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := bytes.Repeat([]byte("a"), 9<<10)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if n, err := l.Write(line); err != nil || n != len(line) {
				t.Errorf("write: %d, %v", n, err)
			}
		})
	}
	wg.Wait()
	if _, err := l.Write([]byte("last\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) != (8<<10)+5 || !bytes.HasSuffix(got, []byte("\nlast\n")) {
		t.Fatalf("unexpected retained log (%d bytes): %v", len(got), err)
	}
}
