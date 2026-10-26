#!/usr/bin/env bash
# Orchestrator product contracts for this repository.
#
# The shared monorepo gate (scripts/test-product-builders.sh,
# scripts/validate-rootfs-scaffold.sh) also asserts Gateway packaging
# (smartdns/xray/pppoe/dnsmasq). Those products are not shipped from this
# repository, so this gate verifies the Orchestrator contract only:
#   * the build profile identifies the Orchestrator product;
#   * the shipped UI bundle matches the pinned hash and the product manifest;
#   * the Orchestrator profile ships no Gateway-only runtime package;
#   * the rootfs scaffold contains the Orchestrator units.
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

fail() {
  echo "$*" >&2
  exit 1
}

build_profile=packaging/build-profiles/orchestrator.json
product_manifest=packaging/product-profiles/orchestrator.json
bundle=packaging/orchestrator-frontend/bundle

for file in "$build_profile" "$product_manifest" "$bundle/app.js" "$bundle/index.html" "$bundle/styles.css" "$bundle/capabilities.json"; do
  [ -f "$file" ] || fail "missing required file: $file"
done

jq -e '.product == "orchestrator"' "$build_profile" >/dev/null || fail "build profile is not the orchestrator product"
jq -e '.product == "orchestrator"' "$product_manifest" >/dev/null || fail "product manifest is not the orchestrator product"

pinned_app=$(jq -r '.frontend_app_sha256' "$build_profile")
pinned_source=$(jq -r '.frontend_bundle_source' "$build_profile")
[ -n "$pinned_app" ] && [ "$pinned_app" != null ] || fail "orchestrator frontend app.js is not pinned"
[ -n "$pinned_source" ] && [ "$pinned_source" != null ] || fail "orchestrator frontend source commit is not pinned"

actual_app=$(sha256sum "$bundle/app.js" | awk '{print $1}')
[ "$actual_app" = "$pinned_app" ] || fail "shipped app.js ($actual_app) does not match the pinned hash ($pinned_app)"

# Use the same profile loader and bundle validator the rootfs build runs.
# shellcheck disable=SC1091
. scripts/lib/product-build-profile.sh
load_product_build_profile "$repo_root" orchestrator "$repo_root/$product_manifest"
validate_frontend_bundle "$repo_root/$bundle" orchestrator

# The Orchestrator image must not pull Gateway-only runtime packages.
if jq -r '(.extra_packages // "")' "$build_profile" | grep -Eqi 'smartdns|xray|dnsmasq|ppp|isc-dhcp'; then
  fail "orchestrator profile contains a Gateway-only runtime package"
fi

# The Orchestrator product manifest must not require Gateway services.
for service in smartdns xray dnsmasq pppoe; do
  if jq -r '.services[]?' "$product_manifest" | grep -qx "$service"; then
    fail "orchestrator product manifest requires the Gateway service: $service"
  fi
done

# The shipped rootfs overlay must expose the Orchestrator control units.
for unit in ly-route-control-api.service ly-route-recovery.service; do
  [ -f "packaging/rootfs-overlay/etc/systemd/system/$unit" ] || fail "missing rootfs unit: $unit"
done

echo "Orchestrator product contracts passed."
