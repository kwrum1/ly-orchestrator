#!/usr/bin/env sh
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/build-product-ui.sh --product gateway|orchestrator --out DIRECTORY [--manifest PATH]
       LY_ROUTE_UI_DIST=/path/to/dist scripts/build-product-ui.sh --product gateway --out DIRECTORY
EOF
}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
product=""
out_dir=""
manifest=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --product) product=${2:-}; shift 2 ;;
    --out) out_dir=${2:-}; shift 2 ;;
    --manifest) manifest=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$product" in
  gateway|orchestrator) ;;
  *) echo "Unsupported product: $product" >&2; exit 2 ;;
esac
if [ -z "$out_dir" ]; then
  echo "--out is required" >&2
  exit 2
fi

source_dir="$repo_root/frontend/gateway"
manifest=${manifest:-$repo_root/packaging/product-profiles/$product.json}
if [ ! -f "$manifest" ]; then
  echo "capability manifest is missing: $manifest" >&2
  exit 1
fi
if [ ! -f "$source_dir/package.json" ] || [ ! -f "$source_dir/vite.config.ts" ]; then
  echo "gateway UI source is incomplete: $source_dir" >&2
  exit 1
fi

dist_dir=${LY_ROUTE_UI_DIST:-}
if [ -z "$dist_dir" ]; then
  command -v pnpm >/dev/null 2>&1 || { echo "pnpm is required to build the product UI" >&2; exit 1; }
  (
    cd "$source_dir"
    if [ -f pnpm-lock.yaml ]; then
      pnpm install --frozen-lockfile
    else
      pnpm install
    fi
    pnpm exec tsc -b
    if [ "$product" = orchestrator ]; then
      LY_ROUTE_PRODUCT=orchestrator pnpm exec vite build --outDir dist-orchestrator
    else
      LY_ROUTE_PRODUCT=gateway pnpm exec vite build --outDir dist
    fi
  )
  if [ "$product" = orchestrator ]; then
    dist_dir="$source_dir/dist-orchestrator"
  else
    dist_dir="$source_dir/dist"
  fi
fi
if [ ! -f "$dist_dir/index.html" ] || [ ! -f "$dist_dir/app.js" ] || [ ! -f "$dist_dir/styles.css" ]; then
  echo "UI dist is incomplete: $dist_dir (need index.html/app.js/styles.css)" >&2
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

if [ "$product" = orchestrator ]; then
  sed -i 's#<title>Ly Route 出口网关</title>#<title>Ly Route 流量编排器</title>#' "$out_dir/index.html"
fi

if grep -qE '(src|href)="https?://' "$out_dir/index.html"; then
  echo "UI index references a remote asset" >&2
  exit 1
fi
if grep -RIlE 'iKuai|UniFi|Ubiquiti|Panabit' "$out_dir" >/dev/null 2>&1; then
  echo "UI bundle contains a third-party product name" >&2
  exit 1
fi

printf 'Built %s UI bundle at %s\n' "$product" "$out_dir"
