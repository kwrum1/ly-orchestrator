#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
cleanup() {
  if [ "${LY_ROUTE_KEEP_TEST_TMP:-0}" = 1 ]; then
    printf 'kept test fixture: %s\n' "$tmp" >&2
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT INT TERM

mkdir -p "$tmp/bin"
cat > "$tmp/bin/systemctl" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
cat > "$tmp/bin/ip" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
cat > "$tmp/bin/vppctl" <<'EOF'
#!/usr/bin/env sh
printf '%s\n' "$*" >> "$VPPCTL_LOG"
case "$*" in
  'show plugin') printf '%s\n' "$VPPCTL_PLUGINS" ;;
  'show cli') printf '%s\n' 'show dpdk interface' ;;
  'show hardware-interfaces')
    printf '%s 1 up %s\n  %s\n    pci: device 8086:1539 address %s numa 0\n    carrier %s\n' \
      "${VPPCTL_VPP_INTERFACE:-GigabitEthernet3/0/0}" "${VPPCTL_VPP_INTERFACE:-GigabitEthernet3/0/0}" \
      "${VPPCTL_DRIVER_MODEL:-Intel e1000}" "${VPPCTL_ACTIVE_PCI:-}" "${VPPCTL_HARDWARE_LINK:-up}"
    ;;
  show\ hardware-interfaces\ *)
    printf '%s PCI %s\n  %s\n    carrier %s\n' "$3" "${VPPCTL_ACTIVE_PCI:-}" "${VPPCTL_DRIVER_MODEL:-Intel e1000}" "${VPPCTL_HARDWARE_LINK:-up}" ;;
  'show threads') printf '%s\n' "${VPPCTL_THREADS:-vpp_main}" ;;
  'show dpdk interface hqos placement') printf '%s\n' "${VPPCTL_HQOS_PLACEMENT:-}" ;;
  create\ interface\ af_xdp*)
    case "$*" in
      *' zero-copy'*) [ "${VPPCTL_AFXDP_SUCCESS:-0}" = 1 ] ;;
      *' no-zero-copy'*) [ "${VPPCTL_AFXDP_SUCCESS:-0}" = 1 ] ;;
      *) exit 1 ;;
    esac
    ;;
  create\ interface\ rdma*) [ "${VPPCTL_RDMA_SUCCESS:-0}" = 1 ] ;;
  show\ interface\ *) printf '%s %s\n' "$3" "${VPPCTL_LINK_STATE:-up}" ;;
  *) exit 0 ;;
esac
EOF
cat > "$tmp/bin/native-benchmark" <<'EOF'
#!/usr/bin/env sh
case "$2" in
  rdma) printf '%s\n' "${VPPCTL_RDMA_SCORE:-90}" ;;
  af_xdp) printf '%s\n' "${VPPCTL_AFXDP_SCORE:-80}" ;;
  af_packet) printf '%s\n' 10 ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$tmp/bin/systemctl" "$tmp/bin/ip" "$tmp/bin/vppctl" "$tmp/bin/native-benchmark"

