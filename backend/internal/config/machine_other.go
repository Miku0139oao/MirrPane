//go:build !windows

package config

// machineSecret is unavailable, so the seed falls back to random.
func machineSecret() string { return "" }
