//go:build !linux && !darwin

package bridge

import (
	"fmt"
	"os"
	"path/filepath"
)

func allocShm(nbytes int) (name string, data []byte, f *os.File, err error) {
	name = shmName()
	path := filepath.Join(os.TempDir(), name)

	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create shm file: %w", err)
	}
	if err = f.Truncate(int64(nbytes)); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, nil, fmt.Errorf("truncate shm: %w", err)
	}
	data = make([]byte, nbytes)
	return name, data, f, nil
}

func freeShm(blk *ShmBlock) {
	if blk.File != nil {
		path := filepath.Join(os.TempDir(), blk.Name)
		blk.File.Close()
		os.Remove(path)
	}
}
