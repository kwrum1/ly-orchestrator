#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture="$repo_root/packaging/fixtures/product-conversion/gateway-to-orchestrator.json"
[ -f "$fixture" ] || { echo "missing conversion fixture: $fixture" >&2; exit 1; }

cd "$repo_root/backend"
go test ./internal/productupdate ./internal/persistence -count=1
go test ./internal/httpapi -run 'TestOrchestratorUpgrade|TestGatewayConversion|TestFirmwareUpgradeInstaller' -count=1

printf 'orchestrator upgrade and product-conversion backend contracts passed\n'
