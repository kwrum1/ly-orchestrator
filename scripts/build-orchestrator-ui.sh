#!/usr/bin/env sh
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/build-orchestrator-ui.sh --out DIRECTORY [--dist DIRECTORY]

Builds the orchestrator console from frontend/ (Vite + TypeScript) and writes a
bundle with the same static layout as the frozen baseline
(packaging/orchestrator-ui-baseline/):

  index.html  app.js  app.js.sha256  styles.css
  capabilities.json  shell.js  commercial.css  product.css

--dist skips the build and packages an existing frontend/dist directory.
The frozen baseline stays the deployment artifact: this script never writes to
packaging/orchestrator-ui-baseline/.
EOF
}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out_dir=""
dist_dir=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --out) out_dir=${2:-}; shift 2 ;;
    --dist) dist_dir=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$out_dir" ] || { echo "--out is required" >&2; exit 2; }

source_dir="$repo_root/frontend"
manifest="$repo_root/packaging/product-profiles/orchestrator.json"
[ -f "$manifest" ] || { echo "capability manifest is missing: $manifest" >&2; exit 1; }
[ -f "$source_dir/package.json" ] || { echo "UI source is missing: $source_dir" >&2; exit 1; }

if [ -z "$dist_dir" ]; then
  command -v pnpm >/dev/null 2>&1 || { echo "pnpm is required to build the orchestrator UI" >&2; exit 1; }
  (
    cd "$source_dir"
    if [ -f pnpm-lock.yaml ]; then
      pnpm install --frozen-lockfile
    else
      pnpm install
    fi
    pnpm exec tsc -b
    pnpm exec vite build
  )
  dist_dir="$source_dir/dist"
fi

for required in index.html app.js styles.css; do
  [ -f "$dist_dir/$required" ] || { echo "UI dist is incomplete: $dist_dir (missing $required)" >&2; exit 1; }
done

rm -rf "$out_dir"
mkdir -p "$out_dir"
cp -a "$dist_dir/." "$out_dir/"
cp "$manifest" "$out_dir/capabilities.json"

# The packaging contract has always carried these names; the React bundle does
# not use them, so keep explicit placeholders instead of dropping them.
[ -f "$out_dir/shell.js" ] || printf '%s\n' "/* compatibility placeholder */" > "$out_dir/shell.js"
[ -f "$out_dir/commercial.css" ] || printf '%s\n' "/* compatibility placeholder */" > "$out_dir/commercial.css"
[ -f "$out_dir/product.css" ] || printf '%s\n' "/* compatibility placeholder */" > "$out_dir/product.css"

find "$out_dir" -name '*.map' -delete

# Cache-bust the two entry assets the way the frozen baseline does.
node - "$out_dir" <<'NODE'
const { createHash } = require("node:crypto");
const { readFileSync, writeFileSync } = require("node:fs");
const path = require("node:path");
const dir = process.argv[2];
const appJs = readFileSync(path.join(dir, "app.js"));
const digest = createHash("sha256").update(appJs).digest("hex").slice(0, 12);
writeFileSync(path.join(dir, "app.js.sha256"), digest + "\n");
const indexPath = path.join(dir, "index.html");
const html = readFileSync(indexPath, "utf8")
  .replace('src="/app.js"', `src="/app.js?v=${digest}"`)
  .replace('href="/styles.css"', `href="/styles.css?v=${digest}"`);
writeFileSync(indexPath, html);
NODE

if grep -qE '(src|href)="https?://' "$out_dir/index.html"; then
  echo "UI index references a remote asset" >&2
  exit 1
fi

printf 'Built orchestrator UI bundle at %s\n' "$out_dir"
