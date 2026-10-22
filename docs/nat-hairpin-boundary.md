# NAT hairpin: VPP 26.06 boundary

Status: **unsupported / fail closed** for the current gateway dataplane.

## What was tested

Clean VPP 26.06 / AF_XDP gateway, LAN client on `192.168.88.0/24`, internal
HTTP service `192.168.88.50:8080`, NAT44-ED.

Tested external addresses:

- WAN interface address `100.64.0.x`
- independent NAT-pool/VIP addresses `100.64.0.108`, `198.51.100.50`

Tested mapping variants:

- plain static mapping + `nat44 add address`
- `self-twice-nat`
- `twice-nat`
- `set interface nat44 in lylan out wan`
- separate LAN/client namespaces (client and server are not the same host)

WAN-side access to the external address works. LAN-side hairpin does not create
a usable NAT44 session / the translated response is not returned to the LAN
client. This also fails with an explicit independent VIP, so it is not solely
the "WAN's own address is local" case.

## Product behavior

`port_map` with `"hairpin": true` is rejected by NAT compilation with a typed
error. It is not silently accepted and is not reported as `applied`.

Use one of these alternatives:

1. route the LAN source network to the internal service directly;
2. expose the service on a dedicated routed VIP without hairpin and ensure the
   client network can resolve/route that VIP;
3. implement a VPP-native hairpin/twice-NAT path in a future dataplane release
   and add a real two-namespace acceptance test before enabling the field.

The explicit `external_address`/VIP path remains available for ordinary WAN
port forwarding. It does not imply hairpin support.

## Evidence

- `vppctl show nat44 static mappings` contains the mapping and external VIP.
- `vppctl show nat44 addresses` contains the VIP.
- LAN client `curl http://<vip>:18080/` times out.
- WAN client `curl http://<vip>:18080/` returns HTTP 200.
