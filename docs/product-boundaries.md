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

## Testing model for Gateway and Orchestrator

- Gateway and Orchestrator use the same delivery loop:
  1. dedicated functional tester produces a feature matrix and bug report;
  2. development expert performs targeted fixes;
  3. tester reruns regression;
  4. only a clean regression can open the next gate.
- The tester is read-only and does not fix code; the development expert fixes code and builds artifacts.
- Orchestrator testing starts only after its backend reaches a testable development milestone and the user or coordinator explicitly starts it.
- Orchestrator test scope must cover: backend/API, DB/persistence, auth/audit/health, topology/groups, policy compile/apply, configuration export/snapshot/import/restore, telemetry, upgrade/version/rollback, rootfs/upgrade/disk, the real VPP orchestrator plugin, and Gateway/Orchestrator conversion once implemented.
- Orchestrator tests use an isolated temporary instance, database and ports; they must not modify the Gateway router or delete user VMs.
- Explicit exclusions: no multi-site/cross-gateway management, no Orchestrator ISO, no frontend UI testing, no security/penetration review.
- Every report must distinguish "environment not ready" from "product bug", include reproduction and evidence, and classify P0/P1/P2.
