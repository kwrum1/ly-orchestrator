#!/usr/bin/env bash
# Build an Orchestrator upgrade package accepted by this repository's own
# installer (backend/internal/httpapi/orchestrator_upgrade_install.go and the
# strict stage validator in backend/internal/httpapi/upgrade_package.go).
#
# Layout (paths are relative to the package root):
#   manifest.json                      strict JSON, package_type ly-route-upgrade
#   checksums.sha256                   sha256sum -c compatible
#   usr/lib/ly-route/ly-route-control  control-plane binary (executable)
#   usr/lib/ly-route/vpp-apply         dataplane helper (executable, required)
#   opt/ly-route/admin/...             built console (app.js/index.html/...)
#   etc/ly-route/version.env           LY_ROUTE_VERSION=<version>
#
# The frozen baseline packaging/orchestrator-ui-baseline/ is never modified or
# used as input here: the console is rebuilt from frontend/.
#
# Usage:
#   scripts/build-orchestrator-upgrade-package.sh --version 0.1 [--out dist]
#     [--base-version 26.06+lyroute1] [--arch amd64|arm64]
#     [--ui DIR] [--control FILE] [--vpp-apply FILE] [--skip-build]
#     [--commit REV] [--services a.service,b.service]
#
# Defaults:
#   --out dist
#   --arch amd64
#   --vpp-apply $LY_ROUTE_VPP_APPLY, then /usr/lib/ly-route/vpp-apply
#   --ui/--control: built from frontend/ and backend/ unless --skip-build
set -euo pipefail

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=""
out_dir="$repo_root/dist"
base_version="${LY_ROUTE_BASE_VERSION:-26.06+lyroute1}"
arch="amd64"
ui_dir=""
control_file=""
vpp_apply="${LY_ROUTE_VPP_APPLY:-/usr/lib/ly-route/vpp-apply}"
skip_build=false
commit=""
services="ly-route-control-api.service"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --out) out_dir=${2:-}; shift 2 ;;
    --base-version) base_version=${2:-}; shift 2 ;;
    --arch) arch=${2:-}; shift 2 ;;
    --ui) ui_dir=${2:-}; shift 2 ;;
    --control) control_file=${2:-}; shift 2 ;;
    --vpp-apply) vpp_apply=${2:-}; shift 2 ;;
    --skip-build) skip_build=true; shift ;;
    --commit) commit=${2:-}; shift 2 ;;
    --services) services=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required (for example 0.1)" >&2; exit 2; }
case "$version" in
  *[!0-9A-Za-z.+~-]*) echo "invalid version: $version" >&2; exit 2 ;;
esac
case "$arch" in
  amd64) goarch=amd64 ;;
  arm64|aarch64) arch=arm64; goarch=arm64 ;;
  *) echo "unsupported arch: $arch (use amd64 or arm64)" >&2; exit 2 ;;
esac

