# Dataplane breadth: any NIC, evidence-based fallback

## Driver/model is not a gate

The native path accepts any PCI NIC that VPP attaches through DPDK. The probe
records the hardware driver/model (`driver_model`, for example
`VMware vmxnet3` or `Intel e1000`) and the VPP link state (`link_state`) plus
the hardware carrier line (`hardware_link`) as evidence. It never requires an
Intel `e1000`/`igb` string.

A takeover candidate is accepted as high-performance native only when:

- the VPP interface for that PCI exists in `show hardware-interfaces`;
- `show interface <name>` reports the interface state as `up`.

The model string is only used in failure explanations.

## Destructive candidates and rollback

DPDK/UIO takeover is destructive: the Linux netdev disappears. The apply
bootstrap therefore treats it as a candidate, not a fact:

1. bind the candidate ports (vfio where isolated, otherwise uio);
2. restart VPP;
3. verify each port by PCI -> VPP interface -> `show interface` state;
4. if a port fails verification, run `release-dpdk-port.sh`:
   - unbind `uio_pci_generic`/`vfio-pci`;
   - restore the recorded original kernel driver (`vmxnet3`, `igb`, ...);
   - remove that port's `dev` line from `/etc/vpp/startup.conf`;
   - remove the port from the ownership record;
5. restart VPP again and re-probe. The restored netdev is now available to the
   ordered AF_XDP / AF_PACKET candidates.

No port is left half-bound. If every candidate fails, the port is reported as
`dataplane_locked` with its name and reason.

## Boot behavior

`prepare-vfio.sh` in boot mode only re-binds ports that were previously
verified and committed as DPDK (`active.json` tier `vpp_dpdk`). An unverified
candidate stays on its kernel driver. This prevents a failed takeover from
surviving a reboot, and it allows AF_PACKET to remain reachable.

When there is no verified DPDK owner, the preflight removes any stale
`# BEGIN LY ROUTE DPDK` block and disables `dpdk_plugin.so`. `data_interfaces:
null` or `[]` is treated as an empty inventory, never as a Python TypeError.

## Link state on vmxnet3

On vmxnet3 the VPP `show interface` state and the hardware readback can differ.
The probe records both:

- `link_state` — VPP interface state (`up`/`down`);
- `hardware_link` — `carrier up`/`carrier down` from the hardware block.

A down DPDK link is not approved as native; the bootstrap restores the kernel
driver and uses AF_PACKET (or another native hook). A down link does not by
itself prevent the port from being assigned: the AF attachment still commits
(and the runtime is reported degraded). This is why an isolated port group can
show `carrier down` yet the port remains usable in the compatibility path.

## Proof ownership

An empty probe result does not erase an existing proof for the same realizable
interface set. The probe preserves the existing proof when every interface in
it still has a Linux netdev or a product-owned DPDK binding. If the interface
set changed, or the old proof's ports are no longer realizable, the empty result
is written and the apply is locked with the per-port reason.

## vmxnet3 live result

On the ESXi gateway VM, VPP did create `GigabitEthernet13/0/0` and
`GigabitEthernet1b/0/0` for the two vmxnet3 data ports, but both reported
`down`. Bootstrap therefore restored `vmxnet3`, removed both startup entries,
and the ports were committed through AF_PACKET. With consent, apply returned
`202 degraded` with `AF_PACKET 兼容模式（性能受限）`; without consent it returned
`409 af_fallback_required` with ports `["eth1","eth2"]`.
