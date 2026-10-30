package vpp

import (
	"os"
	"path/filepath"
	"strings"
)

// AF_XDP and RDMA attachments cannot work on paravirtual NICs. Probing them by
// actually creating a VPP interface is a dataplane event: on vmxnet3 the attach
// fails only after touching the live port, and the surrounding create/delete
// cycle used to be blamed for dataplane restarts. The verdict is therefore
// decided read-only from the kernel driver and persisted in the capability
// proof (valid for the proof lifetime), never re-probed on every apply.
//
// This is the control-plane half of the crash guard. It also rejects a stale or
// hand-edited proof that still claims an AF_XDP/RDMA candidate on one of these
// drivers, and the dataplane watcher uses it before recreating an attachment.
var nativeProbeUnsupportedDrivers = map[string]bool{
	"vmxnet3":    true,
	"e1000":      true,
	"e1000e":     true,
	"virtio_net": true,
}

// NativeProbeDriverUnsupported reports whether an AF_XDP/RDMA candidate must be
// rejected for the given kernel driver.
func NativeProbeDriverUnsupported(hook NativeHook, driver string) bool {
	supported, known := NativeProbeDriverVerdict(hook, driver)
	return known && !supported
}

// NativeProbeDriverVerdict is the tri-state form of the persisted read-only
// driver verdict. known=false means the report did not record enough evidence
// (missing driver, or a hook with no driver verdict); callers must treat that
// as unknown, not as support.
func NativeProbeDriverVerdict(hook NativeHook, driver string) (supported bool, known bool) {
	switch NativeHook(strings.ToLower(strings.TrimSpace(string(hook)))) {
	case NativeHookAFXDP, NativeHookRDMA:
	default:
		return false, false
	}
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver == "" {
		return false, false
	}
	return !nativeProbeUnsupportedDrivers[driver], true
}

// NativeProbeUnsupportedNetdev reports the kernel driver of a Linux netdev when
// that driver cannot host an AF_XDP/RDMA attachment. It returns "" when the
// netdev is unknown or its driver is not on the deny list.
func NativeProbeUnsupportedNetdev(linuxInterface string) string {
	linuxInterface = strings.TrimSpace(linuxInterface)
	if linuxInterface == "" {
		return ""
	}
	root := strings.TrimSpace(os.Getenv("LY_ROUTE_SYSFS_ROOT"))
	if root == "" {
		root = "/sys"
	}
	link := filepath.Join(root, "class", "net", linuxInterface, "device", "driver")
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	driver := strings.ToLower(filepath.Base(strings.TrimSpace(target)))
	if nativeProbeUnsupportedDrivers[driver] {
		return driver
	}
	return ""
}
