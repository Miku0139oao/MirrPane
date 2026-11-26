package protocol

import (
	"net"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDefaultRouteIndexUsesNetworkOrder(t *testing.T) {
	index, ok := defaultRouteIndex(net.IPv4(127, 0, 0, 1))
	if !ok {
		t.Skip("IPv4 routing unavailable")
	}
	iface, err := net.InterfaceByIndex(index)
	if err != nil || iface.Flags&net.FlagLoopback == 0 {
		t.Fatalf("127.0.0.1 routed to non-loopback interface: index=%d interface=%v err=%v", index, iface, err)
	}
}

func TestPhysicalGatewayAdapter(t *testing.T) {
	var storage syscall.RawSockaddrAny
	address := (*windows.RawSockaddrInet4)(unsafe.Pointer(&storage))
	*address = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: [4]byte{192, 168, 1, 1}}
	gateway := windows.IpAdapterGatewayAddress{Address: windows.SocketAddress{
		Sockaddr:       &storage,
		SockaddrLength: int32(unsafe.Sizeof(*address)),
	}}
	base := windows.IpAdapterAddresses{OperStatus: windows.IfOperStatusUp, IfType: windows.IF_TYPE_ETHERNET_CSMACD, PhysicalAddressLength: 6, FirstGatewayAddress: &gateway}
	for _, tc := range []struct {
		name   string
		change func(*windows.IpAdapterAddresses)
		want   bool
	}{
		{"external Hyper-V bridge", func(a *windows.IpAdapterAddresses) {}, true},
		{"Wi-Fi", func(a *windows.IpAdapterAddresses) { a.IfType = windows.IF_TYPE_IEEE80211 }, true},
		{"Meta virtual TUN", func(a *windows.IpAdapterAddresses) { a.IfType = 53 }, false},
		{"tunnel", func(a *windows.IpAdapterAddresses) { a.IfType = windows.IF_TYPE_TUNNEL }, false},
		{"PPP", func(a *windows.IpAdapterAddresses) { a.IfType = windows.IF_TYPE_PPP }, false},
		{"host-only switch", func(a *windows.IpAdapterAddresses) { a.FirstGatewayAddress = nil }, false},
		{"disconnected", func(a *windows.IpAdapterAddresses) { a.OperStatus = windows.IfOperStatusDown }, false},
		{"no MAC", func(a *windows.IpAdapterAddresses) { a.PhysicalAddressLength = 0 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.change(&a)
			if got := physicalGatewayAdapter(&a); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectedLANInterface(t *testing.T) {
	iface := LANInterface()
	if iface == nil {
		t.Skip("no active IPv4 LAN interface")
	}
	t.Logf("LAN interface: %s (index %d), address %s", iface.Name, iface.Index, LocalIPv4())
	for _, ip := range addresses(iface) {
		if isUsableIPv4(ip) {
			return
		}
	}
	t.Fatal("selected an interface without a usable LAN IPv4 address")
}
