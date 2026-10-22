# Apply transaction dependency graph (209)

Branch: `feat/apply-graph-209` (base `f739ffb`).

This replaces the phase-specific apply ordering with an explicit dependency
graph and a strict reverse rollback. The previous split (pre-gateway PPPoE /
post-gateway services / Linux routing retries) is gone from the production
path; a compatibility adapter remains only for legacy/test gateway doubles.

## Graph model

`backend/internal/runtime/apply/apply_graph.go`

- Nodes have `Run`, `Verify`, `Rollback` and a timeout.
- Every edge has a wait predicate, timeout and failure policy (optional or
  required).
- `Run` validates node names, missing dependencies and cycles, derives a stable
  topological order, waits every edge predicate, then runs/verifies each node.
- `Rollback` walks executed nodes in strict reverse topological order and keeps
  going after a failure, aggregating one actionable error per node.
- Failures are `GraphError{Node,From,To,Phase,Timeout,Cause}`; the message names
  the missing predicate or node and its timeout.

## Nodes and edges

| Node | Depends on | Wait predicate | Timeout |
| --- | --- | --- | --- |
| `pppoe` | interfaces | helper status + VPP session interface | 90s edge |
| `interfaces` | - | - | 2m |
| `bonds` | interfaces | - | 2m |
| `wan-groups` | bonds | - | 2m |
| `service-networks` | interfaces, pppoe | PPPoE status/interface | 2m node |
| `routes` | wan-groups, service-networks | DNS TAP + NAT44 input role | 30s edge |
| `acls` | routes | - | 2m |
| `qos` | routes, interfaces | - | 2m |
| `nat44` | interfaces, service-networks | DNS NAT role | 3m |
| `port-maps` | nat44 | NAT44 initialized | 3m |
| `services` | all resource nodes | - | 5m |

`service-networks` is a first-class VPP owner (`vpp.dns-service.network` and
`vpp.proxy-service.network` moved out of `routes`). Its apply step:

1. creates the TAP/table/route,
2. verifies the NAT44 input role,
3. if absent, resolves the runtime underlay and re-attaches the role,
4. waits for the role readback,
5. fails with `DNS service interface "<if>" is missing its NAT44 input role on
   underlay "<underlay>"` instead of a generic snapshot error.

## Rollback

Reverse topological order covers every executed node:

`services` -> `port-maps` -> `nat44` -> `qos` -> `acls` -> `routes` ->
`service-networks` (NAT role/TAP) -> `pppoe` -> `wan-groups` -> `bonds` ->
`interfaces` (LCP/ABF then native attachment).

After cleanup, the prior generation is replayed forward. The interfaces restore
also recreates native attachments from the persisted assignment candidates
(the verified proof itself is stripped in snapshots), so `lyroute-*` exists
before interface state is replayed. This fixes
`interface "lyroute-eth2" was not returned` during rollback restore.

## Idempotency

- Resource adapters still diff desired/live and skip unchanged resources.
- `service-networks` detects an unchanged generation and performs a
  **readback-only** verification (`show` commands only), producing the same
  committed evidence without re-creating TAPs/NAT roles.
- Services skip unchanged artifacts via the service controller.
- A fully empty verified live snapshot (rollback/restart) is treated as a clean
  state to rebuild. A partially missing snapshot still fails closed before any
  mutation.

## Live validation (independent 209 instance)

Full `lab-desired-209.json` import + clean apply:

```text
status = committed
runtime_state = running
transaction = runtime-309ac17dc2d5be78ae96e244948f5be1
components: smartdns/kea/xray/pppoe/vpp/persistence all running
```

DNS service network and NAT44 role:

```text
show nat44 interfaces
  lyroute-eth2 in
  lyppp-8a258199 out
  lydnscdd4e1 in
show ip fib table 59377
  0.0.0.0/0 [@5]: ipv4 via 0.0.0.0 lyppp-8a258199
```

PPPoE:

```text
vppctl ping 100.64.0.1 source lyppp-8a258199 repeat 3
3 sent, 3 received, 0% packet loss
```

Re-apply after a failed generation (empty live state) rebuilt the graph and
committed:

```text
transaction runtime-6097d211fd26c2f765558d7a50c6b7e7  committed/running
```

Repeat apply with an unchanged generation committed without rebuilding:

```text
transaction runtime-4fc5f90df83aa0501d1eb9095b989eb2  committed/running
```

## Test coverage

- `apply_graph_test.go`: topological order, edge wait/timeout, missing node,
  cycle, reverse rollback, rollback continues after failure, node timeout,
  verify failure.
- `gateway_graph_hooks_test.go`: PPPoE -> service-networks -> routes ordering,
  NAT-role edge, services last, rollback order services -> networks -> PPPoE ->
  interfaces.
- `dns_service_network_roles_test.go`: missing NAT44 role repair and actionable
  fail-closed error.
- `supplemental_operations_test.go`: DNS service-network owner split.
- Existing apply/httpapi/service/vpp suites pass (`applygraph-tests.log`).

## Known P0-5 boundary

With the full fixture (including `pm-lab-http` port map), a repeat apply after a
PPPoE/session address change can still fail at the existing NAT return-guard
readback (`NAT return guard "pm-lab-http" pre-NAT bypass is missing`). That is
port-mapping/NAT return-guard reconvergence (P0-5), not graph ordering. Removing
the port map (test variant) and repeating apply commits idempotently; rollback
of the port-map failure completes and the next apply from empty live state
rebuilds and commits. P0-5 should own the guard rebuild on WAN address/session
change.

## Cherry-pick

```bash
git fetch /root/work/ly-route-gateway-applygraph feat/apply-graph-209
git cherry-pick <commit>
```

The branch contains one commit on top of `f739ffb`.
