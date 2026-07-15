//go:build unix && !linux

package codexshim

import (
	"errors"
	"os"
	"os/exec"
)

// Inherit the shim's outer process group so provider-level cancellation still
// reaches the complete native subprocess tree.
func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
