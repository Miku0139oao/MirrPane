// Package mdns advertises this receiver over DNS-SD so that iOS and macOS
// clients can find it in the Screen Mirroring menu.
package mdns

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/grandcat/zeroconf"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// Advertiser publishes the _airplay._tcp and _raop._tcp records.
type Advertiser struct {
	server *zeroconf.Server
	raop   *zeroconf.Server
	mu     sync.Mutex

	name    string
	port    int
	device  protocol.DeviceInfo
	pinPW   bool
	counter int

	// airplay and raopTXT are the exact key sets published, kept so the control
	// endpoint can answer /info?txtAirPlay with what a sender would have read.
	airplay []string
	raopTXT []string
}

// New creates an advertiser. port is the HTTP control port the receiver listens on.
func New(name string, port int, device protocol.DeviceInfo) *Advertiser {
	return &Advertiser{name: name, port: port, device: device}
}

// SetPinEnabled toggles whether the TXT records claim a password is required.
func (a *Advertiser) SetPinEnabled(enabled bool) {
	a.mu.Lock()
	a.pinPW = enabled
	a.mu.Unlock()
}

// Start registers the DNS-SD services and blocks until they are live or fails.
func (a *Advertiser) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.server != nil {
		return nil
	}

	// The _airplay._tcp record carries the AirPlay screen-mirroring service.
	// Its TXT keys are the ones UxPlay (GPLv3) advertises; see lib/dnssdint.h
	// and lib/mdnsd/dnssd_mdnsd.c in that project.
	airplayTXT := []string{
		"deviceid=" + protocol.AirplayDeviceID(a.device.MAC),
		"features=" + protocol.FeaturesString(),
		"pw=" + boolString(a.pinPW),
		"flags=0x4",
		"model=" + a.device.Model,
		"srcvers=" + a.device.SrcVersion,
		"vv=2",
		"pi=" + a.device.PairingID,
	}
	if a.device.PublicKey != "" {
		airplayTXT = append(airplayTXT, "pk="+a.device.PublicKey)
	}

	// The _raop._tcp record is the audio (RAOP) service. Mirroring clients on
	// modern iOS use _airplay._tcp, but HomePod-style senders and macOS still
	// probe _raop._tcp.
	features := protocol.FeaturesString()
	raopTXT := []string{
		"ch=2",
		"cn=0,1,2,3",
		"da=true",
		"et=0,3,5",
		"vv=2",
		"ft=" + features,
		"am=" + a.device.Model,
		"md=0,1,2",
		"rhd=5.6.0.0",
		"pw=" + boolString(a.pinPW),
		"sr=44100",
		"ss=16",
		"sv=false",
		"tp=UDP",
		"txtvers=1",
		"sf=0x4",
		"deviceid=" + protocol.RAOPDeviceID(a.device.MAC),
		"vs=" + a.device.SrcVersion,
		"vn=65537",
	}
	if a.device.PublicKey != "" {
		raopTXT = append(raopTXT, "pk="+a.device.PublicKey)
	}

	// Keep the published bytes for /info?txtAirPlay, which must answer with the
	// record a sender would have read over DNS-SD.
	a.airplay, a.raopTXT = airplayTXT, raopTXT

	// zeroconf can only publish one service per server, so publish _airplay._tcp
	// and _raop._tcp through two servers that share the lifetime.
	// The interface is pinned to the LAN one: advertising every adapter makes the
	// receiver offer addresses an iPhone cannot route to.
	lan := protocol.LANInterface()
	if lan == nil {
		return fmt.Errorf("mdns: no multicast-capable LAN interface with a usable IPv4 address")
	}
	ifaces := []net.Interface{*lan}

	srv, err := zeroconf.Register(
		a.name,
		"_airplay._tcp",
		"local.",
		a.port,
		airplayTXT,
		ifaces,
	)
	if err != nil {
		return fmt.Errorf("register _airplay._tcp: %w", err)
	}
	a.server = srv

	// Best effort: _raop._tcp is optional for screen mirroring.
	if raopSrv, err := zeroconf.Register(
		raopServiceName(a.device.MAC)+"@"+a.name,
		"_raop._tcp",
		"local.",
		a.port,
		raopTXT,
		ifaces,
	); err == nil {
		a.raop = raopSrv
	} else {
		log.Printf("mdns: _raop._tcp registration skipped: %v", err)
	}

	log.Printf("mdns: advertising %q on %q (index %d), port %d (features %s)", a.name, lan.Name, lan.Index, a.port, features)
	return nil
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// raopServiceName follows the AirPlay convention of <MAC>@<name>, with the MAC
// in the uppercase, separator-free RAOP form.
func raopServiceName(mac string) string {
	return protocol.RAOPDeviceID(mac)
}

// TXTRecord returns the published records in the DNS-SD wire form the control
// endpoint must echo back for /info?txtAirPlay and /info?txtRAOP: each entry is
// a length byte followed by "key=value", concatenated with no separator.
//
// This is what UxPlay puts in the DATA node of its reply; a sender that asked
// for the record accepts it in exactly this form and nothing else.
func (a *Advertiser) TXTRecord(raop bool) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := a.airplay
	if raop {
		keys = a.raopTXT
	}
	var out []byte
	for _, kv := range keys {
		if len(kv) > 255 {
			continue
		}
		out = append(out, byte(len(kv)))
		out = append(out, kv...)
	}
	return out
}

// Stop unregisters the services.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.raop != nil {
		a.raop.Shutdown()
		a.raop = nil
	}
	if a.server != nil {
		a.server.Shutdown()
		a.server = nil
	}
}

// String reports the current advertised state for the logs.
func (a *Advertiser) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server == nil {
		return "not advertising"
	}
	return net.JoinHostPort("", strconv.Itoa(a.port))
}
