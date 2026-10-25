# Dataplane takeover and fallback policy

This document records the product decision for physical NIC takeover, including
IOMMU-less hardware such as the Intel J1900 / Bay Trail i211 boards.

## Ordered fallback (per interface)

Every configured data port independently tries the following transports, in
order:

1. `dpdk` + `vfio-pci` (IOMMU-isolated devices)
2. `af_xdp` zero-copy (non-destructive, keeps the kernel netdevice)
3. `dpdk` + `uio_pci_generic` (no usable IOMMU group)
4. `af_xdp` copy
5. `rdma`
6. `af_packet` compatibility mode (explicit operator consent only)

AF_XDP zero-copy ranks above the destructive no-IOMMU DPDK path because it is
verified only when the VPP runtime probe actually created a zero-copy
interface. It keeps the kernel netdevice (and therefore the management/fallback
path), does not require a PCI re-bind, and is link-independent: only the Linux
interface needs to be admin-up, not its carrier. See
`docs/DATAPLANE-REFERENCES.md` for the evidence behind the order.

Capability and link state are orthogonal. A down link never changes the order,
never demotes a takeover, never un-binds a port and never produces
`dataplane_locked` by itself. A taken-over port with a down link is reported as
`verification=pending_link`; a watcher promotes it to `verified` when the link
rises, without a new apply request.

A port that cannot use any of these is not assigned a LAN/WAN role. An
unassigned unsupported port does not block an apply. Only a configured/assigned
port that cannot be realized is an apply-blocking condition, and the error names
that port.

Consent is keyed by the product interface id (`eth0`, `eth1`, ...), which is
derived from the PCI address and is stable before and after a DPDK bind. The
policy API accepts a Linux netdev name for backwards compatibility and stores
the canonical product id; matching accepts either form. The
`af_fallback_required` error always lists product ids.

## Ownership preflight

`/usr/lib/ly-route/prepare-vfio.sh` binds each selected data PCI independently:

- isolated IOMMU group -> `vfio-pci`;
- no usable IOMMU group -> `uio_pci_generic`;
- a single appliance may mix both transports (`uio-driver auto` in the VPP
  startup block);
- the management PCI is always excluded and is never bound to a DPDK driver.

The data PCI list is derived, in precedence order, from:

1. `/etc/ly-route/vfio-devices` (explicit `name|mac|pci|role` rows);
2. `/etc/ly-route/dataplane-inventory.json` written by the control API from
   operator-configured interfaces (the normal API-driven case);
3. `/etc/ly-route/installed-network.json` written by the installer.

The script verifies the actual kernel driver after every bind and fails closed
with `VPP ownership locked: ...` if the binding did not take effect. It records
the original kernel driver and whether the product created the ownership in
`/var/lib/ly-route/dataplane/ownership.json`. Rollback only returns devices to
Linux when that record says the product created the ownership; externally owned
DPDK bindings are left untouched. Repeated applies do not re-bind a device that
VPP already owns.

## AF_PACKET consent gate

AF_PACKET is never selected silently. The probe always records it as the last
candidate with `acceptance_only=true` and `high_performance=false`.

The persisted policy API is:

```text
GET /api/v1/dataplane/policy
PUT /api/v1/dataplane/policy
```

Both use this shape. `ports` contains product interface ids (`eth0`, `eth1`, ...
as returned by `/api/v1/interfaces`); an empty list means every port that would
need AF_PACKET.

```json
{ "af_packet_fallback": false, "ports": [] }
```

Enabling consent for the listed ports:

```bash
curl -sk -b cookies.txt -X PUT -H 'Content-Type: application/json' \
  -d '{"af_packet_fallback":true,"ports":["eth2","eth3"]}' \
  https://DEVICE/api/v1/dataplane/policy
```

If an assigned port can only be realized through AF_PACKET and consent is
absent, `POST /api/v1/runtime/apply` returns HTTP 409 with a machine-readable
error and no partial state:

```json
{
  "status": "af_fallback_required",
  "runtime_state": "degraded",
  "error": {
    "code": "af_fallback_required",
    "ports": ["eth2", "eth3"],
    "message": "AF_PACKET 兼容模式需要操作员明确确认：该模式仍由 VPP 转发，但性能受限。请在数据面策略中允许这些端口使用 AF_PACKET 兼容模式后重试。"
  }
}
```

With consent the apply commits, the runtime state is `degraded`, and the
per-interface reason is `AF_PACKET 兼容模式（性能受限）`. Traffic still passes
through VPP's AF_PACKET attachment; this is not a kernel bypass and it is not a
fail-open path. Disabling consent re-locks the next apply that would need AF.
The consent value and `ports` are part of the runtime plan hash.

## Residual risk: DMA without an IOMMU

`uio_pci_generic` gives VPP direct DMA access to the NIC without an IOMMU
translation/protection layer. A malicious device or a driver defect could DMA
to arbitrary host memory. On J1900/Bay Trail there is no usable VT-d, so this
risk cannot be removed by configuration; the alternative is to leave the port
on the kernel stack. This is an explicit acceptance decision for IOMMU-less
hardware, not a hidden optimization. AF_PACKET does not have this DMA risk but
has lower throughput and is marked `acceptance_only`.

## Operator-facing statement

> 在无 IOMMU（无 VT-d）的硬件上，产品可以使用 DPDK + uio_pci_generic 原生接管数据网卡，并把管理网卡始终保留在内核驱动上。该路径没有 IOMMU 的 DMA 隔离保护，属于明确接受的残余风险；如果网卡无法原生接管，只有在操作员通过数据面策略明确允许后，才会使用 VPP AF_PACKET 兼容模式（性能受限，状态为 degraded）。未确认时应用会返回 af_fallback_required，不会静默降级，也不会绕过 VPP 转发。
