#!/usr/bin/env sh
set -eu

# Backwards-compatible gateway entrypoint. Product-aware builds use
# build-product-ui.sh; this wrapper keeps existing release automation working.
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exec "$repo_root/scripts/build-product-ui.sh" --product gateway "$@"
