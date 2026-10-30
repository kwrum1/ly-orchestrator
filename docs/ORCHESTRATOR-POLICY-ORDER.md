# Orchestrator policy ordering contract

The orchestrator policy collection is ordered. There is exactly one source of
truth for that order: **the position of an entry in its JSON array**.

## Write contract

`PUT /api/v1/orchestrator/policy` takes the complete desired policy:

```json
{
  "schema_version": 1,
  "ip_objects": [],
  "policy_groups": [
    {
      "id": "first",
      "rules": [
        { "id": "first-allow", "match": { "sources": ["any"], "destinations": ["any"], "protocol": "any" }, "action": { "kind": "direct" } },
        { "id": "first-block", "match": { "sources": ["any"], "destinations": ["any"], "protocol": "any" }, "action": { "kind": "drop" } }
      ]
    },
    {
      "id": "second",
      "rules": [
        { "id": "second-allow", "match": { "sources": ["any"], "destinations": ["any"], "protocol": "any" }, "action": { "kind": "direct" } }
      ]
    }
  ],
  "default": { "kind": "direct" }
}
```

* Group order is the order of `policy_groups`.
* Rule order inside a group is the order of that group's `rules`.
* First matching rule wins inside a group; groups are evaluated from first to
  last, so the conflict pair above flips from `direct` to `drop` when the two
  rule entries are swapped.
* `position` (group) and `sequence` (rule) may still be present on the wire for
  compatibility with older clients. **They are ignored.** The server derives
  them from the array index as `10, 20, 30, ...` and writes those derived values
  into the persisted document and the readback. Clients must not use them to
  express order.
* The UI strips `position`/`sequence` from every PUT and never displays them.

## Read contract

`GET /api/v1/orchestrator/policy` returns the same arrays in persisted execution
order. `position`/`sequence` in the response are server-derived, read-only
diagnostics. A client that round-trips the response unchanged is safe because
the server ignores the numbers and re-derives them from the arrays.

## Determinism and apply

`ParsePolicy` normalizes the collection order into the persisted ordinals, and
`CompilePolicy` / the transparent VPP command builder sort by those persisted
ordinals with an ID tie-breaker. No map iteration, database row order or
document creation timestamp participates in the plan.

The apply no-op gate hashes the persisted policy document. Because the arrays
and the derived ordinals change together, an order-only edit produces a new
desired source hash. `/api/v1/runtime/apply` therefore cannot reuse the previous
transaction and must render/apply the new generation.

## VM verification

Use the same topology as the running VM. The two payloads below differ only in
array order; the first should compile to `direct`, the second to `drop` for a
flow matching both rules.

```bash
BASE=https://127.0.0.1
# replace with the admin cookie used by the appliance UI
COOKIE='session=...'

curl -sk -X PUT "$BASE/api/v1/orchestrator/policy" \
  -H "Content-Type: application/json" -H "Cookie: $COOKIE" \
  -d '{"schema_version":1,"ip_objects":[],"policy_groups":[{"id":"conflict","rules":[{"id":"allow","match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"direct"}},{"id":"block","match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"drop"}}]}],"default":{"kind":"direct"}}'

curl -sk -X POST "$BASE/api/v1/orchestrator/policy/compile" \
  -H "Content-Type: application/json" -H "Cookie: $COOKIE" \
  -d '{"flow":{"source_ip":"192.0.2.10","destination_ip":"198.51.100.10","protocol":"tcp","source_port":50000,"destination_port":443},"prelude":{}}'

# expect: "exit":"lan", first decision rule_id "allow"

curl -sk -X PUT "$BASE/api/v1/orchestrator/policy" \
  -H "Content-Type: application/json" -H "Cookie: $COOKIE" \
  -d '{"schema_version":1,"ip_objects":[],"policy_groups":[{"id":"conflict","rules":[{"id":"block","match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"drop"}},{"id":"allow","match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"direct"}}]}],"default":{"kind":"direct"}}'

curl -sk -X POST "$BASE/api/v1/orchestrator/policy/compile" \
  -H "Content-Type: application/json" -H "Cookie: $COOKIE" \
  -d '{"flow":{"source_ip":"192.0.2.10","destination_ip":"198.51.100.10","protocol":"tcp","source_port":50000,"destination_port":443},"prelude":{}}'

# expect: "exit":"drop", first decision rule_id "block"

# The order-only edit must not be reported as an unchanged apply.
curl -sk -X POST "$BASE/api/v1/runtime/apply" \
  -H "Content-Type: application/json" -H "Cookie: $COOKIE" -d '{}'
# expect: no "unchanged":true / "noop":true for the second PUT
```

Group-level verification uses the same payload shape with the two groups
swapped; `GET /api/v1/orchestrator/policy` must return the groups in the order
that was submitted.
