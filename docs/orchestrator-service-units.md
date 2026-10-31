# Orchestrator systemd unit boundary

The orchestrator appliance ships only these units. The control-plane binary must
never start, enable, restart or query a gateway-only unit on the orchestrator
product path.

## Unit inventory

| Purpose | Units |
| --- | --- |
| Control plane | `ly-route-control-api.service` |
| Dataplane | `vpp.service`, `ly-route-vpp-apply.service`, `ly-route-vpp-session-enable.service`, `ly-route-vpp-tune.service` |
| Infrastructure | `ly-route-firstboot.service`, `ly-route-recovery.service`, `ly-route-runtime-check.service`, `ly-route-vfio-preflight.service`, `ly-route-pingora.service` |

Everything else is gateway-only and must not appear in the orchestrator path:

- `smartdns.service`, `dnsmasq.service`, `xray.service`, `ly-route-pppoe-*`
- `ly-route-dns-vpp-datapath.service`, `ly-route-dns-vpp-session.service`,
  `ly-route-dns-vpp-proxy*.service`, `ly-route-dns-ipset-sync.*`
- `ly-route-policy-routing.service`, `ly-route-wan*.service`, `nftables.service`,
  `radvd.service`, `nginx.service`

## Enforcement in the control plane

- `product.Orchestrator()` allows only `control-api` and `vpp`; every capability
  and component list is filtered with `profile.AllowsService`.
- `degradedRuntimeCapabilities` and `runtimeStatusComponents` only probe the
  services the active profile allows, so `/api/v1/interfaces` no longer carries
  `smartdns_runtime_apply`/`dnsmasq_runtime_apply`/`xray_runtime_apply` entries
  on the orchestrator.
- `captureRunningServices` only snapshots allowed services before a dataplane
  transition.
- `requestDNSPostApplyRepair` returns immediately unless the profile is the
  gateway product, and it verifies the heal unit's `LoadState` (cached for five
  minutes) before starting it. A missing unit is skipped silently.
- The upgrade installer only reloads `nginx.service` when `systemctl cat` finds
  the unit; the orchestrator edge is `ly-route-pingora.service`.

## Appliance cleanup (10.1.18.234, 2026-11-01)

- Removed dangling `multi-user.target.wants/systemd-resolved.service` symlink
  (target unit not installed).
- Removed unused gateway DNS units that referenced missing units:
  `ly-route-dns-ipset-sync.service`, `ly-route-dns-ipset-sync.timer`,
  `ly-route-dns-vpp-session.service` (requires missing
  `ly-route-dns-vpp-proxy*.service`). Originals are backed up in
  `/root/systemd-backup-027`.
- `systemctl --failed` is empty; all gateway units report `LoadState=not-found`.
