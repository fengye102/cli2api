//go:build !windows

package zcode

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// reapLeftovers kills any process still carrying marker in its command line.
// A headless Chromium re-parents part of its process tree to init and survives
// the solver's own clean-up, so the browser profile directory doubles as a
// marker the gateway can sweep by.
func reapLeftovers(marker string) {
	if strings.TrimSpace(marker) == "" {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !strings.Contains(string(raw), marker) {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}
}
