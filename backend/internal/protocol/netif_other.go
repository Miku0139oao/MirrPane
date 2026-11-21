//go:build !windows

package protocol

import "net"

func preferredLANIndex() (int, bool) { return 0, false }

// defaultRouteIndex reports the adapter holding the default route. On non-Windows
// platforms it is answered by the UDP probe in LANInterface instead.
func defaultRouteIndex(dst net.IP) (int, bool) { return 0, false }
