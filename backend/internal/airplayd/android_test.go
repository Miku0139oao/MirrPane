package airplayd

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/receiver"
	"github.com/miku0139/airplay-go/backend/internal/scrcpy"
)

// An Android session owns the display while AirPlay is idle: /api/state
// reports the phone as the live source so the UI needs no second state model.
func TestAndroidLiveStateOverridesIdleAPI(t *testing.T) {
	state := newRuntimeState()
	am := newAndroidManager(log.New(io.Discard, "", 0), nil, state, nil)
	am.phase = androidLive
	am.device = "Pixel 9"
	am.serial = "R58M12345"
	am.width, am.height = 1080, 2400

	info := protocol.NewDeviceInfo("鏡屏", "AA:BB:CC:DD:EE:FF")
	cfg := &config.Config{Name: "鏡屏", Width: 1920, Height: 1080, MaxFPS: 60}
	h := apiHandler(info, 7000, cfg, state, nil, nil, nil, "", am)

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "127.0.0.1:54321"
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d %s", rec.Code, rec.Body.String())
	}
	var body apiState
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "android" || body.Status != "live" || body.Sender != "Pixel 9" ||
		body.Width != 1080 || body.Height != 2400 {
		t.Fatalf("android live state not exposed: %+v", body)
	}
	if body.Android == nil || body.Android.Phase != androidLive || body.Android.Serial != "R58M12345" {
		t.Fatalf("android object missing: %+v", body.Android)
	}

	// An AirPlay session outranks the Android object.
	state.set(receiver.StatusLive, "")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "127.0.0.1:54321"
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)
	body = apiState{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "" || body.Sender == "Pixel 9" {
		t.Fatalf("android object overrode a live AirPlay session: %+v", body)
	}
}

func TestAndroidConnectRefusedWhileAirPlayLive(t *testing.T) {
	state := newRuntimeState()
	state.set(receiver.StatusLive, "")
	am := newAndroidManager(log.New(io.Discard, "", 0), nil, state, nil)
	if err := am.Connect("R58M12345", scrcpy.Options{}); err == nil {
		t.Fatal("connect was accepted during an AirPlay session")
	}
	if am.Active() {
		t.Fatal("a refused connect left the manager active")
	}
}

func TestAndroidInputValidators(t *testing.T) {
	for _, serial := range []string{"R58M12345", "192.168.1.7:5555", "emulator-5554"} {
		if err := validAndroidSerial(serial); err != nil {
			t.Fatalf("serial %q rejected: %v", serial, err)
		}
	}
	for _, serial := range []string{"", "-s evil", "has space", "x;rm", string([]byte{0x7f})} {
		if err := validAndroidSerial(serial); err == nil {
			t.Fatalf("serial %q accepted", serial)
		}
	}
	for _, address := range []string{"192.168.1.7:5555", "host.local:37000"} {
		if err := validAndroidAddress(address); err != nil {
			t.Fatalf("address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"", "192.168.1.7", "-h:1", "a b:12", "host:-1", "host:999999"} {
		if err := validAndroidAddress(address); err == nil {
			t.Fatalf("address %q accepted", address)
		}
	}
}

func TestAndroidInputNeedsLiveSession(t *testing.T) {
	am := newAndroidManager(log.New(io.Discard, "", 0), nil, newRuntimeState(), nil)
	if err := am.Input(androidInput{Kind: "back"}); err == nil {
		t.Fatal("input reached a session that does not exist")
	}
}

func TestPCM16LEConversion(t *testing.T) {
	got := pcm16le([]byte{0x34, 0x12, 0xfe, 0xff, 0x00, 0x80})
	want := []int16{0x1234, -2, -32768}
	if len(got) != len(want) {
		t.Fatalf("samples = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}
