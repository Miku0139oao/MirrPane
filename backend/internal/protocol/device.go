// Package protocol implements the AirPlay receiver side of the protocol.
//
// Portions of this file are derived from UxPlay (https://github.com/FDH2/UxPlay),
// Copyright (C) 2012 Juho Vähä-Herttua and contributors, licensed under the
// GNU General Public License v3. This project is likewise GPLv3.
package protocol

import (
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"
)

// Model string reported to clients. AppleTV3,2 is the model UxPlay reports and
// the one iOS is known to accept for mirroring.
const Model = "AppleTV3,2"

// SourceVersion matches the AirPlay protocol version UxPlay advertises.
const SourceVersion = "220.68"

// ProtocolVersion for /info.
const ProtocolVersion = "1.1"

// Feature bits: low 32 bits, then high 32 bits. Bit 27 is "supports legacy
// pairing" (the FairPlay fairplay handshake used by screen mirroring).
const (
	FeaturesLow  uint32 = 0x5A7FFEE6
	FeaturesHigh uint32 = 0x0
)

// Features returns the two feature words advertised over mDNS and /info.
func Features() (uint32, uint32) { return FeaturesLow, FeaturesHigh }

// FeaturesString renders the two words the way the mDNS TXT record wants them.
func FeaturesString() string {
	return fmt.Sprintf("0x%X,0x%X", FeaturesLow, FeaturesHigh)
}

// DeviceInfo is everything a client learns about this receiver.
type DeviceInfo struct {
	Name       string // human readable name shown on the iPhone
	MAC        string // deviceid, AA:BB:CC:DD:EE:FF form
	PublicKey  string // 64 hex characters, curve25519 public key (AirPlay 2)
	PairingID  string // pi, a UUID
	Model      string
	SrcVersion string
	// PwRequired tells clients whether a PIN/password is needed.
	PwRequired bool
}

// NewDeviceInfo builds device info with a stable pairing ID derived from the MAC
// so it does not change between launches.
func NewDeviceInfo(name, mac string) DeviceInfo {
	return DeviceInfo{
		Name:       name,
		MAC:        strings.ToUpper(mac),
		PublicKey:  "", // filled by the pairing module once keys exist
		PairingID:  uuid.NewSHA1(uuid.NameSpaceOID, []byte("airplay-go/"+strings.ToUpper(mac))).String(),
		Model:      Model,
		SrcVersion: SourceVersion,
	}
}

// HardwareAddress returns the MAC address of the LAN interface, formatted as
// AA:BB:CC:DD:EE:FF. Falls back to a deterministic synthetic address so that
// discovery still works on machines with no real adapter.
func HardwareAddress() string {
	if iface := LANInterface(); iface != nil && len(iface.HardwareAddr) >= 6 {
		hw := iface.HardwareAddr
		return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
			hw[0], hw[1], hw[2], hw[3], hw[4], hw[5])
	}
	return "02:00:00:00:00:01"
}

// virtualInterfacePrefixes are adapters that exist on the machine but that no
// iPhone on the home network can reach. Advertising their address makes the
// receiver invisible, so they are skipped when picking the LAN interface.
//
// "vethernet" is deliberately absent: a Hyper-V external switch carries the real
// LAN on many machines, so its name tells us nothing.
var virtualInterfacePrefixes = []string{
	"tailscale", "zerotier", "wireguard", "openvpn",
	"virtualbox", "vmware", "loopback", "bluetooth",
	"npcap", "wintun", "docker", "cloudflarewarp", "clash", "mihomo", "sing-box",
}

func virtualInterfaceName(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range virtualInterfacePrefixes {
		if strings.Contains(name, prefix) {
			return true
		}
	}
	return false
}

// interfaceScore ranks a candidate LAN adapter. Higher is better; a score of
// zero or below means "not a candidate".
func interfaceScore(iface net.Interface) int {
	return scoreInterface(iface, addresses(&iface))
}

