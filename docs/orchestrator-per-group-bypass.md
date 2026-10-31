# Per-group fault bypass with passive flow analysis

## Motivation

A security appliance / firewall in front of a service can start dropping or
resetting traffic even though the routing and the policy configuration are
intact. DNS queries that leave the client but never receive an answer were one
production incident of this class, but the problem is generic: a web/API
endpoint, a database port or any request/response service can be blocked the
same way. The orchestrator must notice "requests are leaving, nothing comes
back" for **a specific policy group** and fail open only that group, while every
other group keeps enforcing its configured policy.

This feature can be used for any business flow; DNS is only one example.

## Semantics

### Per-group, independent bypass

* Each topology group (`grp-a`, `grp-b`, ...) has its own detector and its own
  bypass flag in the plugin. One group can bypass while the other keeps
  policy-based forwarding, and either group can trigger first.
* A bypassed group forwards like a transparent port pair and skips **its own**
  policy evaluation (including a `drop` rule defined in that group). Other
  groups are untouched.
* The committed policy generation is never changed by a bypass; clearing the
  flag restores the configured policy without a re-apply.
* The legacy global flag (`set ly-route orchestrator bypass on|off`) still
  works and, when active, bypasses every group.

### Passive analysis: no traffic is healthy

For each configured passive target (`dst_ip`, `dst_port`, `protocol`) the plugin
watches both directions on the group's ports:

| Direction | Counter |
|---|---|
| client → `dst_ip:dst_port` | `attempts` (TCP: SYN without ACK; UDP: every datagram) |
| server → client (swapped tuple) TCP SYN-ACK | `synack`, counts as success |
| server → client other non-RST packet | `responses`, counts as success |
| server → client RST | `rst`, failure evidence |
| ICMP destination-unreachable quoting the request (IPv4) | `icmp_unreachable`, failure evidence |

Decision rules (identical for TCP and UDP):

* `attempts == 0` in the window → **health `unknown`**, never bypass. Absence of
  business traffic must not move the service to another path.
* `attempts > 0` and `success == 0` after a full window → **fault**, bypass the
  group. This covers zero replies, "RST only" and ICMP unreachable.
* `success >= recovery_threshold` inside the window → **recovered**, clear the
  group bypass and restore policy.

For UDP the target must be a **request/response service** (DNS, NTP, QUIC,
custom echo, ...). A one-way UDP stream (syslog, metrics push, video) has no
reverse packet by design and must not be configured as a probe target,
otherwise it would be misclassified as a fault.

Any answer counts as success for a request/response service, including a DNS
error answer (`NXDOMAIN`, `SERVFAIL`) or an application-level error page: the
service replied, so the group is healthy. Only zero replies, RST or ICMP
unreachable trigger a bypass.

### Window and thresholds

The control plane reads the plugin's cumulative counters every
`interval_seconds` (default 2 s) and keeps a trailing sample history:

* `window_seconds` (default 10 s) is the trailing observation window per group.
* The window counters are `current - oldest sample inside the window`.
* A passive verdict is **conclusive only after a full window** of observation
  (`window_ready`). This prevents a single lost first contact from switching
  the path.
* `failure_threshold` applies to consecutive failed interface/TCP rounds. A
  passive window with requests and zero success is conclusive on its own and
  enters bypass when the window closes.
* `recovery_threshold` (default 3) is the number of successful responses that
  must appear in the window before the group bypass is cleared.

`timeout = attempts - success - rst` is exposed for diagnostics; `rst` is
"answered with a refusal", not a timeout.

## Configuration

`GET/PUT /api/v1/orchestrator/bypass/config`

```json
{
  "enabled": true,
  "interval_seconds": 2,
  "timeout_ms": 1000,
  "failure_threshold": 3,
  "recovery_threshold": 3,
  "groups": [
    {
      "name": "grp-a",
      "window_seconds": 8,
      "failure_threshold": 2,
      "recovery_threshold": 2,
      "interfaces": ["ens160", "ens224"],
      "flow_targets": [
        {"dst_ip": "10.0.0.10", "dst_port": 443, "protocol": "tcp"}
      ],
      "tcp_targets": [
        {"target": "10.0.0.10:443", "source": "192.168.10.10"}
      ]
    },
    {
      "name": "grp-b",
      "window_seconds": 8,
      "flow_targets": [
        {"dst_ip": "10.0.0.20", "dst_port": 3306, "protocol": "tcp"},
        {"dst_ip": "10.0.0.53", "dst_port": 53, "protocol": "udp"},
        {"dst_ip": "10.0.0.123", "dst_port": 123, "protocol": "udp"}
      ]
    }
  ]
}
```

* `interfaces` is optional; when empty the group's topology ports are used.
* `tcp_targets` is optional and active. A host-side TCP connect needs a
  reachable source address/route. When `source` cannot be bound the probe is
  **inconclusive** (not a failure), because data-plane group ports are not host
  interfaces. Without a usable source, use passive `flow_targets`.
* `flow_targets` is fully generic: any TCP/UDP destination and port, any number
  of targets per group. No DNS-specific logic exists in the data plane.

### Correct / wrong target examples

