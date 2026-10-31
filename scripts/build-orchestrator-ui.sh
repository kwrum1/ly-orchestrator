#!/usr/bin/env sh
# Compose the frozen baseline UI with the two incremental pages (镜像口 /
# 故障旁路). The baseline bundle is copied byte-for-byte; app.js is never
# rebuilt or modified. Only index.html gains one <script> tag.
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/build-orchestrator-ui-with-extras.sh --out DIRECTORY
EOF
}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out_dir=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --out) out_dir=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$out_dir" ] || { echo "--out is required" >&2; exit 2; }
baseline="$repo_root/packaging/orchestrator-ui-baseline"
extras="$repo_root/packaging/orchestrator-ui-extras"
[ -f "$baseline/app.js" ] || { echo "baseline UI is missing: $baseline" >&2; exit 1; }
[ -f "$extras/orchestrator-extras.js" ] || { echo "UI extras are missing: $extras" >&2; exit 1; }

rm -rf "$out_dir"
mkdir -p "$out_dir"
cp -a "$baseline/." "$out_dir/"
cp -a "$extras/." "$out_dir/"
rm -f "$out_dir/README.md"

python3 - "$out_dir" <<'PY'
import io, os, sys
root = sys.argv[1]
index = os.path.join(root, "index.html")
text = io.open(index, encoding="utf-8").read()
tag = '    <script src="/orchestrator-extras.js"></script>\n'
if "orchestrator-extras.js" not in text:
    text = text.replace("  </body>", tag + "  </body>")
io.open(index, "w", encoding="utf-8", newline="\n").write(text)
PY

baseline_sha=$(sha256sum "$baseline/app.js" | cut -d' ' -f1)
out_sha=$(sha256sum "$out_dir/app.js" | cut -d' ' -f1)
[ "$baseline_sha" = "$out_sha" ] || { echo "app.js changed during composition" >&2; exit 1; }
[ "$(wc -c <"$out_dir/app.js" | tr -d ' ')" = "985377" ] || { echo "app.js is not the frozen 985377-byte baseline" >&2; exit 1; }

printf 'Composed baseline UI + extras at %s (app.js %s)\n' "$out_dir" "$out_sha"