run_check() {
  scenario=$1
  management=$2
  data_interfaces=$3
  plugins=$4
  afxdp_success=$5
  management_shared=${6:-false}
  scenario_dir="$tmp/$scenario"
  mkdir -p "$scenario_dir" "$scenario_dir/sys"
  for interface in $(printf '%s' "$data_interfaces" | tr ',' ' '); do
    if [ -z "${VPPCTL_ACTIVE_PCI:-}" ]; then
      mkdir -p "$scenario_dir/sys/class/net/$interface"
    fi
  done
  PATH="$tmp/bin:$PATH" \
    VPPCTL_LOG="$scenario_dir/vppctl.log" \
    VPPCTL_PLUGINS="$plugins" \
    VPPCTL_AFXDP_SUCCESS="$afxdp_success" \
    VPPCTL_RDMA_SUCCESS="${VPPCTL_RDMA_SUCCESS:-0}" \
    LY_ROUTE_VPP_NATIVE_BENCHMARK="$tmp/bin/native-benchmark" \
    LY_ROUTE_RUNTIME_READINESS="$scenario_dir/readiness.json" \
    LY_ROUTE_VPP_CAPABILITY_PROOF="$scenario_dir/proof.json" \
    LY_ROUTE_DATAPLANE_ACTIVE_STATE="$scenario_dir/active.json" \
    LY_ROUTE_ACTIVE_DPDK_READER="$repo_root/packaging/rootfs-overlay/usr/lib/ly-route/active-dpdk-state.py" \
    LY_ROUTE_DATAPLANE_INVENTORY="$scenario_dir/inventory.json" \
    LY_ROUTE_DPDK_OWNERSHIP_STATE="$scenario_dir/ownership.json" \
    LY_ROUTE_DPDK_VERIFICATION_STATE="$scenario_dir/dpdk-verification.json" \
    LY_ROUTE_COMMERCIAL_RUNTIME_REQUIRED=false \
    LY_ROUTE_PRODUCT=test \
    LY_ROUTE_REQUIRED_COMMANDS=sh \
    LY_ROUTE_REQUIRED_UNITS= \
    LY_ROUTE_MANAGEMENT_INTERFACE="$management" \
    LY_ROUTE_MANAGEMENT_SHARED="$management_shared" \
    LY_ROUTE_VPP_DATA_INTERFACES="$data_interfaces" \
    LY_ROUTE_SYSFS_ROOT="$scenario_dir/sys" \
    "$repo_root/packaging/rootfs-overlay/usr/lib/ly-route/runtime-check.sh"
}

setup_dpdk_fixture() {
  scenario=$1
  interface_name=$2
  scenario_dir="$tmp/$scenario"
  device="$scenario_dir/sys/devices/pci0000:00/0000:03:00.0"
  driver="$scenario_dir/sys/bus/pci/drivers/vfio-pci"
  iommu="$scenario_dir/sys/kernel/iommu_groups/17"
  mkdir -p "$scenario_dir/sys/class/net/$interface_name" "$device" "$driver" "$iommu" \
    "$scenario_dir/sys/module/vfio_pci" "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB" \
    "$scenario_dir/sys/bus/pci/devices"
  ln -s "$device" "$scenario_dir/sys/class/net/$interface_name/device"
  ln -s "$device" "$scenario_dir/sys/bus/pci/devices/0000:03:00.0"
  ln -s "$driver" "$device/driver"
  ln -s "$iommu" "$device/iommu_group"
  printf '%s\n' 64 > "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
  cat > "$scenario_dir/inventory.json" <<EOF
{"schema_version":1,"source":"test","management":{"linux_interface":"eth0","pci":"0000:01:00.0"},"data_interfaces":[{"linux_interface":"$interface_name","pci":"0000:03:00.0","mac":"00:11:22:33:44:55","kernel_driver":"ixgbe","role":"data"}]}
EOF
  chmod 0600 "$scenario_dir/inventory.json"
}

setup_active_dpdk_fixture() {
  scenario=$1
  interface_name=$2
  scenario_dir="$tmp/$scenario"
  device="$scenario_dir/sys/devices/pci0000:00/0000:03:00.0"
  driver="$scenario_dir/sys/bus/pci/drivers/vfio-pci"
  iommu="$scenario_dir/sys/kernel/iommu_groups/17"
  mkdir -p "$device" "$driver" "$iommu" "$scenario_dir/sys/bus/pci/devices" \
    "$scenario_dir/sys/module/vfio_pci" "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB"
  ln -s "$device" "$scenario_dir/sys/bus/pci/devices/0000:03:00.0"
  ln -s "$driver" "$device/driver"
  ln -s "$iommu" "$device/iommu_group"
  printf '%s\n' 64 > "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
  cat > "$scenario_dir/active.json" <<EOF
{"path":{"tier":"vpp_dpdk","smart_qos":true,"attachments":[{"linux_interface":"$interface_name","vpp_interface":"TenGigabitEthernet3/0/0","tier":"vpp_dpdk","hook":"dpdk","mode":"vfio_pci","pci_address":"0000:03:00.0","kernel_driver":"ixgbe","iommu_group":"17"}],"prerequisites":[]},"snapshot":{"transaction_id":"active-test","startup_config":"","devices":[]},"applied_at":"2026-08-01T00:00:00Z"}
EOF
  chmod 0600 "$scenario_dir/active.json"
}

