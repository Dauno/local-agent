//go:build unix

package recoverableresult

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openRegularNoFollow(baseDir, name string) (*os.File, error) {
	if !validOpaqueID(name) {
		return nil, errors.New("storage locator is invalid")
	}
	dirFD, err := unix.Open(baseDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)

	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open storage file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("storage file is not regular")
	}
	return file, nil
}

func removeFileNoFollow(baseDir, name string) error {
	if !validOpaqueID(name) {
		return errors.New("storage locator is invalid")
	}
	dirFD, err := unix.Open(baseDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	return unix.Unlinkat(dirFD, name, 0)
}

func validOpaqueID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
