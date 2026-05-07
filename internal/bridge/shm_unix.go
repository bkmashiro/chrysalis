//go:build linux || darwin

package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// shmDir returns the directory used for shared-memory files.
func shmDir() string {
	// On Linux /dev/shm is a tmpfs; on macOS we use /tmp.
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		return "/dev/shm"
	}
	return os.TempDir()
}

func allocShm(nbytes int) (name string, data []byte, f *os.File, err error) {
	name = shmName()
	path := filepath.Join(shmDir(), name)

	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create shm file %s: %w", path, err)
	}

	if err = f.Truncate(int64(nbytes)); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, nil, fmt.Errorf("truncate shm: %w", err)
	}

	data, err = syscall.Mmap(int(f.Fd()), 0, nbytes,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, nil, fmt.Errorf("mmap shm: %w", err)
	}

	return name, data, f, nil
}

func freeShm(blk *ShmBlock) {
	if blk.Data != nil {
		_ = syscall.Munmap(blk.Data)
	}
	if blk.File != nil {
		path := filepath.Join(shmDir(), blk.Name)
		blk.File.Close()
		os.Remove(path)
	}
}
