# VM Functional Lab

This directory is the functional-acceptance lab for the three-VM topology:

```text
pppoe-server (WAN) <-> router (WAN/LAN) <-> client (LAN)
```

## Commands

```bash
# Build seeds, bridges/taps and start the three VMs.
scripts/acceptance/vm-lab/lab.sh up

# Show VM status.
scripts/acceptance/vm-lab/lab.sh status

# SSH helpers.
scripts/acceptance/vm-lab/lab.sh ssh router
scripts/acceptance/vm-lab/lab.sh ssh server
scripts/acceptance/vm-lab/lab.sh ssh client

# Stop VMs and remove taps/bridges.
scripts/acceptance/vm-lab/lab.sh down
```

## UI-only configuration rule

All WAN/LAN/PPPoE/DHCP/DNS/NAT/QoS/security writes must be performed in the
real UI. API calls are allowed only for readback and evidence collection.
Playwright/browser automation must accept UI confirm dialogs; it must not call
write APIs directly.

## Router VMnet dataplane

The router disk is prepared with:

```text
LY_ROUTE_VMNET_AF_PACKET_ACCEPTANCE=true
LY_ROUTE_VPP_DATA_INTERFACES=eth1,eth2
```

This switch is explicit and test-only. Physical hosts keep the native/DPDK
first selection order. The VM fallback is AF_PACKET host-interface.

## Current phase

Functional acceptance first; UI redesign and UI acceptance only after the
functional matrix passes. VM throughput is not a production benchmark.
