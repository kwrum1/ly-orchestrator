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
import hashlib, io, os, re, sys

root = sys.argv[1]
index = os.path.join(root, "index.html")
text = io.open(index, encoding="utf-8").read()

def digest(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()[:12]

# The appliance serves the console with a week-long asset cache. app.js and
# styles.css already carry a content hash, but the extras bundle did not, so a
# shipped UI fix never reached an operator who had opened the console before.
# The version must cover every extras asset, not just the entry script: the
# pages load each other by path, so a change to mirror.js has to invalidate the
# same cached copies as a change to orchestrator-extras.js.
extras_files = ["orchestrator-extras.js", "mirror.js", "mirror.html", "bypass.js", "bypass.html"]
combined = hashlib.sha256()
for name in extras_files:
    path = os.path.join(root, name)
    if os.path.isfile(path):
        with open(path, "rb") as handle:
            combined.update(name.encode())
            combined.update(handle.read())
extras_version = combined.hexdigest()[:12]
extras_tag = '<script src="/orchestrator-extras.js?v=%s"></script>' % extras_version
if "orchestrator-extras.js" in text:
    text = re.sub(r'<script src="/orchestrator-extras\.js[^"]*"></script>', extras_tag, text)
else:
    text = text.replace("  </body>", "    " + extras_tag + "\n  </body>")

io.open(index, "w", encoding="utf-8", newline="\n").write(text)
print("extras version:", extras_version)
PY

baseline_sha=$(sha256sum "$baseline/app.js" | cut -d' ' -f1)
out_sha=$(sha256sum "$out_dir/app.js" | cut -d' ' -f1)
[ "$baseline_sha" = "$out_sha" ] || { echo "app.js changed during composition" >&2; exit 1; }
[ "$(wc -c <"$out_dir/app.js" | tr -d ' ')" = "985377" ] || { echo "app.js is not the frozen 985377-byte baseline" >&2; exit 1; }

printf 'Composed baseline UI + extras at %s (app.js %s)\n' "$out_dir" "$out_sha"
