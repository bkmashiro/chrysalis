// Package bridge implements shared-memory allocation and the wazero host function dispatcher.
package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// ShmBlock describes an allocated shared-memory segment.
type ShmBlock struct {
	Name  string // e.g. "/chr-a1b2c3" (used as filename under /dev/shm on Linux)
	File  *os.File
	Data  []byte // mmap'd slice (or file content read into memory for cross-platform)
	Size  int
}

// ShmManager allocates and tracks shared memory blocks.
type ShmManager struct {
	mu     sync.Mutex
	blocks map[uint32]*ShmBlock
	nextH  uint32
}

// NewShmManager creates a new ShmManager.
func NewShmManager() *ShmManager {
	return &ShmManager{blocks: make(map[uint32]*ShmBlock)}
}

// Alloc allocates nbytes of shared memory and returns a handle + the writable slice.
func (m *ShmManager) Alloc(nbytes int) (uint32, []byte, error) {
	name, data, f, err := allocShm(nbytes)
	if err != nil {
		return 0, nil, err
	}
	blk := &ShmBlock{Name: name, File: f, Data: data, Size: nbytes}
	m.mu.Lock()
	h := m.nextH
	m.nextH++
	m.blocks[h] = blk
	m.mu.Unlock()
	return h, data, nil
}

// Get returns the ShmBlock for handle h.
func (m *ShmManager) Get(h uint32) (*ShmBlock, bool) {
	m.mu.Lock()
	blk, ok := m.blocks[h]
	m.mu.Unlock()
	return blk, ok
}

// Free releases the shared memory block for handle h.
func (m *ShmManager) Free(h uint32) {
	m.mu.Lock()
	blk, ok := m.blocks[h]
	if ok {
		delete(m.blocks, h)
	}
	m.mu.Unlock()
	if ok {
		freeShm(blk)
	}
}

// FreeAll releases every block tracked by this manager.
func (m *ShmManager) FreeAll() {
	m.mu.Lock()
	blocks := make(map[uint32]*ShmBlock, len(m.blocks))
	for k, v := range m.blocks {
		blocks[k] = v
	}
	m.blocks = make(map[uint32]*ShmBlock)
	m.mu.Unlock()
	for _, blk := range blocks {
		freeShm(blk)
	}
}

// randomHex returns n random hex characters.
func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)[:n]
}

// shmCounter is incremented for each shm allocation. Combined with a per-pid
// random suffix, it guarantees uniqueness regardless of allocation rate —
// 6 hex chars of pure randomness collided via the birthday paradox once we
// crossed a few thousand allocs per pid (e.g. a /run with thousands of
// bridge calls).
var (
	shmCounter atomic.Uint64
	shmPidTag  string
	shmPidOnce sync.Once
)

func shmPid() string {
	shmPidOnce.Do(func() {
		shmPidTag = fmt.Sprintf("%d-%s", os.Getpid(), randomHex(6))
	})
	return shmPidTag
}

// shmName generates a unique shm segment name. Format:
//
//	chr-<pid>-<6hex>-<counter>
//
// The pid+random tag distinguishes processes; the counter distinguishes
// allocations within a process and is monotonic.
func shmName() string {
	return fmt.Sprintf("chr-%s-%d", shmPid(), shmCounter.Add(1))
}
