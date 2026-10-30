//go:build !unix

package applystate

// Non-unix builds (developer workstations) still get the in-process mutex and
// the atomic rename. The shipped appliance is unix and uses flock.
func (store *Store) lockFile() (func(), error) {
	return func() {}, nil
}
