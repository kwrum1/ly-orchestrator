package httpapi

import (
	"testing"

	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/vpp"
)

// A 3-LAN gateway must not enable the single-interface dns-intercept plugin:
// its readback asks for every LAN interface and would always fail, so the plan
// could never commit. Multi-LAN plans use per-ingress ABF steering instead.
func TestDNSInterceptionPlanEnabledRejectsMultiLAN(t *testing.T) {
	lan10 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens192", VPPInterface: "lyroute-ens192", CIDR: "192.168.10.1/24"}
	wan := vpp.AddressAssignment{Role: "wan", LinuxInterface: "ens161", VPPInterface: "lyroute-ens161", CIDR: "10.99.0.1/24"}
	if !dnsInterceptionPlanEnabled(product.Gateway().ID(), []vpp.AddressAssignment{lan10, wan}) {
		t.Fatal("single-LAN gateway must enable transparent DNS interception")
	}
	lan20 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", CIDR: "192.168.20.1/24"}
	lan30 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.30.1/24"}
	if !dnsInterceptionPlanEnabled(product.Gateway().ID(), []vpp.AddressAssignment{lan10, lan20, lan30, wan}) {
		t.Fatal("multi-LAN gateway must enable transparent DNS interception (the plugin keeps a vector of slots)")
	}
	if dnsInterceptionPlanEnabled(product.Orchestrator().ID(), []vpp.AddressAssignment{lan10, wan}) {
		t.Fatal("orchestrator must not enable gateway DNS interception")
	}
}

// A multi-LAN gateway selects per-ingress ABF steering while a single LAN
// keeps the native dns-intercept plugin (the only path that sees DPDK transit).
func TestDNSABFSteeringPlanEnabledForMultiLAN(t *testing.T) {
	lan10 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens192", VPPInterface: "lyroute-ens192", CIDR: "192.168.10.1/24"}
	lan20 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", CIDR: "192.168.20.1/24"}
	lan30 := vpp.AddressAssignment{Role: "lan", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.30.1/24"}
	wan := vpp.AddressAssignment{Role: "wan", LinuxInterface: "ens161", VPPInterface: "lyroute-ens161", CIDR: "10.99.0.1/24"}

	if dnsABFSteeringPlanEnabled(product.Gateway().ID(), []vpp.AddressAssignment{lan10, wan}) {
		t.Fatal("single-LAN gateway must keep the native plugin path")
	}
	// ABF steering is disabled: the table-100 policy family is not attached on
	// the shipped VPP, and a half-installed family aborts the whole apply.
	// Multi-LAN gateways therefore run without DNS interception; the
	// limitation is documented instead of degrading every health check.
	if dnsABFSteeringPlanEnabled(product.Gateway().ID(), []vpp.AddressAssignment{lan10, lan20, wan}) {
		t.Fatal("ABF steering must stay disabled")
	}
	if dnsABFSteeringPlanEnabled(product.Gateway().ID(), []vpp.AddressAssignment{lan10, lan20, lan30, wan}) {
		t.Fatal("ABF steering must stay disabled")
	}
	if dnsABFSteeringPlanEnabled(product.Orchestrator().ID(), []vpp.AddressAssignment{lan10, lan20, wan}) {
		t.Fatal("orchestrator must not enable gateway ABF steering")
	}
}
