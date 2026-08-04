//go:build unix

package app

import "syscall"

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil || err == syscall.EPERM {
		return true
	}
	return false
}
