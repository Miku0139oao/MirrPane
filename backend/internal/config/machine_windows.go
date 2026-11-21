//go:build windows

package config

import "golang.org/x/sys/windows/registry"

// machineSecret returns a value that is stable for this Windows installation and
// differs between machines.
//
// It is not a secret in the cryptographic sense — a local process can read it —
// but it is not broadcast either, which is what matters here: the pairing seed
// must be unpredictable to a peer on the network, and this is not on the wire.
func machineSecret() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()

	guid, _, err := key.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return guid
}
