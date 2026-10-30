//go:build !linux

package httpapi

// AF_PACKET is a Linux dataplane. Other build targets keep the call site
// compilable without carrying Linux ioctl dependencies.
func disableHostGRO(string) error {
	return nil
}
