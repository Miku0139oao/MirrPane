//go:build !windows

package main

import "os/exec"

func configureSidecarProcess(_ *exec.Cmd) {}
