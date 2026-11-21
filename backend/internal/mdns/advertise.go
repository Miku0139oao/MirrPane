// Package mdns advertises this receiver over DNS-SD so that iOS and macOS
// clients can find it in the Screen Mirroring menu.
package mdns

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

type serviceServer interface {
	Shutdown()
}

// lanInterface includes the addresses because an adapter can retain its index
// across a DHCP renewal or Wi-Fi reconnect. ipv4 is the sorted IPv4 address set.
type lanInterface struct {
	iface net.Interface
	ipv4  string
}

// Advertiser publishes the _airplay._tcp and _raop._tcp records.
type Advertiser struct {
	server serviceServer
	raop   serviceServer
	mu     sync.Mutex
	lan    lanInterface
	stop   chan struct{}
	done   chan struct{}

	// Dependencies are set before Start; tests need neither real adapters nor
	// multicast sockets, and can shorten the polling interval.
	resolveInterface func() (lanInterface, error)
	register         func(string, string, string, int, []string, []net.Interface) (serviceServer, error)
	watchInterval    time.Duration

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
	return &Advertiser{
		name: name, port: port, device: device,
		resolveInterface: resolveLANInterface,
		register:         registerService,
		watchInterval:    5 * time.Second,
	}
}

func registerService(instance, service, domain string, port int, txt []string, ifaces []net.Interface) (serviceServer, error) {
	return zeroconf.Register(instance, service, domain, port, txt, ifaces)
}

func resolveLANInterface() (lanInterface, error) {
	lan := protocol.LANInterface()
	if lan == nil {
		return lanInterface{}, fmt.Errorf("mdns: no multicast-capable LAN interface with a usable IPv4 address")
	}
	addrs, err := lan.Addrs()
	if err != nil {
		return lanInterface{}, fmt.Errorf("mdns: addresses for %q: %w", lan.Name, err)
	}
	var ips []string
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ip := ipnet.IP.To4(); ip != nil {
				ips = append(ips, ip.String())
			}
		}
	}
	if len(ips) == 0 {
		return lanInterface{}, fmt.Errorf("mdns: LAN interface %q lost its IPv4 address", lan.Name)
	}
	sort.Strings(ips)
	return lanInterface{iface: *lan, ipv4: strings.Join(ips, ",")}, nil
}

// SetPinEnabled toggles whether the TXT records claim a password is required.
func (a *Advertiser) SetPinEnabled(enabled bool) {
	a.mu.Lock()
	a.pinPW = enabled
	a.mu.Unlock()
}

// Start registers the DNS-SD services and watches for LAN changes until Stop.
// Repeated calls while the watchdog is running are harmless, even if a failed
// re-registration is waiting for the next retry.
func (a *Advertiser) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.stop != nil {
		return nil
	}
	if err := a.refreshLocked(); err != nil {
		return err
	}
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	go a.watch(a.stop, a.done, a.watchInterval)
	return nil
}

func (a *Advertiser) watch(stop <-chan struct{}, done chan<- struct{}, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		// Stop may have won the mutex after this tick. An old watchdog must
		// never restart services, including after a subsequent Start.
		select {
		case <-stop:
			a.mu.Unlock()
			return
		default:
		}
		if err := a.refreshLocked(); err != nil {
			log.Printf("mdns: refresh failed (will retry): %v", err)
		}
		a.mu.Unlock()
	}
}

// refreshLocked reconciles the two services against one LAN snapshot. A failed
// lookup or registration leaves them stopped so the next tick can retry.
func (a *Advertiser) refreshLocked() error {
	lan, err := a.resolveInterface()
	if err != nil {
		a.stopServicesLocked()
		return err
	}
	if a.server != nil && a.lan.iface.Index == lan.iface.Index && a.lan.ipv4 == lan.ipv4 {
		a.startRAOPLocked(lan.iface)
		return nil
	}
	a.stopServicesLocked()
	return a.startServicesLocked(lan)
}

func (a *Advertiser) startServicesLocked(lan lanInterface) error {

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
	ifaces := []net.Interface{lan.iface}

	srv, err := a.register(
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
	a.lan = lan
	a.startRAOPLocked(lan.iface)

	log.Printf("mdns: advertising %q on %q (index %d, IPv4 %s), port %d (features %s)", a.name, lan.iface.Name, lan.iface.Index, lan.ipv4, a.port, features)
	return nil
}

func (a *Advertiser) startRAOPLocked(iface net.Interface) {
	if a.raop != nil {
		return
	}
	// Best effort: _raop._tcp is optional for screen mirroring.
	if raopSrv, err := a.register(
		raopServiceName(a.device.MAC)+"@"+a.name,
		"_raop._tcp",
		"local.",
		a.port,
		a.raopTXT,
		[]net.Interface{iface},
	); err == nil {
		a.raop = raopSrv
	} else {
		log.Printf("mdns: _raop._tcp registration skipped: %v", err)
	}
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

// Stop unregisters both services and waits for the watchdog to exit. Waiting
// outside mu lets an already queued tick observe cancellation without deadlock.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	if a.stop != nil {
		close(a.stop)
		a.stop = nil
	}
	// Retain done so concurrent Stop calls also wait for this watchdog.
	done := a.done
	a.stopServicesLocked()
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *Advertiser) stopServicesLocked() {
	if a.raop != nil {
		a.raop.Shutdown()
		a.raop = nil
	}
	if a.server != nil {
		a.server.Shutdown()
		a.server = nil
	}
	a.lan = lanInterface{}
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
