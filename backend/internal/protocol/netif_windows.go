//go:build windows

package protocol

import (
	"encoding/binary"
	"net"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetBestInterface = iphlpapi.NewProc("GetBestInterface")
)

// defaultRouteIndex asks the Windows routing table which adapter would carry
// traffic to dst. No packet is sent and no interface name is guessed, so bridged
// Hyper-V switches, renamed adapters and localised Windows all work.
//
// DWORD GetBestInterface(IPAddr dwDestAddr, PDWORD pdwBestIfIndex) — IPAddr is a
// 32-bit address in network byte order.
func defaultRouteIndex(dst net.IP) (int, bool) {
	v4 := dst.To4()
	if v4 == nil {
		return 0, false
	}
	// IPAddr stores the network-order bytes in a native Windows ULONG.
	// Using BigEndian.Uint32 reverses non-palindromic addresses on Windows.
	dest := binary.NativeEndian.Uint32(v4)
	var idx uint32
	ret, _, _ := procGetBestInterface.Call(
		uintptr(dest),
		uintptr(unsafe.Pointer(&idx)),
	)
	if ret != 0 || idx == 0 {
		return 0, false
	}
	return int(idx), true
}

// preferredLANIndex chooses a connected Ethernet/Wi-Fi adapter with an IPv4
// gateway, independently of VPN-owned default routes. Hyper-V external switches
// present as Ethernet and are deliberately supported. No PowerShell or packets
// are needed; IP Helper supplies the adapter metadata and interface metrics.
func preferredLANIndex() (int, bool) {
	size := uint32(15 << 10)
	for range 4 {
		if size == 0 || size > 1<<20 {
			return 0, false
		}
		buffer := make([]byte, size)
		head := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0, head, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return 0, false
		}
		index, metric := 0, ^uint32(0)
		for adapter := head; adapter != nil; adapter = adapter.Next {
			if !physicalGatewayAdapter(adapter) {
				continue
			}
			iface, err := net.InterfaceByIndex(int(adapter.IfIndex))
			if err != nil || interfaceScore(*iface) == 0 {
				continue
			}
			if index == 0 || adapter.Ipv4Metric < metric {
				index, metric = iface.Index, adapter.Ipv4Metric
			}
		}
		runtime.KeepAlive(buffer)
		return index, index != 0
	}
	return 0, false
}

func physicalGatewayAdapter(adapter *windows.IpAdapterAddresses) bool {
	if adapter.OperStatus != windows.IfOperStatusUp || adapter.PhysicalAddressLength < 6 {
		return false
	}
	if adapter.IfType != windows.IF_TYPE_ETHERNET_CSMACD && adapter.IfType != windows.IF_TYPE_IEEE80211 {
		return false
	}
	if virtualInterfaceName(windows.UTF16PtrToString(adapter.Description)) {
		return false
	}
	for gateway := adapter.FirstGatewayAddress; gateway != nil; gateway = gateway.Next {
		if isUsableIPv4(gateway.Address.IP()) {
			return true
		}
	}
	return false
}
