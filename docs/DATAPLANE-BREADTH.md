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

## Functional verification and link state

The bootstrap never trusts a status string alone. After binding it runs
`set interface state <if> up` and then verifies:

1. VPP is still alive and the interface exists;
2. if `show interface` says `up`, the port is functionally verified with method
   `link_state_up`;
3. if it says `down`, the port is accepted only when the link speed is non-zero
   and a functional TX probe increments the VPP TX counter (method
   `tx_counter_and_link_speed`; set `LY_ROUTE_DPDK_FUNCTIONAL_PEER` to a peer
   address);
4. otherwise the port is rejected, the kernel driver restored, and the AF
   path is tried.

The proof records `link_state`, `hardware_link`, `link_speed_mbps`,
`functional_verified` and `functional_method`. A functionally verified port is
native even if `link_state` says `down`; a port that is down and not
functionally verified is restored to Linux and can still be assigned through
AF_PACKET.

### vmxnet3 result

On the ESXi gateway VM, bringing the vmxnet3 DPDK ports up aborts VPP:

```text
received signal SIGSEGV ...
#2 clib_sysfs_read_bitmap
#4 unserialize_vnet_interface_state
vpp.service: Main process exited, code=killed, status=6/ABRT
```

The bootstrap treats this as a failed functional verification, restores
`vmxnet3`, removes the `dev` entries, restarts VPP, and commits AF_PACKET. So
the earlier `down` was a symptom of a broken vmxnet3 DPDK PMD on this build,
not a mere status-string false negative. Native vmxnet3 is not claimed.

A down link in the AF path does not by itself prevent assignment: the AF
attachment still commits and the runtime is reported degraded. This is why an
isolated port group can show `carrier down` yet the port remains usable in the
compatibility path.

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
