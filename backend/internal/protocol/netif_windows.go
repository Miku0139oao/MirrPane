//go:build windows

package protocol

import (
	"encoding/binary"
	"net"
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
	dest := binary.BigEndian.Uint32(v4)
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
