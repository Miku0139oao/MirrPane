//go:build !windows

package main

// newPlatformFrameSource falls back to the synthetic pattern off Windows: the
// shared-memory receiver only exists for the Windows build.
func newPlatformFrameSource(string) FrameSource {
	return NewSyntheticFrameSource(960, 540)
}
