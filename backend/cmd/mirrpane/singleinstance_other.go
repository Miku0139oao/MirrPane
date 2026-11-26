//go:build !windows

package main

// acquireSingleInstance is a no-op outside Windows: the receiver only ships
// for Windows, and platforms such as CI's build host need not serialize runs.
func acquireSingleInstance() (func(), bool) { return func() {}, true }
