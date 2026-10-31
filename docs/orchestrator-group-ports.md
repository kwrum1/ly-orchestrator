# Orchestration group ports as real entry/exit ports

## LAN/WAN port tags (`port_roles`)

`PUT /api/v1/orchestrator/topology` accepts the frozen console's
`port_roles: { "<iface>": "lan" | "wan" }` map alongside `interfaces` and
`orchestration_groups`.  It used to be rejected by the strict decoder
(`unknown field "port_roles"`).

- The map is persisted with the topology document and echoed by `GET`.
- Tags are operator metadata: they do not change logical LAN/WAN ownership
  (`interfaces[].role/port`).  The console uses them as the side hint when
  selecting a group port, and the 网卡设置 page shows them.
- Values are restricted to `lan`/`wan`; an unknown value is a 422
  `invalid_topology`.
- Config export/import (`/api/v1/config/...`) carries the same field.

## Problem

The transparent orchestrator only routed traffic that entered through the LAN
boundary.  A client attached to a group port (`grp-a/ens160`, `grp-b/ens193`)
had its policy evaluated but the frame was dropped or left through an unusable
port:

- `packets for another host ignored` (unicast frames for the group peer)
- `IP packets forwarded without an egress interface`
- client `curl` always `000`, for `direct`, `via` and `drop`

## Data-path contract (implementation)

VPP plugin `runtime/vpp-orchestrator/ly_route_orchestrator/`:

1. **Sides.** LAN boundary and every group `lan_facing` port face clients; WAN
   boundary and every group `wan_facing` port face servers.  Policy rules are
   authored client-to-server, so only server-side ingress has its tuple
   reversed before matching.  Group ports are no longer treated as an opaque
   "service return" side (that inverted port matching on group entry).
2. **Egress selection** (`ly_orch_target_next`):
   - policy path (`via`) wins: client-to-server leaves the `wan_facing` port of
     the last group in the path, server-to-client leaves the `lan_facing` port
     of the first group;
   - otherwise the learned destination address decides the port, so a client
     attached to `grp-b/ens193` is answered through `ens193`;
   - otherwise a group port forwards out its opposite port (transparent pair),
     and a boundary port forwards to the other boundary.
3. **L2 next hop.** Every forwarded frame has its source rewritten to the
   egress port MAC.  The destination is the learned address-to-MAC on that
   egress, else a configured static next hop
   (`wan-next-hop-mac <mac>` / `lan-next-hop-mac <mac>` on the boundary or
   group CLI command), else the frame is flooded as broadcast (unknown-unicast
   flooding, counted) so the host answers and the table learns its MAC.  Frames
   are never forwarded with an unusable destination MAC.
4. **Opposing ports on one segment.** Two group ports can share an L2 segment
   (lab: `grp-b/ens161` and `grp-a/ens160` on vlan10).  Frames whose source MAC
   is one of our own ports are dropped (`own transmitted frames ignored (loop
   breaker)`), which prevents a line-rate forwarding loop between sibling
   ports.
5. **Telemetry.** Policy counters are incremented for group-port ingress too.
   New VPP error counters: `packets forwarded without an egress next-hop MAC`,
   `unicast packets flooded to learn the next-hop MAC`, `own transmitted frames
   ignored (loop breaker)`.

## CLI additions

```text
set ly-route orchestrator candidate group grp-a wan-facing ens224 lan-facing ens160 \
    wan-next-hop-mac 00:50:56:3f:12:84 lan-next-hop-mac 00:50:56:3f:12:82
set ly-route orchestrator candidate boundary wan ens256 lan ens192 \
    wan-next-hop-mac <mac> lan-next-hop-mac <mac>
```

Static next hops are optional; learned entries work without them and are the
default.  The topology API is unchanged: the group's own port pair provides the
peer relation, so no extra "peer/NH" field is required.

## Acceptance (VM 10.1.18.234, real clients)

Fixture: A `192.168.10.10` attached to `grp-a/ens160` (static ARP to
`00:50:56:3f:12:82`), B `192.168.20.10` attached to `grp-b/ens193` (static ARP
to `00:50:56:3f:12:87`), server `192.168.10.82/192.168.20.82` on lab-wan
(static ARP to `ens224`/`ens256`).

| Case | Result |
|---|---|
| A → server:80 `direct` (client on `grp-a/ens160`) | `200` in every round, `policy ga packets > 0` |
| B → server:80 `direct` (client on `grp-b/ens193`) | `200` once the server address is learned (`policy gb packets > 0`); first contact can be lost because the unknown destination leaves the group's own wan port (`ens161`, vlan10) |
| A rule `drop` | `000`, drops counter grows |
| B rule `direct` (same policy) | `200` |
| policy groups `[allow, block]` | `200` |
| policy groups `[block, allow]` (swap) | `000` |
| LAN boundary B (`ens192`) after restore | `200` (unchanged) |
| control API restart | config re-applied, traffic keeps working |

Known limitation: a client behind `grp-b` (whose wan-facing port is a vlan10
segment, not the server segment) relies on the learned address table or the
policy `via` path to reach a server on lab-wan.  Deterministic first contact
needs either a `wan-next-hop-mac` on `grp-b` that points at the peer group port
or a `via` rule that names the server-side group.  The LAN boundary and
`grp-a` paths are deterministic because their far side is the server segment.

Flow entries are recorded for both directions of each client, with a
`192.168.10.82 -> 192.168.10.10` / `192.168.20.82 -> 192.168.20.10` return flow.
