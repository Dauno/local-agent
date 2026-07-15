//go:build unix && !linux

package agentcli

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup places the child in its own process group so the entire
// subprocess tree (including an opencode grandchild) can be terminated together.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup terminates the whole process group led by the child.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Negative pid targets the process group.
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
