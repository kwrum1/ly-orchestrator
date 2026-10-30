//go:build linux

package httpapi

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// AF_PACKET interfaces are created with cksum-gso-disable. That flag only says
// VPP must not expect the kernel virtio-net header; Linux can still coalesce RX
// TCP segments with GRO before the packet socket sees them. VPP then receives
// frames larger than the physical MTU and every one of them fails on transmit.
// Disable GRO on the host netdev before attaching so AF_PACKET sees the
// MTU-sized frames its no-offload contract requires.
const (
	siocEthtool = 0x8946
	ethtoolGGRO = 0x0000002B
	ethtoolSGRO = 0x0000002C
	ifreqSize   = 40 // sizeof(struct ifreq) on 64-bit Linux
	ifNameSize  = 16
)

type ethtoolValue struct {
	Cmd  uint32
	Data uint32
}

type groIfreq struct {
	Name [ifNameSize]byte
	Data unsafe.Pointer
	Pad  [16]byte // struct ifreq union tail; keeps sizeof(ifreq) == 40
}

func newGROIfreq(name string, command, data uint32) (groIfreq, *ethtoolValue, error) {
	var request groIfreq
	if name == "" {
		return request, nil, fmt.Errorf("empty network interface name")
	}
	if len(name) >= ifNameSize {
		return request, nil, fmt.Errorf("network interface name %q is too long", name)
	}
	copy(request.Name[:], name)
	value := &ethtoolValue{Cmd: command, Data: data}
	request.Data = unsafe.Pointer(value)
	return request, value, nil
}

func ethtoolGRO(name string, command, data uint32) (uint32, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	request, value, err := newGROIfreq(name, command, data)
	if err != nil {
		return 0, err
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(siocEthtool), uintptr(unsafe.Pointer(&request)))
	runtime.KeepAlive(request)
	runtime.KeepAlive(value)
	if errno != 0 {
		return 0, errno
	}
	return value.Data, nil
}

func groUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENODEV) ||
		errors.Is(err, unix.ENXIO) ||
		errors.Is(err, unix.ENOTTY)
}

// disableHostGRO turns off generic receive offload on the Linux netdev that
// VPP is about to attach. Missing or GRO-incapable devices are ignored: the
// subsequent create reports the real problem, and a device without GRO has
// nothing to coalesce.
func disableHostGRO(name string) error {
	enabled, err := ethtoolGRO(name, ethtoolGGRO, 0)
	if err != nil {
		if groUnsupported(err) {
			return nil
		}
		return err
	}
	if enabled == 0 {
		return nil
	}
	if _, err := ethtoolGRO(name, ethtoolSGRO, 0); err != nil {
		if groUnsupported(err) {
			return nil
		}
		return err
	}
	enabled, err = ethtoolGRO(name, ethtoolGGRO, 0)
	if err != nil {
		if groUnsupported(err) {
			return nil
		}
		return err
	}
	if enabled != 0 {
		return fmt.Errorf("GRO is still enabled on %s", name)
	}
	return nil
}
