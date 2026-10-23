# VPP FIB table lifecycle for gateway apply

## Incident

A failed Any-to-Any route-policy apply caused VPP 26.06 to abort:

```text
fib/entry: BUG: ipv4 table 67670 (index 2) is not empty
ip route: Nonexistent table id 59657
received signal SIGSEGV
```

The crash was reproducible through the apply graph: a failed route-policy
install/rollback recreated a private FIB while the old table still owned
routes, and the next route add crashed in
`fib_attached_export_import`/`fib_table_lookup_exact_match`.

## Root cause

VPP 26.06 has two unsafe CLI lifecycle behaviours:

1. `ip table del <id>` aborts the process when the table still owns a route.
2. Deleting and immediately recreating a table can leave stale
   attached-export state. A later `ip route add` can then crash even when the
   table looked empty.
3. A removed default path is retained by VPP as a special drop entry, so
   "route delete succeeded" does not mean `show ip fib table` is empty.
4. The optimized radix route chain duplicated the LAN bypass as a
   `via <interface>` route in the private FIB. Adding that connected path
   immediately after the lookup-DPO default route triggered the same VPP
   crash during a failed policy install.

## Fix

- **Never send destructive `ip table del` to VPP.** Private tables use stable
  resource-hash IDs and are retained for the lifetime of the VPP process.
- When a delete is requested, enumerate the live FIB routes and remove them
  first. The retained table is then left as a blackhole sentinel, which is
  unreferenced once the pre-NAT classifier/ABF objects are removed.
- Make `ip table add` idempotent: skip it when the table already exists.
- Skip route deletes aimed at a nonexistent table instead of feeding VPP
  invalid FIB IDs.
- Do not duplicate the LAN-to-LAN bypass in the radix private FIB. The
  pre-NAT classifier installs rule `64999` (`LAN -> LAN bypass`) before the
  policy table and is the authoritative bypass.
- Treat a table with only a drop default as absent during readback. A table
  with a usable default path (`ipv4 via`, `lookup in ipv4-VRF`) is still live
  and fails closed.
- Route-policy snapshots always request `show ly-route pre-nat-route`
  (optional). During rollback, a live native classifier is accepted as
  authoritative even when the old plan cannot rebuild its radix path and no
  legacy ACL exists.

## Validation

Lab: clean 209 guest, current main `91b49bb` + this change, one failed
route-policy apply/rollback cycle repeated three times.

| Cycle | Apply | Rollback | VPP restarts |
| --- | --- | --- | --- |
| 1 | `apply_failed` | `rolled_back` | 0 |
| 2 | `apply_failed` | `rolled_back` | 0 |
| 3 | `apply_failed` | `rolled_back` | 0 |

After all three cycles:

- VPP remained `active`, `NRestarts=0`; no `SIGSEGV`, `fib/entry: BUG`,
  `Nonexistent table id`, or `Main process exited`.
- PPPoE remained connected and the WAN ping path stayed ready.
- Port map `18080` remained installed.
- `lydnscdd4e1` remained a NAT44 input interface.
- Removing the test policies and applying the normal desired state committed
  (`runtime-6d7040df82055d8b6d158742c9e0d597`), with the port map and DNS role
  present on the final generation.

The repeated failure was injected by failing the VPP route batch that carries
the new policy's pre-NAT rule; no product code path is test-only.

## Regression coverage

- `table_lifecycle_test.go`: destructive table deletes are skipped, existing
  routes are removed, missing-table route commands are skipped, table adds are
  idempotent, and drop vs forwarding default detection.
- `route_policy_native_restore_test.go`: restore readback accepts a live native
  pre-NAT classifier without a legacy ACL, and unsupported pre-NAT inventory
  falls back safely.
- `vppctl_absence_test.go`: retained blackhole FIB proves absence while a live
  forwarding default does not.
- `route_policy_scope_test.go`: optimized route policies keep the LAN bypass in
  the pre-NAT classifier and no longer duplicate it in the private FIB.
- `production_gateway_closure_test.go`: failed apply rollback uses route
  cleanup, never a destructive table delete.
- `executor_test.go`: a failed apply, rollback, and replay can repeat three
  times without losing the prior generation.

## Boundary

This change fixes the VPP table/FIB crash and the failed-apply replay path.
Rebinding a port mapping's external address after a PPPoE session/address
change remains part of the existing P0-5 NAT return-guard work and is not
expanded here.
