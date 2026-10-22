#!/usr/bin/env sh
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/build-gateway-ui.sh --out DIRECTORY [--manifest PATH]
       LY_ROUTE_UI_DIST=/path/to/dist scripts/build-gateway-ui.sh --out DIRECTORY
EOF
}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
source_dir="$repo_root/frontend/gateway"
out_dir=""
manifest="$repo_root/packaging/product-profiles/gateway.json"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --out) out_dir=${2:-}; shift 2 ;;
    --manifest) manifest=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$out_dir" ]; then
  echo "--out is required" >&2
  exit 2
fi
if [ ! -f "$manifest" ]; then
  echo "gateway capability manifest is missing: $manifest" >&2
  exit 1
fi
if [ ! -f "$source_dir/package.json" ] || [ ! -f "$source_dir/vite.config.ts" ]; then
  echo "gateway UI source is incomplete: $source_dir" >&2
  exit 1
fi

dist_dir=${LY_ROUTE_UI_DIST:-}
if [ -z "$dist_dir" ]; then
  command -v pnpm >/dev/null 2>&1 || { echo "pnpm is required to build the gateway UI" >&2; exit 1; }
  (
    cd "$source_dir"
    if [ -f pnpm-lock.yaml ]; then
      pnpm install --frozen-lockfile
    else
      pnpm install
    fi
    pnpm build
  )
  dist_dir="$source_dir/dist"
fi
if [ ! -f "$dist_dir/index.html" ] || [ ! -f "$dist_dir/app.js" ] || [ ! -f "$dist_dir/styles.css" ]; then
  echo "gateway UI dist is incomplete: $dist_dir (need index.html/app.js/styles.css)" >&2
  exit 1
fi

rm -rf "$out_dir"
mkdir -p "$out_dir"
cp -a "$dist_dir/." "$out_dir/"
cp "$manifest" "$out_dir/capabilities.json"
# Keep the historical packaging contract: these names are hashed by
# build-upgrade-package.sh even though the new bundle does not use them.
[ -f "$out_dir/shell.js" ] || printf '%s\n' "/* compatibility placeholder */" > "$out_dir/shell.js"
[ -f "$out_dir/commercial.css" ] || printf '%s\n' "/* compatibility placeholder */" > "$out_dir/commercial.css"
find "$out_dir" -name '*.map' -delete

# Refuse an HTML entry that reaches a remote origin. JS libraries may contain
# namespace/XML URLs; the entrypoint is the meaningful offline contract.
if grep -qE '(src|href)="https?://' "$out_dir/index.html"; then
  echo "gateway UI index references a remote asset" >&2
  exit 1
fi
if grep -RIlE 'iKuai|UniFi|Ubiquiti|Panabit' "$out_dir" >/dev/null 2>&1; then
  echo "gateway UI bundle contains a third-party product name" >&2
  exit 1
fi

printf 'Built gateway UI bundle at %s\n' "$out_dir"
