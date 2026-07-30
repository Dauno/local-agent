//go:build !linux

package rangedreader

import (
	"errors"
	"os"
	"path/filepath"
)

func openSecure(root, relative string) (*os.File, error) {
	path := filepath.Join(root, relative)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("unsupported path")
	}
	return os.Open(path)
}