command -v zstd >/dev/null 2>&1 || { echo "zstd is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }

if [ -z "$commit" ]; then
  commit=$(cd "$repo_root" && git rev-parse --short HEAD 2>/dev/null || echo unknown)
fi

stage=$(mktemp -d "${TMPDIR:-/tmp}/ly-orch-upgrade-$version.XXXXXX")
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/usr/lib/ly-route" "$stage/opt/ly-route/admin" "$stage/etc/ly-route"

# 1. Console. Build from frontend/ unless the caller supplied a dist bundle.
if [ -z "$ui_dir" ]; then
  [ "$skip_build" = false ] || { echo "--skip-build requires --ui" >&2; exit 2; }
  ui_dir="$stage/.ui-build"
  sh "$repo_root/scripts/build-orchestrator-ui.sh" --out "$ui_dir"
fi
for required in index.html app.js styles.css; do
  [ -f "$ui_dir/$required" ] || { echo "UI bundle is incomplete: $ui_dir (missing $required)" >&2; exit 1; }
done
cp -a "$ui_dir/." "$stage/opt/ly-route/admin/"
rm -rf "$stage/.ui-build" "$stage/opt/ly-route/admin/.ui-build"

# 2. Control binary. The version is stamped into the binary as well as
# version.env, so /health and the installer preflight agree even before the
# EnvironmentFile is reloaded.
if [ -z "$control_file" ]; then
  [ "$skip_build" = false ] || { echo "--skip-build requires --control" >&2; exit 2; }
  control_file="$stage/.control-build/ly-route-control"
  mkdir -p "$(dirname "$control_file")"
  (
    cd "$repo_root/backend"
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath \
      -ldflags "-s -w -X ly-route/backend/orchestrator.buildVersion=$version" \
      -o "$control_file" ./cmd/orchestrator-control
  )
fi
[ -f "$control_file" ] || { echo "control binary is missing: $control_file" >&2; exit 1; }
install -m 0755 "$control_file" "$stage/usr/lib/ly-route/ly-route-control"
rm -rf "$stage/.control-build"

# 3. vpp-apply. The installer refuses a package without it; the appliance
# package owns the helper, so this build only carries the current file
# forward. Pass --vpp-apply or LY_ROUTE_VPP_APPLY when building elsewhere.
[ -f "$vpp_apply" ] || {
  echo "vpp-apply helper is missing: $vpp_apply" >&2
  echo "pass --vpp-apply /path/to/vpp-apply (copy it from the appliance package)" >&2
  exit 1
}
install -m 0755 "$vpp_apply" "$stage/usr/lib/ly-route/vpp-apply"

# 4. Version identity read by ly-route-control-api.service (EnvironmentFile).
printf 'LY_ROUTE_VERSION=%s\n' "$version" >"$stage/etc/ly-route/version.env"

# 5. Checksums cover every payload file except the two metadata files.
(
  cd "$stage"
  find . -type f ! -name manifest.json ! -name checksums.sha256 -printf '%P\n' | LC_ALL=C sort | while IFS= read -r file; do
    printf '%s  %s\n' "$(sha256sum -- "$file" | cut -d' ' -f1)" "$file"
  done >checksums.sha256
)

# 6. Manifest. The stage validator uses DisallowUnknownFields, so only the
# fields declared by upgradePackageManifest may appear. The installer greps
# for `"field": "value"` with a space after the colon, which json.dumps(indent=2)
# produces.
manifest_checksums=$(python3 - "$stage" <<'PY'
import hashlib, json, os, sys
root = sys.argv[1]
checksums = {}
for base, _dirs, files in os.walk(root):
    for name in files:
        full = os.path.join(base, name)
        rel = os.path.relpath(full, root).replace(os.sep, "/")
        if rel in ("manifest.json", "checksums.sha256"):
            continue
        with open(full, "rb") as handle:
            checksums[rel] = hashlib.sha256(handle.read()).hexdigest()
print(json.dumps(checksums, sort_keys=True))
PY
)

python3 - "$stage/manifest.json" "$version" "$base_version" "$arch" "$commit" "$services" "$manifest_checksums" <<'PY'
import json, sys
from datetime import datetime, timezone
path, version, base_version, arch, commit, services, checksums = sys.argv[1:8]
manifest = {
    "package_type": "ly-route-upgrade",
    "version": version,
    "product": "orchestrator",
    "from_version": base_version,
    "to_version": version,
    "suite": "bookworm",
    "arch": arch,
    "commit": commit,
    "created_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "install_root": "/",
    "services": [item.strip() for item in services.split(",") if item.strip()],
    "checksums": json.loads(checksums),
}
with open(path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, indent=2, sort_keys=False)
    handle.write("\n")
PY

# 7. Sanity-check the contract the receiver enforces, before packaging.
python3 - "$stage" <<'PY'
import json, os, subprocess, sys
root = sys.argv[1]
manifest = json.load(open(os.path.join(root, "manifest.json"), encoding="utf-8"))
assert manifest["package_type"] == "ly-route-upgrade", manifest
assert manifest["product"] == "orchestrator", manifest
assert manifest["version"] == manifest["to_version"], manifest
required = ["usr/lib/ly-route/ly-route-control", "opt/ly-route/admin/app.js"]
for rel in required:
    assert rel in manifest["checksums"], f"manifest checksums missing {rel}"
for rel, digest in manifest["checksums"].items():
    full = os.path.join(root, rel)
    assert os.path.isfile(full), f"payload missing {rel}"
    import hashlib
    actual = hashlib.sha256(open(full, "rb").read()).hexdigest()
    assert actual == digest, f"checksum mismatch {rel}"
subprocess.run(["sha256sum", "-c", "checksums.sha256"], cwd=root, check=True,
               stdout=subprocess.DEVNULL)
for rel in ("usr/lib/ly-route/ly-route-control", "usr/lib/ly-route/vpp-apply"):
    mode = os.stat(os.path.join(root, rel)).st_mode
    assert mode & 0o111, f"{rel} is not executable"
PY

# 8. Deterministic-ish archive: tar with fixed owner metadata, zstd -19.
mkdir -p "$out_dir"
package_name="ly-route-orchestrator-${version}-${arch}.tar.zst"
package_path="$out_dir/$package_name"
rm -f "$package_path" "$package_path.sha256"
tar --owner=0 --group=0 --numeric-owner -cf - -C "$stage" . | zstd -19 --no-progress -q -o "$package_path"
(
  cd "$out_dir"
  sha256sum "$package_name" >"$package_name.sha256"
)

printf 'version:  %s\n' "$version"
printf 'base:     %s\n' "$base_version"
printf 'arch:     %s\n' "$arch"
printf 'package:  %s\n' "$package_path"
printf 'sha256:   %s\n' "$(cut -d' ' -f1 "$package_path.sha256")"
printf 'size:     %s bytes\n' "$(wc -c <"$package_path" | tr -d ' ')"
