package agentcli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	rootPID := cmd.Process.Pid
	descendants := linuxDescendants(rootPID)

	// A nested shim may place its native CLI in a separate process group to
	// handle parser failures. Stop descendants first, then kill every discovered
	// group so cancellation still covers the full subprocess tree.
	groups := make(map[int]struct{})
	for _, pid := range descendants {
		_ = syscall.Kill(pid, syscall.SIGSTOP)
		if group, err := syscall.Getpgid(pid); err == nil && group > 0 && group != rootPID {
			groups[group] = struct{}{}
		}
	}
	for group := range groups {
		_ = syscall.Kill(-group, syscall.SIGKILL)
	}
	for _, pid := range descendants {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	err := syscall.Kill(-rootPID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func linuxDescendants(rootPID int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "PPid:") {
				continue
			}
			parent, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			if err == nil {
				children[parent] = append(children[parent], pid)
			}
			break
		}
	}

	var result []int
	queue := append([]int(nil), children[rootPID]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		result = append(result, pid)
		queue = append(queue, children[pid]...)
	}
	return result
}
