package vpp

import (
	"os"
	"strings"
)

// DPDKTransportOverride returns the operator's appliance-wide DPDK transport
// consent from LY_ROUTE_DPDK_TRANSPORT. The value is intentionally read from
// the environment instead of the persisted capability proof: N5100
// (Jasper Lake + igc) reports an isolated IOMMU group but VFIO EAL always
// fails with ENOMEM, so the operator's choice must win over hardware-derived
// defaults in every path that binds a device or renders VPP's startup config.
func DPDKTransportOverride() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_TRANSPORT"))) {
	case "uio":
		return "uio"
	case "vfio":
		return "vfio"
	default:
		return ""
	}
}
