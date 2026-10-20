# Product Boundaries

## Orchestrator scope

- Orchestrator is a **single-node local VPP traffic orchestrator**.
- It manages local topology, groups, policies, VPP service-chain/transparent orchestration, telemetry, snapshots and runtime apply.
- Multi-site / cross-gateway registration, heartbeat and centralized distribution are **out of scope** for this design. They must not be invented in the current product.

## Distribution and product conversion

- Orchestrator is distributed as an **upgrade/conversion package**, not a separate ISO.
- The intended installation path is:
  - install the gateway appliance image;
  - upgrade/convert it to Orchestrator;
  - optionally convert back to Gateway.
- Gateway <-> Orchestrator conversion must:
  - use the same base OS/dataplane/version family;
  - preserve or verify VPP and dataplane packages;
  - run transactionally with backup and rollback;
  - preserve the previous enabled/active service state;
  - be signed and version-checked.
- Conversion is **not lossless by definition**: product-specific configuration and database schemas may require migration or an explicit reset.
- Recommended build model:
  - one universal base rootfs/disk with both product profiles;
  - small Gateway <-> Orchestrator conversion packages;
  - keep a recovery image/package even when the normal path is upgrade-only.

## External proxy integration

- Traffic steering to an external nginx-like proxy is **L3 service chaining / policy steering, not an L2 bridge**.
- A single-port proxy can be supported by:
  1. two logical L3 hand-offs (for example 802.1Q ingress/egress VLANs) on one physical port; preferred;
  2. one-arm TPROXY / transparent mode with explicit return routing and loop prevention;
  3. explicit HTTP proxy mode with policy-based redirection (limited to supported application traffic).
- The current local Xray two-TAP service network is an implementation precedent, not a ready-made external-appliance integration.
- Nginx-like proxies only cover their supported protocols (typically TCP/HTTP/HTTPS); UDP/QUIC and arbitrary transparent traffic require a different proxy.
- Integration must define: health checks, bypass/fail-closed behavior, asymmetric-return validation, MTU/MSS handling, conntrack/state, loop prevention and rollback.
