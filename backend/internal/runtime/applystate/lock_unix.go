//go:build unix

package applystate

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockFile takes an exclusive advisory lock across processes. The in-process
// mutex alone cannot protect against two control processes (for example a
// controller plus a manual `vpp-apply`/CLI helper); the lock file makes the
// read-modify-write sequence serial across both.
func (store *Store) lockFile() (func(), error) {
	path := store.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("apply state: create lock directory: %w", err)
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("apply state: open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err = syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = handle.Close()
			return nil, fmt.Errorf("apply state: another writer holds %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
		_ = handle.Close()
	}, nil
}
