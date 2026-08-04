//go:build !unix

package app

func processExists(pid int) bool {
	return false
}
