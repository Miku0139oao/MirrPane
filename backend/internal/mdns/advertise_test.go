package mdns

import (
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

type fakeService struct {
	instance string
	service  string
	iface    net.Interface
	txt      []string
	stops    int
	onStop   func()
}

func (s *fakeService) Shutdown() {
	s.onStop()
}

type fakeLAN struct {
	mu           sync.Mutex
	current      lanInterface
	err          error
	resolveCalls int
	failService  string
	services     []*fakeService
	events       []string
}

type fakeLANState struct {
	resolveCalls int
	services     []fakeService
	events       []string
}

// These helpers release the fake's lock before the test calls the advertiser:
// advertiser callbacks take a.mu before f.mu, never the other way around.
func (f *fakeLAN) setInterface(index int, ipv4 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current.iface.Index = index
	f.current.ipv4 = ipv4
}

func (f *fakeLAN) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeLAN) setRegistrationFailure(service string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failService = service
}

func (f *fakeLAN) snapshot() fakeLANState {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := fakeLANState{
		resolveCalls: f.resolveCalls,
		services:     make([]fakeService, len(f.services)),
		events:       slices.Clone(f.events),
	}
	for i, s := range f.services {
		state.services[i] = *s
		state.services[i].txt = slices.Clone(s.txt)
	}
	return state
}

func newWatchdogTestAdvertiser() (*Advertiser, *fakeLAN) {
	f := &fakeLAN{current: lanInterface{
		iface: net.Interface{Index: 7, Name: "Wi-Fi"},
		ipv4:  "192.168.1.20",
	}}
	a := testAdvertiser(false)
	a.watchInterval = 10 * time.Millisecond
	a.resolveInterface = func() (lanInterface, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.resolveCalls++
		return f.current, f.err
	}
	a.register = func(instance, service, domain string, port int, txt []string, ifaces []net.Interface) (serviceServer, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if service == f.failService {
			return nil, errors.New("multicast registration unavailable")
		}
		if len(ifaces) != 1 {
			return nil, errors.New("expected exactly one LAN interface")
		}
		s := &fakeService{
			instance: instance,
			service:  service,
			iface:    ifaces[0],
			txt:      slices.Clone(txt),
		}
		s.onStop = func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			s.stops++
			f.events = append(f.events, "stop "+service)
		}
		f.services = append(f.services, s)
		f.events = append(f.events, "start "+service)
		return s, nil
	}
	return a, f
}

func watchdogTick(a *Advertiser) {
	synctest.Wait()
	time.Sleep(a.watchInterval)
	synctest.Wait()
}

func TestWatchdogSameInterfaceDoesNotRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, f := newWatchdogTestAdvertiser()
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		defer a.Stop()
		done := a.done
		if err := a.Start(); err != nil || a.done != done {
			t.Fatalf("repeated Start replaced the watchdog: %v", err)
		}
		for range 3 {
			watchdogTick(a)
		}
		state := f.snapshot()
		if state.resolveCalls != 4 || len(state.services) != 2 {
			t.Fatalf("unchanged LAN: %d lookups, %d services", state.resolveCalls, len(state.services))
		}
		for _, s := range state.services {
			if s.stops != 0 {
				t.Fatalf("unchanged LAN stopped %s", s.service)
			}
		}
		a.Stop()
		a.Stop()
		select {
		case <-done:
		default:
			t.Fatal("Stop returned before the watchdog exited")
		}
		watchdogTick(a)
		state = f.snapshot()
		if state.resolveCalls != 4 {
			t.Fatal("watchdog kept resolving after Stop")
		}
		for _, s := range state.services {
			if s.stops != 1 {
				t.Fatalf("repeated Stop shut down %s %d times", s.service, s.stops)
			}
		}
	})
}

func TestWatchdogChangedInterfaceRestartsBothServices(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index int
		ipv4  string
	}{
		{"index", 8, "192.168.1.20"},
		{"IPv4", 7, "192.168.1.21"},
		{"index and IPv4", 8, "192.168.2.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, f := newWatchdogTestAdvertiser()
				if err := a.Start(); err != nil {
					t.Fatal(err)
				}
				defer a.Stop()
				synctest.Wait()
				f.setInterface(tc.index, tc.ipv4)
				watchdogTick(a)
				state := f.snapshot()
				if len(state.services) != 4 {
					t.Fatalf("got %d service registrations, want 4", len(state.services))
				}
				wantEvents := []string{
					"start _airplay._tcp", "start _raop._tcp",
					"stop _raop._tcp", "stop _airplay._tcp",
					"start _airplay._tcp", "start _raop._tcp",
				}
				if !slices.Equal(state.events, wantEvents) {
					t.Fatalf("restart order = %v, want %v", state.events, wantEvents)
				}
				for i, old := range state.services[:2] {
					next := state.services[i+2]
					if old.stops != 1 || next.stops != 0 || next.iface.Index != tc.index {
						t.Fatalf("%s: old stops %d, new stops %d, interface %d", old.service, old.stops, next.stops, next.iface.Index)
					}
					if old.instance != next.instance || !slices.Equal(old.txt, next.txt) {
						t.Fatalf("restart changed %s identity or TXT records", old.service)
					}
				}
				watchdogTick(a)
				if len(f.snapshot().services) != 4 {
					t.Fatal("stable replacement LAN restarted again")
				}
			})
		})
	}
}

func TestWatchdogRecoversAfterLANLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, f := newWatchdogTestAdvertiser()
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		defer a.Stop()
		synctest.Wait()
		f.setError(errors.New("LAN disconnected"))
		watchdogTick(a)
		if a.String() != "not advertising" {
			t.Fatal("still advertising after losing the LAN")
		}
		for _, s := range f.snapshot().services {
			if s.stops != 1 {
				t.Fatalf("LAN loss left %s running", s.service)
			}
		}
		f.setError(nil)
		watchdogTick(a)
		if len(f.snapshot().services) != 4 || a.String() == "not advertising" {
			t.Fatal("advertising did not recover when the same LAN returned")
		}
	})
}

func TestWatchdogRetriesRegistration(t *testing.T) {
	for _, service := range []string{"_airplay._tcp", "_raop._tcp"} {
		t.Run(service, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, f := newWatchdogTestAdvertiser()
				if err := a.Start(); err != nil {
					t.Fatal(err)
				}
				defer a.Stop()
				done := a.done
				synctest.Wait()
				f.setInterface(7, "192.168.1.21")
				f.setRegistrationFailure(service)
				watchdogTick(a)
				wantServices := 2
				if service == "_raop._tcp" {
					wantServices++ // AirPlay remains available while RAOP retries.
				}
				state := f.snapshot()
				if len(state.services) != wantServices {
					t.Fatalf("registration failure: %d services, want %d", len(state.services), wantServices)
				}
				if err := a.Start(); err != nil || a.done != done {
					t.Fatalf("Start replaced a retrying watchdog: %v", err)
				}
				f.setRegistrationFailure("")
				watchdogTick(a)
				state = f.snapshot()
				if len(state.services) != 4 {
					t.Fatalf("retry created %d services, want 4", len(state.services))
				}
				for _, s := range state.services[2:] {
					if s.stops != 0 {
						t.Fatalf("retry stopped the new %s", s.service)
					}
				}
			})
		})
	}
}

func TestStopDuringWatchdogRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, f := newWatchdogTestAdvertiser()
		entered, release := make(chan struct{}), make(chan struct{})
		register := a.register
		a.register = func(instance, service, domain string, port int, txt []string, ifaces []net.Interface) (serviceServer, error) {
			if ifaces[0].Index == 8 && service == "_airplay._tcp" {
				close(entered)
				<-release
			}
			return register(instance, service, domain, port, txt, ifaces)
		}
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		defer a.Stop()
		done := a.done
		synctest.Wait()
		f.setInterface(8, "192.168.1.20")
		time.Sleep(a.watchInterval)
		<-entered
		stopping, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			close(stopping)
			a.Stop()
			close(stopped)
		}()
		<-stopping
		close(release)
		<-stopped
		select {
		case <-done:
		default:
			t.Fatal("Stop returned with a live watchdog")
		}
		state := f.snapshot()
		if len(state.services) != 4 {
			t.Fatalf("expected an in-flight restart, got %d registrations", len(state.services))
		}
		for _, s := range state.services {
			if s.stops != 1 {
				t.Fatalf("Stop left %s with %d shutdowns", s.service, s.stops)
			}
		}
		calls := state.resolveCalls
		watchdogTick(a)
		if f.snapshot().resolveCalls != calls || a.String() != "not advertising" {
			t.Fatal("watchdog restarted after Stop returned")
		}
	})
}

func TestConcurrentStartStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, f := newWatchdogTestAdvertiser()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if err := a.Start(); err != nil {
					t.Errorf("Start: %v", err)
				}
				a.Stop()
			})
		}
		wg.Wait()
		a.Stop()
		for _, s := range f.snapshot().services {
			if s.stops != 1 {
				t.Fatalf("concurrent Stop shut down %s %d times", s.service, s.stops)
			}
		}
		// A stopped advertiser can be started again with a fresh watchdog.
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		defer a.Stop()
		watchdogTick(a)
		state := f.snapshot()
		for _, s := range state.services[len(state.services)-2:] {
			if s.stops != 0 {
				t.Fatalf("an old watchdog stopped the new %s", s.service)
			}
		}
	})
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
