# Upstream-First Replacement Policy

This policy applies to VPP, PPPoE, systemd, runtime apply, packaging, and any
future firmware feature.

## Rule

Do not keep patching a self-maintained implementation indefinitely when its
root cause is unstable or expensive to fix. First establish the root cause,
impact, and repair cost. If the risk is high, the schedule is long, or the
same area regresses repeatedly, evaluate a replacement before adding another
patch.

Preference order:

1. VPP native plugin/API.
2. An officially maintained upstream implementation.
3. A mature third-party implementation (for example a maintained PPPoE client
   plugin).
4. Continue maintaining the in-tree implementation.

A mature implementation must not be rejected merely to keep local control.
Conversely, it must not be adopted merely to avoid work: the existing product
API, configuration, readback, and UI contracts must remain intact.

## Required evidence for a replacement

Every replacement assessment records:

- Candidates and exact upstream source, version/tag/commit.
- License and provenance; update `PROVENANCE`/`README.provenance`.
- Why the current implementation is being replaced (root cause, regressions,
  risk, cost).
- Contract impact: config schema, API response, readback fields, unit names,
  CLI, upgrade/rollback.
- Adapter boundary and a runtime fallback switch.
- Build recipe and artifact hash; no unreviewed prebuilt binary.
- Unit, integration, and live dataplane tests.
- Rollback procedure and the old implementation retained until acceptance.
- Known gaps of the upstream implementation; it is not assumed bug-free.

## Current module assessment (2026-10-20)

| Area | Current | Decision | Next step |
|---|---|---|---|
| systemd health | custom `systemctl` subprocess wrapper + cache | **Replace candidate** | Add `ServiceProbe` adapter backed by upstream `go-systemd/dbus`; keep CLI probe as fallback. |
| PPPoE client | Hi-Jiajun/Raydo-derived plugin, locally patched | **Stop piling patches; pin and evaluate upstream** | Pin the upstream revision, diff local patches, evaluate VPP native `pppoe_plugin` and upstream plugin; keep adapter/fallback. |
| VPP read-only snapshots | `vppctl` process per command + 3s cache | **Evaluate official GoVPP** | Introduce a snapshot interface; use GoVPP binary API where stable, fallback to vppctl. |
| NAT / pre-NAT routing | VPP NAT44 + custom pre-NAT plugin | **Evaluate upstream ABF/PBR** | Compare contracts and live behavior; keep custom plugin behind adapter until proven. |
| Upgrade | custom shell installer | **Evaluate RAUC/swupdate** | Keep current verified installer as fallback. |

## Rule for this branch

The P0 fixes already committed are treated as stabilization and evidence, not
as a permanent endorsement of the current implementations. Any further work
on a module marked "replace candidate" or "evaluate" must first produce the
assessment above, not another speculative patch.
