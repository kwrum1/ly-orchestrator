# Dataplane takeover references and ordered policy

This document records the mechanisms we studied before changing the takeover
policy. It is deliberately concrete: every claim below maps to a mechanism that
exists in the cited project or in the Linux kernel, and every policy choice is
tied to evidence we can reproduce on the appliance.

Operator requirement being implemented: **takeover capability and link state are
orthogonal**. A port whose cable is unplugged must still be taken over, and the
UI must keep reporting that capability.

## 1. What other projects actually do

### 1.1 Cilium XDP acceleration (the "does not take the NIC away" model)

Cilium's `loadBalancer.acceleration` feature attaches an eBPF program at the
native XDP hook of an existing netdevice. It never unbinds the kernel driver
and never hides the netdevice from Linux. Relevant properties:

- The netdevice stays owned by its kernel driver (`ixgbe`, `ice`, `mlx5`, ...).
- Acceleration is **best effort per device**: `native` requires native XDP
  support, `best-effort` enables it only on devices that support it. A device
  without native XDP keeps working through the normal stack.
- The feature is enabled on the device used for direct routing; it is not a
  global destructive takeover.
- Because the netdevice persists, `link down` has no effect on *whether* the
  acceleration is configured. Link state is a property of the cable, not of the
  acceleration capability.

Source: Cilium docs, "LoadBalancer & NodePort XDP Acceleration"
(`loadBalancer.acceleration=native|best-effort`, native XDP driver support,
`best-effort` for mixed device support) and "XDP drivers".

What we adopted:

- A down link is a **state of the proof**, not of ownership. Ownership survives.
- Capability is reported for every physical port, including unassigned spares.
- Per-device best-effort: a port that cannot use a native path is not a global
  `dataplane_locked`; it falls through to the next path, and only becomes a
  consent question when the remaining path is AF_PACKET.

What we rejected:

- Plain XDP acceleration as the *forwarding* dataplane. Cilium's XDP program
  only accelerates specific NodePort/DSR paths; it is not a full L3 forwarder
  with NAT, ACL, QoS and routing. Ly Route needs VPP as the forwarder, so we
  use XDP only through AF_XDP (a socket that VPP can own), not as the datapath.

### 1.2 AF_XDP (kernel documentation)

- AF_XDP is a socket that receives frames redirected by an XDP program. It does
  not move the PCI device to another driver; the netdevice and its kernel
  driver stay in place.
- Two modes: `XDP_ZEROCOPY` (NIC DMAs directly into the user-space UMEM) and
  `XDP_COPY` (kernel copies). Zero-copy needs driver/NIC support; copy mode is
  the compatible fallback.
- The kernel can report zero-copy support (`NETDEV_XDP_ACT_XSK_ZEROCOPY` via
  `XDP_FEATURES`, and `XDP_OPTIONS_ZEROCOPY` after bind).

Source: Linux kernel `Documentation/networking/af_xdp.html`.

What we adopted:

- A non-destructive, stack-preserving path is a first-class high-performance
  candidate. It keeps the netdevice, so:
  - the management/fallback path is never lost,
  - a cable-down port can still be attached (the netdevice is admin-up even
    when carrier is down),
  - an admin-up crash in the PMD can be recovered by deleting the VPP interface
    without a PCI re-bind.
- Zero-copy is preferred over copy mode, but only when the VPP runtime probe
  actually created a zero-copy interface. The probe is non-destructive to the
  kernel binding (it creates and deletes a VPP interface, it does not change
  the PCI driver).

### 1.3 DPDK `dpdk-devbind`

- `dpdk-devbind --status` prints `if=`, `drv=` and `unused=` per PCI device, so
  ownership is knowable before touching a device.
- Binding is destructive: `--bind=vfio-pci <pci>` unbinds the kernel driver and
  binds `vfio-pci`; `--bind=<kernel-driver>` (or `--unbind`) restores it.
- `--force` is required for devices that carry routes; without it the tool
  refuses to break connectivity.
- `--noiommu-mode` explicitly marks the unsafe VFIO path on hosts without an
  IOMMU.

Source: DPDK `dpdk-devbind` tool documentation.

What we adopted:

- The destructive path must record the original kernel driver and must be able
  to restore it (`release-dpdk-port.sh`). A failed candidate is restored before
  the next candidate is attempted; no half-bound port is left behind.
- The management port is never bound, mirroring the `--force`/route warning.
- VFIO is preferred only when an IOMMU group protects the device; otherwise UIO
  is the explicit no-IOMMU fallback.

What we rejected:

- Shelling out to `dpdk-devbind.py` as the primary implementation. The image
  already owns the sysfs binding logic and needs to write the VPP startup
  `dev` block atomically; a Python dependency in the crash path adds a failure
  mode. `prepare-vfio.sh`/`release-dpdk-port.sh` implement the same operations
  with `driver_override` + `bind`/`unbind`, and record the same evidence.

### 1.4 VPP `af_xdp` plugin

- `create interface af_xdp <host-if> [name <vpp-name>] ... [zero-copy|no-zero-copy]`
  creates a VPP interface on top of an existing Linux netdevice.
- The kernel interface must be **admin up** before creation, otherwise Linux
  denies the AF_XDP socket. Carrier state is not the requirement.
- The plugin claims RX queues `[0, num_rx_queues)`; `num-rx-queues all` claims
  all queues, `mac-reuse` keeps the Linux MAC so VPP does not need a MAC rewrite.