```json
// Web / API (TCP)
{"dst_ip": "10.0.0.10", "dst_port": 443, "protocol": "tcp"}
// Database (TCP)
{"dst_ip": "10.0.0.20", "dst_port": 3306, "protocol": "tcp"}
// Custom TCP service on any port
{"dst_ip": "10.0.0.30", "dst_port": 8443, "protocol": "tcp"}
// UDP request/response: DNS is one example
{"dst_ip": "10.0.0.53", "dst_port": 53, "protocol": "udp"}
// UDP request/response: NTP
{"dst_ip": "10.0.0.123", "dst_port": 123, "protocol": "udp"}
// UDP request/response: QUIC/HTTP3
{"dst_ip": "10.0.0.10", "dst_port": 443, "protocol": "udp"}
```

Wrong (one-way UDP; will be misclassified as faulty):

```json
{"dst_ip": "10.0.0.40", "dst_port": 514, "protocol": "udp"}   // syslog
{"dst_ip": "10.0.0.41", "dst_port": 8125, "protocol": "udp"}  // metrics push
```

## API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/orchestrator/bypass` | global + per-group state, window counters, probe samples |
| `POST` | `/api/v1/orchestrator/bypass` | `{"group":"grp-b","enabled":true}` or global `{"enabled":true}` |
| `GET` | `/api/v1/orchestrator/bypass/config` | active configuration |
| `PUT` | `/api/v1/orchestrator/bypass/config` | replace configuration |

`GET /api/v1/orchestrator/bypass` returns the aggregate `enabled`/`reason` plus
`groups[]` with `health` (`unknown` / `healthy` / `faulty` / `bypass`),
`flow_attempts`, `flow_success`, `flow_rst`, `flow_timeouts`, per-target
cumulative counters and the plugin `bypass_packets`.

`/api/v1/health` keeps the appliance `status: ok` when a single group bypasses.
The group list and reason are shown in the `orchestrator_runtime` dependency:

```
"reason": "policy group fault bypass active: grp-b (other groups keep policy enforcement)"
```

## VPP plugin

Runtime CLI (not part of the committed generation):

```text
set ly-route orchestrator bypass group grp-b on cause auto reason flow_no_response
set ly-route orchestrator bypass group grp-b off cause auto reason recovered
set ly-route orchestrator probe clear
set ly-route orchestrator probe group grp-a dst 192.168.10.82 port 80 proto tcp window 8
set ly-route orchestrator probe group grp-b dst 192.168.10.82 port 53 proto udp window 8
```

`show ly-route orchestrator` adds:

```text
group-bypass grp-b state on forced no cause auto packets 30 reason flow_no_response
probe-targets 2
group-probe grp-a dst 192.168.10.82 port 80 proto tcp attempts 14 syn 14 synack 0 rst 14 responses 0 icmp-unreachable 0 window 8
```

Node error counters: `packets forwarded by fault bypass` (global) and
`packets forwarded by policy-group fault bypass` (per-group). Per-group bypass
packets are also reported on every `group-bypass`/`group-health` row.

## Acceptance on VM 10.1.18.234

Fixture: client-a `192.168.10.10` on `grp-a/ens160`, client-b `192.168.20.10`
on `grp-b/ens193`, server `192.168.10.82/192.168.20.82`.
Policy: grp-a drops TCP/8080 and forwards TCP/80 by default; grp-b routes
TCP/80 `via grp-b` and drops TCP/8080.
Passive targets: grp-a `192.168.10.82:80/tcp`, window 8 s.

| # | Fault | Expected | Observed |
|---|---|---|---|
| 0 | none | both groups off | `grp-a off grp-b off`; client-a:80 `200`, :8080 `000` |
| 1 | grp-b interface `ens161` down | only grp-b bypass | `grp-b on(bypass)` / `grp-a off` |
| 2 | grp-b interface up again | only grp-b recovers | `grp-b off(healthy)` / `grp-a off` |
| 3 | server:80 stops answering (RST), client-a traffic | only grp-a bypass after window | `grp-a on(bypass)` / `grp-b off(healthy)`; plugin `attempts 14 syn 14 rst 14 success 0`; reason `flow_no_response_4/4` |
| 4 | both faults together | both bypass | `grp-a on` / `grp-b on` |
| 5 | recover grp-b only | grp-b off, grp-a still bypassed | `grp-b off(healthy)` / `grp-a on(bypass)` |
| 6 | server:80 restarted | grp-a recovers | `grp-a off(healthy)`; window success 42; plugin `synack 14 responses 70`; reason `probe_recovered_11/2` |
| 7 | force grp-b with API | /health not degraded for one group | `/health status ok`, dependency `orchestrator_runtime available`, reason lists `grp-b` |

## Limits

* **Active TCP probes** are optional. They run from the host network stack, so
  they need a reachable source address and route. A group port is a data-plane
  port, not a host interface; without a usable source the probe is reported
  inconclusive instead of bypassing.
* **Passive precision.** TCP attempts count SYNs; UDP attempts count datagrams.
  Counters are cumulative per target and aggregated across targets for the
  window, so retries and multiple flows are visible but not individually
  attributable. ICMP unreachable inspection is IPv4-only today.
* **Intentional drops.** Do not point `flow_targets` at a service the policy is
  supposed to drop: the drop looks like "no response" and the group will
  correctly (but undesirably) fail open. Configure probes only for flows the
  policy is expected to forward.
* **Bypass scope.** A bypassed group skips its own rules; other groups keep
  theirs. Whether direct forwarding can reach the service depends on the
  physical alternative path. In the acceptance fixture the grp-b WAN leg
  (`ens161`, vlan10) does not reach the server after a cold start, so client-b
  HTTP could not be used as the positive business check; the per-group state
  machine, passive RST detection and recovery were verified on grp-a, and
  grp-b was verified through the interface probe path.