setup_inventory_uio_fixture() {
  scenario=$1
  interface_name=$2
  kernel_driver=${3:-igb}
  scenario_dir="$tmp/$scenario"
  device="$scenario_dir/sys/devices/pci0000:00/0000:03:00.0"
  driver="$scenario_dir/sys/bus/pci/drivers/uio_pci_generic"
  mkdir -p "$device" "$driver" "$scenario_dir/sys/bus/pci/devices" \
    "$scenario_dir/sys/module/uio_pci_generic" "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB"
  ln -s "$device" "$scenario_dir/sys/bus/pci/devices/0000:03:00.0"
  ln -s "$driver" "$device/driver"
  printf '%s\n' 64 > "$scenario_dir/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
  cat > "$scenario_dir/inventory.json" <<EOF
{"schema_version":1,"source":"test","management":{"linux_interface":"eth0","pci":"0000:01:00.0"},"data_interfaces":[{"linux_interface":"$interface_name","pci":"0000:03:00.0","mac":"00:11:22:33:44:55","kernel_driver":"$kernel_driver","role":"data"}]}
EOF
  chmod 0600 "$scenario_dir/inventory.json"
}

run_check native eth0 eth1 af_xdp_plugin.so 1
grep -q '"dataplane_state": "native_ready"' "$tmp/native/readiness.json"
# arch-v1 first boot: optional services (SmartDNS/Kea/xray) are reported as
# degradations and never gate readiness or the dataplane.
grep -q '"status": "ready"' "$tmp/native/readiness.json"
grep -q '"optional_missing": \["smartdns"' "$tmp/native/readiness.json"
grep -q '"hook":"af_xdp"' "$tmp/native/proof.json"
grep -Eq 'create interface af_xdp host-if eth1 name lyroute-proof-[0-9]+-[0-9]+-af_xdp .*zero-copy' "$tmp/native/vppctl.log"
if grep -q 'host-if eth0' "$tmp/native/vppctl.log"; then
  echo "management interface reached VPP probe" >&2
  exit 1
fi

VPPCTL_RDMA_SUCCESS=1 VPPCTL_RDMA_SCORE=50 VPPCTL_AFXDP_SCORE=90 \
  run_check native-benchmark eth0 eth1 'rdma_plugin.so af_xdp_plugin.so' 1
grep -q '"hook":"rdma"' "$tmp/native-benchmark/proof.json"
grep -q '"hook":"af_xdp"' "$tmp/native-benchmark/proof.json"
grep -q '"performance_score":90' "$tmp/native-benchmark/proof.json"

# AF_PACKET is always recorded as the last-resort candidate, never as a
# high-performance path.
run_check af-fallback eth0 eth1 af_packet_plugin.so 0
grep -q '"hook":"af_packet"' "$tmp/af-fallback/proof.json"
grep -q '"acceptance_only":true' "$tmp/af-fallback/proof.json"
grep -q '"high_performance":false' "$tmp/af-fallback/proof.json"

setup_inventory_uio_fixture dpdk-uio-miss eth1
VPPCTL_ACTIVE_PCI=0000:99:00.0 run_check dpdk-uio-miss eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dataplane_locked"' "$tmp/dpdk-uio-miss/readiness.json"
grep -q 'runtime_capability_proof:eth1' "$tmp/dpdk-uio-miss/readiness.json"

run_check unsupported eth0 eth1 unrelated_plugin.so 0
grep -q '"dataplane_state": "dataplane_locked"' "$tmp/unsupported/readiness.json"
grep -q 'runtime_capability_proof:eth1' "$tmp/unsupported/readiness.json"
grep -q '"proofs":\[\]' "$tmp/unsupported/proof.json"

setup_dpdk_fixture dpdk eth1
VPPCTL_ACTIVE_PCI=0000:03:00.0 run_check dpdk eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dpdk_ready"' "$tmp/dpdk/readiness.json"
grep -q '"tier":"vpp_dpdk"' "$tmp/dpdk/proof.json"
grep -q '"pci_address":"0000:03:00.0"' "$tmp/dpdk/proof.json"
grep -q '"kernel_driver":"ixgbe"' "$tmp/dpdk/proof.json"
grep -q '"iommu_group":"17"' "$tmp/dpdk/proof.json"
grep -q '"dpdk_plugin_available":true' "$tmp/dpdk/proof.json"

