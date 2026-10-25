# Interface takeover contract

`GET /api/v1/interfaces` returns one item per physical port (including
unassigned spares and ports whose cable is down). The following keys are the
UI contract; they are additive to the existing interface fields and must not be
replaced by driver/kernel strings.

```json
{
  "id": "eth1",
  "management": false,
  "pci": "0000:13:00.0",
  "takeover_state": "taken_over | available | consent_required | unsupported",
  "takeover_mode": "dpdk_vfio | dpdk_uio | af_xdp_zero_copy | af_xdp_copy | rdma | af_packet | null",
  "takeover_class": "native | compatibility | null",
  "link_state": "up | down | unknown",
  "link_speed_mbps": 1000,
  "verification": "verified | pending_link | failed",
  "reason": "人类中文原因（仅 unsupported/consent_required）"
}
```

## Semantics

- `takeover_state` answers "can/did the product take this port over?".
- `link_state` answers "is the cable/peer live?". It is independent: a down
  link never changes `takeover_state`, never releases the port and never makes
  it `unsupported`.
- `taken_over` means the port is owned by VPP. The proof may be
  `pending_link` (cable down or not yet exercised) or `verified` (functional
  probe passed). Both are approved takeovers.
- `available` means the port is not currently owned but a takeover path is
  available. The next watcher tick or apply can take it.
- `consent_required` means the only available path is AF_PACKET and the
  persisted policy does not allow it for this port. The kernel netdev is intact.
- `unsupported` means there is no usable path, or the port is the management
  port (policy), or the driver/device is crash-guarded and no fallback is
  consented.
- `takeover_mode` and `takeover_class` are `null` when no path is known.
- `verification=failed` is used only for an unsupported port or a failed
  destructive candidate that is no longer owned.
- `reason` is empty for `taken_over` and `available`; it is a short Chinese
  sentence for `unsupported` and `consent_required`.

## What is deliberately not exposed

- No kernel driver name, driver model, kernel stack or probe command.
- No internal error code as user-facing text.
- `pci` is exposed because the operator uses it for inventory correlation; it
  is not a driver or kernel detail.

## Source of the values

- `takeover_state` / `takeover_mode` / `takeover_class`: ownership record,
  verification record, `active.json` and the cached non-destructive capability
  report.
- `link_state` / `link_speed_mbps`: live sysfs when the Linux netdev exists;
  the persisted hardware readback for a DPDK-bound port without a netdev.
- `verification`: the persisted proof, demoted to `pending_link` while the live
  link is down. The watcher promotes it on link up without an apply.
