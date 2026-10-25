# Verified dataplane fallback transcript

## ESXi Gateway VM `10.1.18.230` (vmxnet3, no native VPP/DPDK path)

The consent gate was verified end to end against the real backend. The raw
transcript is stored in `/root/work/esxi124-accept/evidence/uio-20261025/`.

```text
GET  /api/v1/dataplane/policy
200  {"af_packet_fallback":false,"ports":[]}

POST /api/v1/runtime/apply
409  {"status":"af_fallback_required","runtime_state":"degraded",
      "error":{"code":"af_fallback_required","ports":["eth1","eth2"],
               "message":"AF_PACKET 兼容模式需要操作员明确确认：该模式仍由 VPP 转发，但性能受限。请在数据面策略中允许这些端口使用 AF_PACKET 兼容模式后重试。"}}

PUT  /api/v1/dataplane/policy
     {"af_packet_fallback":true,"ports":["eth1","eth2"]}
200  {"af_packet_fallback":true,"ports":["eth1","eth2"]}

POST /api/v1/runtime/apply
202  status=degraded runtime_state=degraded
     component af_packet_fallback=degraded reason="AF_PACKET 兼容模式（性能受限）"

PUT  /api/v1/dataplane/policy
     {"af_packet_fallback":false,"ports":[]}
POST /api/v1/runtime/apply
409  error.code=af_fallback_required, ports=["eth1","eth2"]
```

With consent enabled, `vppctl show interface` showed `lyroute-ens224` and
`lyroute-ens256` up, and the LAN client `10.1.18.232` reached the WAN peer
`100.77.0.1` over ICMP (2/3 replies; the first probe packet was dropped during
VPP feature warm-up). Traffic still traverses VPP; AF_PACKET is not a kernel
bypass.

## J1900 native UIO takeover

Before the final disk image was written, the same backend/probe/preflight code
was hot-deployed to the J1900 and produced:

- `GigabitEthernet3/0/0` (0000:03:00.0, `enp3s0`) and
  `GigabitEthernet4/0/0` (0000:04:00.0, `enp4s0`) in VPP with counters;
- `transport:"uio"`, `mode:"uio_pci_generic"`, `high_performance:true`,
  `acceptance_only:false`, `kernel_driver:"igb"`, `iommu_group:"none"` in
  `/var/lib/ly-route/vpp-native-capabilities.json`;
- `POST /api/v1/runtime/apply` reached `committed`/`running`;
- `enp3s0`/`enp4s0` no longer had an `igb` driver (VPP owned them) while
  `enp1s0` stayed on `igb` and reachable.

The captured hot-run files are in `/root/work/j1900-evidence/uio-20261025/`.

The final image write failed: the target root filesystem had only 2.2 GiB free,
the 4 GiB transfer stopped early, and the box was rebooted with a partial image.
The J1900 is therefore not currently reachable over SSH. Both the restore image
`/root/work/j1900-gw-d8269d6.img` and the new image
`/root/work/j1900-uio-7ffa48d.img` are retained for a physical restore.