setup_inventory_uio_fixture dpdk-uio eth1
VPPCTL_ACTIVE_PCI=0000:03:00.0 run_check dpdk-uio eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dpdk_ready"' "$tmp/dpdk-uio/readiness.json"
grep -q '"mode":"uio_pci_generic"' "$tmp/dpdk-uio/proof.json"
grep -q '"transport":"uio"' "$tmp/dpdk-uio/proof.json"
grep -q '"high_performance":true' "$tmp/dpdk-uio/proof.json"
grep -q '"acceptance_only":false' "$tmp/dpdk-uio/proof.json"
grep -q '"vpp_interface":"GigabitEthernet3/0/0"' "$tmp/dpdk-uio/proof.json"
grep -q '"kernel_driver":"igb"' "$tmp/dpdk-uio/proof.json"

# Any NIC model is valid when VPP attaches it and reports up. vmxnet3 is the
# hardware-breadth regression that must never be gated on Intel e1000/igb.
setup_inventory_uio_fixture dpdk-vmxnet3 eth1 vmxnet3
VPPCTL_ACTIVE_PCI=0000:03:00.0 VPPCTL_DRIVER_MODEL='VMware vmxnet3' \
  run_check dpdk-vmxnet3 eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dpdk_ready"' "$tmp/dpdk-vmxnet3/readiness.json"
grep -q '"driver_model":"VMware vmxnet3"' "$tmp/dpdk-vmxnet3/proof.json"
grep -q '"high_performance":true' "$tmp/dpdk-vmxnet3/proof.json"
grep -q '"functional_verified":true' "$tmp/dpdk-vmxnet3/proof.json"
grep -q '"functional_method":"link_state_up"' "$tmp/dpdk-vmxnet3/proof.json"

# A down VPP interface is recorded but is not treated as an up native attach.
setup_inventory_uio_fixture dpdk-down eth1 vmxnet3
VPPCTL_ACTIVE_PCI=0000:03:00.0 VPPCTL_DRIVER_MODEL='VMware vmxnet3' \
  VPPCTL_LINK_STATE=down VPPCTL_HARDWARE_LINK=down \
  run_check dpdk-down eth0 eth1 dpdk_plugin.so 0
grep -q '"link_state":"down"' "$tmp/dpdk-down/proof.json"
grep -q '"hardware_link":"down"' "$tmp/dpdk-down/proof.json"
grep -q '"functional_verified":false' "$tmp/dpdk-down/proof.json"

# A port that was functionally verified (e.g. TX counter + link speed) is
# native even when the status string says down.
setup_inventory_uio_fixture dpdk-func-down eth1 vmxnet3
cat > "$tmp/dpdk-func-down/dpdk-verification.json" <<'EOF'
{"schema_version":1,"interfaces":[{"linux_interface":"eth1","pci_address":"0000:03:00.0","functional_verified":true,"method":"tx_counter_and_link_speed","link_state":"down","hardware_link":"down","link_speed_mbps":10000}]}
EOF
chmod 0600 "$tmp/dpdk-func-down/dpdk-verification.json"
VPPCTL_ACTIVE_PCI=0000:03:00.0 VPPCTL_DRIVER_MODEL='VMware vmxnet3' \
  VPPCTL_LINK_STATE=down VPPCTL_HARDWARE_LINK=down \
  run_check dpdk-func-down eth0 eth1 dpdk_plugin.so 0
grep -q '"link_state":"down"' "$tmp/dpdk-func-down/proof.json"
grep -q '"functional_verified":true' "$tmp/dpdk-func-down/proof.json"
grep -q '"functional_method":"tx_counter_and_link_speed"' "$tmp/dpdk-func-down/proof.json"

# An empty probe for the same realizable interfaces must not erase an existing
# valid proof.
mkdir -p "$tmp/preserved/sys/class/net/eth1" "$tmp/preserved/sys"
cat > "$tmp/preserved/proof.json" <<'EOF'
{"management_interface":"eth0","proofs":[{"linux_interface":"eth1","candidates":[{"tier":"vpp_native","hook":"af_xdp","mode":"zero_copy","source":"runtime_probe","runtime_verified":true,"native":true,"high_performance":true,"observed_at":"2026-10-25T12:00:00Z","valid_until":"2030-10-25T12:00:00Z","performance_score":100,"mac_address":"00:11:22:33:44:55"}]}]}
EOF
chmod 0600 "$tmp/preserved/proof.json"
run_check preserved eth0 eth1 unrelated_plugin.so 0
grep -q '"hook":"af_xdp"' "$tmp/preserved/proof.json"