func scoreInterface(iface net.Interface, ips []net.IP) int {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 ||
		iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
		return 0
	}
	if virtualInterfaceName(iface.Name) {
		return 0
	}
	usable := 0
	for _, a := range ips {
		if isUsableIPv4(a) {
			usable++
		}
	}
	if usable == 0 {
		return 0
	}
	score := 4 * usable
	if len(iface.HardwareAddr) >= 6 {
		score += 3 // real adapters have a MAC
	}
	if iface.Flags&net.FlagBroadcast != 0 {
		score += 2 // Ethernet/Wi-Fi; cellular and tunnels do not broadcast
	}
	return score
}

// LANInterface returns the interface an iPhone on the same network should use
// to reach this machine, or nil when there is none.
//
// A VPN/TUN can own the internet default route without reaching local multicast.
// Prefer an Ethernet/Wi-Fi gateway adapter (including external Hyper-V bridges)
// before consulting that route. Adapter-name heuristics are the final fallback.
func LANInterface() *net.Interface {
	if idx, ok := preferredLANIndex(); ok {
		if iface, err := net.InterfaceByIndex(idx); err == nil && interfaceScore(*iface) > 0 {
			return iface
		}
	}
	// 1.1.1.1 is used only as a route lookup target; nothing is sent to it.
	if idx, ok := defaultRouteIndex(net.IPv4(1, 1, 1, 1)); ok {
		if iface, err := net.InterfaceByIndex(idx); err == nil && interfaceScore(*iface) > 0 {
			return iface
		}
	}

	// Fallback for platforms without GetBestInterface: a UDP "connect" resolves
	// the route without sending a packet.
	if iface := udpRouteInterface(); iface != nil && interfaceScore(*iface) > 0 {
		return iface
	}

	// Last resort: rank every adapter.
	best := 0
	var bestIface *net.Interface
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range all {
		score := interfaceScore(all[i])
		if score > best {
			best = score
			bestIface = &all[i]
		}
	}
	return bestIface
}

// udpRouteInterface finds the adapter the kernel would use to reach the internet.
func udpRouteInterface() *net.Interface {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return nil
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil {
		return nil
	}
	return interfaceHolding(local.IP)
}

// interfaceHolding returns the interface that owns ip.
func interfaceHolding(ip net.IP) *net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range all {
		for _, a := range addresses(&all[i]) {
			if a.Equal(ip) {
				return &all[i]
			}
		}
	}
	return nil
}

func addresses(iface *net.Interface) []net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			out = append(out, ipnet.IP)
		}
	}
	return out
}

// isUsableIPv4 rejects loopback, link-local, CGNAT and benchmarking addresses
// commonly assigned to TUN adapters (including Mihomo's 198.18.0.1).
func isUsableIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil || !v4.IsGlobalUnicast() || v4.IsLinkLocalUnicast() {
		return false
	}
	// 100.64.0.0/10 is carrier-grade NAT; Tailscale and some VPNs live here.
	if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	// RFC 2544 benchmarking range, also used as a TUN/fake-IP network.
	if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
		return false
	}
	return true
}

// AirplayDeviceID renders the deviceid the way the _airplay._tcp TXT record
// wants it: lowercase hex with colon separators. UxPlay's utils_hwaddr_airplay
// (lib/utils.c) is the reference. The _raop._tcp record uses the opposite form;
// see RAOPDeviceID.
func AirplayDeviceID(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, "-", ":"))
}

// RAOPDeviceID renders the deviceid the _raop._tcp service instance and TXT
// record want: uppercase hex with no separators (utils_hwaddr_raop).
func RAOPDeviceID(mac string) string {
	return strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
}

// LocalIPv4 returns the machine's primary LAN IPv4 address, or "" when offline.
func LocalIPv4() string {
	iface := LANInterface()
	if iface == nil {
		return ""
	}
	for _, ip := range addresses(iface) {
		if isUsableIPv4(ip) {
			return ip.To4().String()
		}
	}
	return ""
}
