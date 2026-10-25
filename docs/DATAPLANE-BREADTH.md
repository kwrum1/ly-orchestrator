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
3. if it says `down`, the port is verified when the link speed is non-zero and
   a functional TX probe increments the VPP TX counter (method
   `tx_counter_and_link_speed`; set `LY_ROUTE_DPDK_FUNCTIONAL_PEER` to a peer
   address);
4. if it says `down` and no counter proof is available, the takeover is still
   **approved** as `verification=pending_link`. The port is not restored; a
   watcher promotes it on link up without a new apply.
5. only an admin-up failure, a missing VPP interface or a VPP/driver crash is a
   rejection: the kernel driver is restored and the failure is persisted in the
   crash guard.

The proof records `link_state`, `hardware_link`, `link_speed_mbps`,
`functional_verified`, `verification` and `functional_method`. A functionally
verified port is native even if `link_state` says `down`. A down link changes
only `verification` (to `pending_link`), never `takeover_state`, and never
un-binds the port.

### Link independence and auto-adoption

- `takeover_state` answers "can this product take the port over?"; `link_state`
  answers "is the cable/peer live?". They are independent fields.
- The watcher runs every 3 seconds (bounded, idempotent) in both Gateway and
  Orchestrator. It reacts to link up, a new PCI device and a new interface on a
  role-assigned port.
- Auto-adoption never binds the management port, never binds a port without a
  role, and never re-binds a port that is already owned by a non-destructive
  path (AF_XDP/AF_PACKET/RDMA keep their kernel netdevice).
- A crash-guarded driver/device is never retried automatically. If AF_PACKET is
  the only remaining path and consent is absent, the port is reported as
  `consent_required` and the kernel netdev stays intact; it is not
  `dataplane_locked`.
- `GET /api/v1/interfaces` reports `takeover_state`, `takeover_mode`,
  `takeover_class`, `link_state`, `link_speed_mbps`, `verification`, `reason`,
  `management` and `pci` for every physical port, including unassigned spares
  and ports that are currently down. No driver/model/kernel string is exposed.

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

### Auto re-adoption after VPP restart

When VPP restarts it recreates only the devices in `startup.conf`. AF_PACKET,
AF_XDP and RDMA interfaces are created by the apply transaction and would
otherwise disappear. The watcher detects an ownership proof whose VPP interface
is missing and re-creates it without an apply:

- AF_PACKET: `create host-interface name <if>`, rename `host-<if>` to
  `lyroute-<if>`, restore the Linux MAC, admin-up;
- AF_XDP: re-create with the recorded zero-copy/copy mode after admin-up;
- the Linux netdev is never removed and no PCI re-bind is attempted.

The interface-existence check parses the `show interface` table instead of
searching for the name as a substring, because VPP echoes CLI errors (which
contain the name) on stdout.

A port that has no ownership proof and no role is never adopted. A port whose
only remaining path is AF_PACKET and whose consent is absent is reported as
`consent_required`; the watcher leaves the private netdev intact and drops the
stale proof instead of pretending the port is still owned.

### Proof ownership

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