setup_active_dpdk_fixture dpdk-active eth1
VPPCTL_ACTIVE_PCI=0000:03:00.0 run_check dpdk-active eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dpdk_ready"' "$tmp/dpdk-active/readiness.json"
grep -q '"source":"active_runtime_readback"' "$tmp/dpdk-active/proof.json"
grep -q '"linux_interface":"eth1"' "$tmp/dpdk-active/proof.json"
grep -q '"kernel_driver":"ixgbe"' "$tmp/dpdk-active/proof.json"
if [ -e "$tmp/dpdk-active/sys/class/net/eth1" ]; then
  echo "active DPDK fixture unexpectedly retained a Linux netdev" >&2
  exit 1
fi

# arch-v1: the retired smart-QoS plugin must not appear in the observed
# capability, and its presence or absence must not change the tier decision.
setup_active_dpdk_fixture dpdk-retired-smart-qos eth1
VPPCTL_ACTIVE_PCI=0000:03:00.0 \
  run_check dpdk-retired-smart-qos eth0 eth1 'dpdk_plugin.so ly_route_smart_qos_plugin.so' 0
grep -q '"dataplane_state": "dpdk_ready"' "$tmp/dpdk-retired-smart-qos/readiness.json"
if grep -q 'smart_qos' "$tmp/dpdk-retired-smart-qos/proof.json"; then
  echo "retired smart-QoS capability leaked into the observed proof" >&2
  exit 1
fi

mkdir -p "$tmp/dpdk-no-iommu/sys/class/net/eth1" "$tmp/dpdk-no-iommu/sys/devices/pci0000:00/0000:03:00.0" \
  "$tmp/dpdk-no-iommu/sys/bus/pci/drivers/ixgbe" "$tmp/dpdk-no-iommu/sys/module/vfio_pci" \
  "$tmp/dpdk-no-iommu/sys/kernel/mm/hugepages/hugepages-2048kB"
ln -s "$tmp/dpdk-no-iommu/sys/devices/pci0000:00/0000:03:00.0" "$tmp/dpdk-no-iommu/sys/class/net/eth1/device"
ln -s "$tmp/dpdk-no-iommu/sys/bus/pci/drivers/ixgbe" "$tmp/dpdk-no-iommu/sys/devices/pci0000:00/0000:03:00.0/driver"
printf '%s\n' 64 > "$tmp/dpdk-no-iommu/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
run_check dpdk-no-iommu eth0 eth1 dpdk_plugin.so 0
grep -q '"dataplane_state": "dataplane_locked"' "$tmp/dpdk-no-iommu/readiness.json"
grep -q 'runtime_capability_proof:eth1' "$tmp/dpdk-no-iommu/readiness.json"

run_check management eth0 eth0 af_xdp_plugin.so 1
grep -q '"dataplane_state": "dataplane_locked"' "$tmp/management/readiness.json"
grep -q 'management_excluded:eth0' "$tmp/management/readiness.json"
if grep -Eq 'host-if eth0|hardware-interfaces .*eth0' "$tmp/management/vppctl.log"; then
  echo "management assignment executed interface-level VPP probes" >&2
  exit 1
fi

run_check shared-management eth0 eth0 af_xdp_plugin.so 1 true
grep -q '"dataplane_state": "native_ready"' "$tmp/shared-management/readiness.json"
grep -q '"linux_interface":"eth0"' "$tmp/shared-management/proof.json"
grep -Eq 'create interface af_xdp host-if eth0 name lyroute-proof-[0-9]+-[0-9]+-af_xdp .*zero-copy' "$tmp/shared-management/vppctl.log"

run_check invalid-shared-mode eth0 eth0 af_xdp_plugin.so 1 shared
grep -q '"dataplane_state": "dataplane_locked"' "$tmp/invalid-shared-mode/readiness.json"
grep -q 'management_shared_valid' "$tmp/invalid-shared-mode/readiness.json"

printf 'VPP native selection scenarios passed\n'
