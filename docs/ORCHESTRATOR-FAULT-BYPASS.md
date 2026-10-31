# Orchestrator fault bypass

Fail-open guard for the transparent orchestrator. When a boundary/group
interface or a configured probe endpoint fails, the control plane flips a flag
inside the VPP plugin. The plugin then skips **all** policy evaluation
(including `default drop`) and forwards through its normal transparent
port-pair path. The committed generation, candidate policy and group topology
are left untouched, so clearing the flag restores the configured policy
without a re-apply.

## Detection

| Probe | Source | Notes |
| --- | --- | --- |
| Interface | `/sys/class/net/<if>/operstate` + `carrier` | Boundary and group ports are derived from the persisted topology; an explicit list overrides it. `operstate=down` or `carrier=0` fails. |
| TCP | `net.DialTimeout("tcp", host:port, timeout)` | Default timeout 1 s. Targets must be reachable from the control-plane kernel (management network or a host route); the probe is not injected through VPP. |
| ICMP (optional) | `ping -n -c 1 -W <s>` | Only when `icmp_targets` is configured and the binary is present. |

Defaults: interval 2 s, timeout 1000 ms, 3 consecutive failures enter bypass,
3 consecutive successes leave it. A manual force suppresses automatic
recovery until an operator clears it.

## API

```text
GET  /api/v1/orchestrator/bypass           # state, probes, config, generation
POST /api/v1/orchestrator/bypass           # {"enabled":true|false,"reason":"..."}
PUT  /api/v1/orchestrator/bypass/config    # detection config (persisted)
```

`GET` returns `enabled`, `forced`, `cause`, `reason`, `since`, consecutive
failure/success counters, the last probe rows, the committed generation and the
plugin `bypass_packets` counter. `/health` adds
`orchestrator_runtime.bypass` / `bypass_reason` and a reason string such as
`fault bypass active (auto): probe_failed_3/3:interface:ens161_operstate_down`.

State and configuration are persisted to
`/var/lib/ly-route/orchestrator/bypass.json` (mode 0600) and a forced bypass is
re-asserted after a control-API restart. Environment defaults:
`LY_ROUTE_ORCHESTRATOR_BYPASS_TCP_TARGETS`,
`LY_ROUTE_ORCHESTRATOR_BYPASS_ICMP_TARGETS`,
`LY_ROUTE_ORCHESTRATOR_BYPASS_INTERFACES`.

## VPP CLI

```text
set ly-route orchestrator bypass on|off [cause <token>] [reason <token>]
show ly-route orchestrator
```

`show` reports `bypass state on|off`, `bypass cause`, `bypass reason`,
`bypass forced` and `bypass packets`; the `ly-route-orchestrator` node also
counts `packets forwarded by fault bypass`.

## Limitations

- A bypass cannot repair the physical path: if the port carrying the business
  flow is itself down, direct forwarding through that same port still fails.
  The lab validation therefore faults an idle group port and keeps the working
  pair carrying traffic.
- TCP/ICMP probes run in the host kernel namespace, not inside VPP. A target
  such as `192.168.10.82:80` needs a kernel route/source address on that
  segment; otherwise use a management-reachable address.
- Bypass counters are per committed generation and reset when a new policy is
  committed.
- Interface probes observe the kernel netdev. In AF_PACKET acceptance mode VPP
  may still show the port up after `ip link set <if> down`; the detector uses
  sysfs, which is the requested signal.
