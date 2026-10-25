#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
artifact_dir=${LY_ROUTE_ORCHESTRATOR_DEMO_ARTIFACT_DIR:-"$repo_root/deploy/orchestrator-demo/artifacts"}

# Ship the accepted Orchestrator product UI, not the legacy controller shell.
. "$repo_root/scripts/lib/product-build-profile.sh"
load_product_build_profile "$repo_root" orchestrator ""
if [ -z "${PRODUCT_FRONTEND_BUNDLE:-}" ]; then
  echo "Orchestrator frontend bundle is not pinned in the build profile" >&2
  exit 1
fi
validate_frontend_bundle "$PRODUCT_FRONTEND_BUNDLE" orchestrator

rm -rf "$artifact_dir"
mkdir -p "$artifact_dir"
copy_product_frontend "$artifact_dir/admin" "$PRODUCT_FRONTEND_BUNDLE" orchestrator
(cd "$repo_root/backend" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$artifact_dir/ly-route-orchestrator" ./cmd/orchestrator-control)
chmod 0755 "$artifact_dir/ly-route-orchestrator"
docker compose -f "$repo_root/deploy/orchestrator-demo/docker-compose.yml" build
