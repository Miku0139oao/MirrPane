package mdns

import (
	"strings"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// buildTXT reproduces the record bytes Start would publish, without needing a
// live multicast socket.
func testAdvertiser(pin bool) *Advertiser {
	info := protocol.NewDeviceInfo("映屏", "AA:BB:CC:DD:EE:FF")
	info.PairingID = "0000"
	info.PublicKey = "deadbeef"
	return &Advertiser{name: "映屏", port: 7000, device: info, pinPW: pin}
}

func TestTXTRecordWireFormat(t *testing.T) {
	a := testAdvertiser(false)
	// Populate the record fields the way Start does.
	a.airplay = []string{
		"deviceid=" + protocol.AirplayDeviceID(a.device.MAC),
		"features=" + protocol.FeaturesString(),
		"pw=false",
		"pk=" + a.device.PublicKey,
	}
	a.raopTXT = []string{"txtvers=1", "deviceid=" + protocol.RAOPDeviceID(a.device.MAC)}

	rec := a.TXTRecord(false)
	// Every entry must be length-prefixed: parse the blob back.
	got, ok := parseTXT(rec)
	if !ok {
		t.Fatalf("record is not well-formed: %q", rec)
	}
	if got["deviceid"] != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("deviceid = %q, want the lowercase _airplay form", got["deviceid"])
	}
	if got["pw"] != "false" {
		t.Fatalf("pw = %q", got["pw"])
	}

	raop, _ := parseTXT(a.TXTRecord(true))
	if raop["deviceid"] != "AABBCCDDEEFF" {
		t.Fatalf("raop deviceid = %q, want the uppercase separator-free form", raop["deviceid"])
	}
}

func TestDeviceIDForms(t *testing.T) {
	if got := protocol.AirplayDeviceID("AA:BB:CC:DD:EE:FF"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("airplay form = %q", got)
	}
	if got := protocol.RAOPDeviceID("aa:bb:cc:dd:ee:ff"); got != "AABBCCDDEEFF" {
		t.Fatalf("raop form = %q", got)
	}
}

// parseTXT decodes DNS-SD TXT wire format into a map.
func parseTXT(b []byte) (map[string]string, bool) {
	out := make(map[string]string)
	for len(b) > 0 {
		n := int(b[0])
		if n == 0 || 1+n > len(b) {
			return nil, false
		}
		kv := string(b[1 : 1+n])
		b = b[1+n:]
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		} else {
			out[kv] = ""
		}
	}
	return out, true
}
