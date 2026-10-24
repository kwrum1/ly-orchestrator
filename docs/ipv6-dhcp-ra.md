# IPv6 WAN, DHCPv6-PD, and LAN RA/DHCPv6

This document records the gateway IPv6 path after the P4 change. dnsmasq is the
only DHCPv4/DHCPv6/RA process in the gateway product; `radvd` is not rendered
and no systemd unit depends on it.

## Prefix sources

The desired `interface.ipv6` block selects one of three prefix sources:

1. **PPPoE DHCPv6-PD** (`mode: delegated_prefix`, `source_wan_id: <wan>`).
   The native PPPoE client (`ly-route-pppoe-client`) negotiates IPv6CP and
   performs DHCPv6-PD on the PPP session. It writes the lease to
   `/run/ly-route/pppoe/<wan>.json`:
   ```json
   {"state":"connected","interface":"pppoe_session_ly...",
    "session":{"delegated_prefix":"2001:db8:100::/56"}}
   ```
   The control API reads this file whenever it renders the runtime plan. This
   is the authoritative source for a live PD lease; a configured
   `wan_link.ipv6.delegated_prefix` is only a fallback for lab/static setups.
2. **Configured delegated prefix** (`wan_link.ipv6.delegated_prefix` or
   `.prefix`). Used when no live PD status exists.
3. **Static / same-interface IPv6** (`wan_link.ipv6.mode: static` with an
   address, or `interface.ipv6.prefix`/`address`). The /64 containing the
   static address is used for the LAN.

If none is available, no IPv6 lines are emitted. The dnsmasq drop-in still
contains the IPv4 configuration and `port=0`, so the service starts normally.

## Prefix allocation and dnsmasq binding

* Each LAN gets a distinct /64. A delegated /56 is sub-delegated by LAN index
  (LAN 0 gets `::/64`, LAN 1 gets `1::/64`, ...). A /64 can only serve one LAN.
  The same allocation is applied to VPP LAN addresses so VPP and dnsmasq agree.
* dnsmasq binds to the Linux LCP mirror (`lylan-*`), not the VPP-owned
  `lyroute-*` interface. RA and DHCPv6 multicast therefore enter the host
  control plane, where dnsmasq and SmartDNS live.
* `ra-stateless` is the default: clients build a global address from the RA
  prefix and use DHCPv6 for DNS/other options. Setting
  `interface.ipv6.dhcpv6: stateful` (or `address_mode: stateful`) emits a
  deterministic stateful pool (`<prefix>::100` - `<prefix>::1ff`).
* DNS is advertised with `dhcp-option=tag:<tag>,option6:dns-server,[...]`.
  The default is the LAN gateway address (`<prefix>::1`). SmartDNS remains the
  port 53 owner; the gateway image binds it to `0.0.0.0:53` and `[::]:53`, and
  dnsmasq keeps `port=0`.

Example rendered drop-in for one PPPoE-PD LAN and one static LAN:

```ini
port=0
dhcp-authoritative
log-dhcp
dhcp-leasefile=/var/lib/misc/dnsmasq.leases
enable-ra
interface=lylan-eth2
dhcp-range=set:lyroute-lan0,192.168.88.100,192.168.88.199,255.255.255.0,12h
dhcp-option=tag:lyroute-lan0,3,192.168.88.1
dhcp-option=tag:lyroute-lan0,6,192.168.88.1
dhcp-range=set:lyroute-lan0,2001:db8:100::,ra-stateless,64,12h
dhcp-option=tag:lyroute-lan0,option6:dns-server,[2001:db8:100::1]
ra-param=lylan-eth2,30,0
interface=lylan-eth3
dhcp-range=set:lyroute-lan1,2001:db8:50::,ra-stateless,64,12h
dhcp-option=tag:lyroute-lan1,option6:dns-server,[2001:db8:50::1]
ra-param=lylan-eth3,60,1800
```

The PD LAN sets RA router lifetime `0` because the VPP PPPoE dataplane still
advertises itself as the LAN default router; dnsmasq owns the prefix and
DHCPv6. The static LAN has no VPP RA owner, so dnsmasq advertises itself as
the default router (`1800`).

## Lifecycle

* A PPPoE connect or prefix change updates the status file and triggers the
  existing idempotent recovery transaction, so dnsmasq is re-rendered with the
  new /64.
* `currentDHCPServers` adds an IPv6-only dnsmasq plan for a LAN that has IPv6
  enabled but no DHCPv4 server. A LAN with both families shares one
  `interface=` and one tag.
* Invalid or too-small prefixes produce a runtime warning and are omitted;
  they never produce an invalid dnsmasq file.

## Gate B requirements

A real PPPoE fixture still has to prove:

1. IPv6CP and DHCPv6-PD negotiation, renew/rebind, and prefix change trigger a
   new dnsmasq configuration within the recovery deadline.
2. The LAN LCP mirror owns the delegated /64 (VPP `lcp-sync`) before dnsmasq
   starts; otherwise dnsmasq has no address in the prefix and sends no RA.
3. Client SLAAC address, default route, and DHCPv6 DNS are present for both
   stateless and stateful LANs.
4. The VPP ND RA and dnsmasq RA coexist as intended (VPP router lifetime,
   dnsmasq prefix lifetime 0). If the product requires literally one RA
   sender, suppressing VPP ND RA and moving forwarding to the Linux control
   plane must be validated, including return traffic.
5. Multiple LANs receive distinct /64s and a /64 PD cannot silently address
   more than one LAN.