- Zero-copy reaches tens of Mpps only when the driver supports it and the RX
  path runs on a dedicated core; copy mode is the compatible fallback.

Source: VPP developer docs, "AF_XDP device driver", and the `af_xdp` CLI
reference (`create interface af_xdp ...`).

What we adopted:

- `ip link set dev <if> promisc on up` before creating the AF_XDP interface,
  `num-rx-queues all`, `mac-reuse`, and an explicit `zero-copy` or
  `no-zero-copy` flag. This is exactly what `runtime-check.sh` and the apply
  transaction do.
- Because the requirement is only "admin up", AF_XDP is link-independent: a
  cable-down netdevice can still be attached.

### 1.5 VPP DPDK plugin

- `dpdk { dev <pci> ... }` in `startup.conf` makes VPP take the PCI device at
  process start. This is why a bad candidate can abort VPP itself, not just a
  single interface.
- DPDK owns the device exclusively; the kernel netdevice disappears.

What we adopted:

- A destructive candidate is only accepted after VPP started **and** the
  interface was admin-upped and functionally probed.
- A crash during that probe marks the driver/device `native_unsupported` in a
  persisted capability file. The `dev` line is removed and the kernel driver is
  restored before VPP is restarted, so systemd cannot enter a crash loop with
  the same bad `dev` line.

## 2. Resulting ordered policy

Per port, with measured/verified capability, not a driver allow-list:

| Rank | Mode | Destructive? | Keeps kernel netdev? | Link-independent? | When selected |
| --- | --- | --- | --- | --- | --- |
| 0 | `dpdk_vfio` | yes | no | yes (after bind) | IOMMU group present and `vfio-pci` available |
| 1 | `af_xdp_zero_copy` | no | yes | yes (admin up only) | VPP probe created a zero-copy AF_XDP interface |
| 2 | `dpdk_uio` | yes | no | yes (after bind) | no IOMMU, `uio_pci_generic` available, DPDK PMD attaches |
| 3 | `af_xdp_copy` | no | yes | yes | AF_XDP works but zero-copy is unsupported |
| 4 | `rdma` | no | yes | yes | RDMA plugin and device support |
| 5 | `af_packet` | no | yes | yes | last resort, requires persisted operator consent |

Why AF_XDP zero-copy now sits **before** `dpdk_uio`:

- `dpdk_uio` is destructive and leaves the port invisible to Linux. If the PMD
  is broken (the ESXi `vmxnet3` case) the port is unusable until the kernel
  driver is restored.
- AF_XDP zero-copy is non-destructive and has no PCI re-bind step. On hardware
  where the VPP AF_XDP probe succeeds, it preserves the management path and the
  cable-down behaviour, and it is measurably high-performance (VPP documents
  tens of Mpps with a supported driver and a dedicated RX core).
- The preference is applied only to a **verified** zero-copy candidate from the
  runtime probe. If zero-copy does not attach, the port falls to UIO; theory
  (driver model lists) never promotes it.

Why `dpdk_vfio` stays first:

- It is the fastest path and is IOMMU-protected. The destructive cost is bounded
  by the restore procedure and the functional proof.

Why `af_packet` stays last and consent-gated:

- It keeps the netdevice and always works, but it is a copy path with a socket
  per port. It is a compatibility mode, never a silent downgrade.

## 2.1 Auto-adoption and consent gate

- A role-assigned port with no ownership proof is adopted on the next watcher
  tick. The watcher is bounded (3 s) and rate-limits a permanently failing
  device to one attempt per 30 s.
- A crash-guarded driver/device is never retried. The guard is persisted by
  PCI address, so replacing the Linux driver after a restore does not defeat it.
- AF_PACKET is never adopted silently. Without the persisted policy consent the
  port is `consent_required`, the kernel netdev is untouched, and the state is
  not `dataplane_locked`.
- A port that already has a non-destructive takeover is never re-bound to VFIO
  or UIO by the watcher. An explicit apply may still choose VFIO when it ranks
  first, but a verified AF_XDP zero-copy proof blocks a destructive UIO bind.

## 3. Evidence from our own hardware

- ESXi `.124` / `vmxnet3`: VPP DPDK attaches `GigabitEthernet13/0/0` but
  `set interface state up` SIGSEGVs in `clib_sysfs_read_bitmap` /
  `unserialize_vnet_interface_state`. The port is not native on this platform;
  AF_PACKET is correct. This is the crash that produced the persisted
  `native_unsupported` entry.
- `igb` on the J1900: `uio_pci_generic` works (`GigabitEthernet3/0/0`,
  `4/0/0`), management stayed on `igb`. With the new policy the AF_XDP
  zero-copy probe is attempted first; if the probe fails, UIO is unchanged.
- A down link on `.124`: the port is still committed and reported as
  `taken_over` / `pending_link`; the watcher promotes it when the link rises.

## 4. References

- Cilium, "LoadBalancer & NodePort XDP Acceleration":
  https://docs.cilium.io/en/stable/network/kubernetes/kubeproxy-free/
- Linux kernel, "AF_XDP":
  https://docs.kernel.org/networking/af_xdp.html
- DPDK, "dpdk-devbind Application":
  https://doc.dpdk.org/guides/tools/devbind.html
- VPP, "AF_XDP device driver":
  https://docs.fd.io/vpp/26.02/developer/devicedrivers/af_xdp.html
- VPP, `af_xdp` CLI reference:
  https://docs.fd.io/vpp/26.06/cli-reference/clis/clicmd_src_plugins_af_xdp.html
