//go:build !windows

package zcode

import (
	"os/exec"
	"syscall"
)

// isolateProcess puts the captcha solver in its own process group so that a
// timeout tears down the whole Chromium tree, not just the Node parent.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
