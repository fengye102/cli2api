//go:build windows

package zcode

import "os/exec"

func isolateProcess(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func reapLeftovers(string) {}
