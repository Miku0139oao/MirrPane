package protocol

import (
	"net"
	"testing"
)

func TestUsableIPv4(t *testing.T) {
	for _, tc := range []struct {
		ip     string
		usable bool
	}{
		{"192.168.1.170", true}, {"10.1.2.3", true}, {"172.17.48.1", true},
		{"8.8.8.8", true}, {"198.17.255.254", true}, {"198.20.0.1", true},
		{"100.63.255.254", true}, {"100.128.0.1", true},
		{"198.18.0.1", false}, {"198.19.255.254", false},
		{"100.64.0.1", false}, {"100.127.255.254", false},
		{"127.0.0.1", false}, {"169.254.10.1", false}, {"0.0.0.0", false},
		{"224.0.0.251", false}, {"255.255.255.255", false}, {"::1", false}, {"fe80::1", false},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isUsableIPv4(net.ParseIP(tc.ip)); got != tc.usable {
				t.Fatalf("usable=%v, want %v", got, tc.usable)
			}
		})
	}
}

func TestInterfaceScoreWithTUNAndExternalBridge(t *testing.T) {
	lanFlags := net.FlagUp | net.FlagBroadcast | net.FlagMulticast
	for _, tc := range []struct {
		name, ip string
		flags    net.Flags
		usable   bool
	}{
		{"vEthernet (phy)", "192.168.1.170", lanFlags, true},
		{"乙太網路", "192.168.1.170", lanFlags, true},
		{"Meta", "198.18.0.1", lanFlags, false},
		{"renamed VPN", "10.0.0.2", net.FlagUp | net.FlagMulticast | net.FlagPointToPoint, false},
		{"no multicast", "192.168.1.170", net.FlagUp | net.FlagBroadcast, false},
		{"Tailscale", "100.76.188.9", lanFlags, false},
		{"CloudflareWARP", "10.0.0.2", lanFlags, false},
		{"offline Ethernet", "192.168.1.170", net.FlagBroadcast | net.FlagMulticast, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iface := net.Interface{Name: tc.name, Flags: tc.flags, HardwareAddr: net.HardwareAddr{0, 1, 2, 3, 4, 5}}
			if got := scoreInterface(iface, []net.IP{net.ParseIP(tc.ip)}); (got > 0) != tc.usable {
				t.Fatalf("score=%d, usable=%v", got, tc.usable)
			}
		})
	}
}
