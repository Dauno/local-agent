//go:build !unix

package recoverableresult

import (
	"errors"
	"os"
	"path/filepath"
)

func openRegularNoFollow(baseDir, name string) (*os.File, error) {
	if !validOpaqueID(name) {
		return nil, errors.New("storage locator is invalid")
	}
	path := filepath.Join(baseDir, name)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("storage file is not regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, errors.New("storage file changed during open")
	}
	return file, nil
}

func removeFileNoFollow(baseDir, name string) error {
	if !validOpaqueID(name) {
		return errors.New("storage locator is invalid")
	}
	path := filepath.Join(baseDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("storage file is not regular")
	}
	return os.Remove(path)
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
