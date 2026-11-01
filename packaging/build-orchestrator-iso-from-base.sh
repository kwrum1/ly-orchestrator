#!/usr/bin/env bash
# Build the x86_64 Orchestrator installer ISO from a published base ISO and a
# release upgrade package. This script only assembles binary artifacts; it does
# not compile product source code.
#
# Usage:
#   packaging/build-orchestrator-iso-from-base.sh \
#     --version 1.0.4 \
#     --package ly-route-orchestrator-1.0.4-amd64.tar.zst \
#     --base-iso ly-route-orchestrator-1.0.3-amd64.iso \
#     --out dist
#
# Optional:
#   --image FILE      Base appliance image (.img or .img.zst); default: extract
#                     from the base ISO squashfs
#   --work DIR        Reuse/keep a work directory
#   --keep-work       Do not delete the work directory
#
# Requires root or passwordless sudo for loop mount/umount.
set -euo pipefail

usage() {
  sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
}

version=
package=
base_iso=
base_image=
out_dir=dist
work=
keep_work=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --package) package=${2:-}; shift 2 ;;
    --base-iso) base_iso=${2:-}; shift 2 ;;
    --image) base_image=${2:-}; shift 2 ;;
    --out) out_dir=${2:-}; shift 2 ;;
    --work) work=${2:-}; shift 2 ;;
    --keep-work) keep_work=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required" >&2; exit 2; }
case "$version" in *[!0-9A-Za-z.+~-]*) echo "invalid version: $version" >&2; exit 2 ;; esac
[ -f "$package" ] || { echo "--package must point to an existing upgrade package: $package" >&2; exit 2; }
[ -f "$base_iso" ] || { echo "--base-iso must point to an existing ISO: $base_iso" >&2; exit 2; }
if [ -n "$base_image" ] && [ ! -f "$base_image" ]; then
  echo "--image must point to an existing base image: $base_image" >&2; exit 2
fi

for command in xorriso zstd tar sha256sum stat awk sed losetup mount umount unsquashfs mksquashfs; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done

as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo -n "$@"
  fi
}
as_root true 2>/dev/null || {
  echo "root or passwordless sudo is required for loop mount/umount" >&2
  exit 1
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
case "$out_dir" in /*) artifact_dir=$out_dir ;; *) artifact_dir="$repo_root/$out_dir" ;; esac
mkdir -p "$artifact_dir"

if [ -z "$work" ]; then
  work_root=${LY_ROUTE_ISO_WORK_ROOT:-${TMPDIR:-/tmp}}
  mkdir -p "$work_root"
  work=$(mktemp -d "$work_root/ly-orchestrator-iso.XXXXXX")
fi
work=$(CDPATH= cd -- "$work" && pwd)

mnt=
loop=
cleanup() {
  set +e
  if [ -n "$mnt" ] && mountpoint -q "$mnt" 2>/dev/null; then
    as_root umount "$mnt"
  fi
  if [ -n "$loop" ]; then
    as_root losetup -d "$loop" 2>/dev/null
  fi
  if [ "$keep_work" = "1" ]; then
    echo "work directory kept: $work"
  else
    rm -rf "$work"
  fi
}
trap cleanup EXIT INT TERM

echo "== extract base squashfs =="
xorriso -osirrox on -indev "$base_iso" \
  -extract /live/filesystem.squashfs "$work/filesystem.squashfs" >/dev/null
[ -s "$work/filesystem.squashfs" ] || { echo "base ISO has no live/filesystem.squashfs" >&2; exit 1; }
unsquashfs -d "$work/squashfs-root" "$work/filesystem.squashfs" >/dev/null
rm -f "$work/filesystem.squashfs"
chmod -R u+w "$work/squashfs-root"
install_dir="$work/squashfs-root/opt/ly-route/install"

if [ -z "$base_image" ]; then
  base_image="$install_dir/ly-route-orchestrator-x86_64.img.zst"
fi
[ -f "$base_image" ] || { echo "base appliance image not found: $base_image" >&2; exit 1; }

echo "== prepare base image =="
base_raw="$work/base.img"
if [ -f "${base_image%.zst}" ]; then
  base_raw=${base_image%.zst}
  echo "using uncompressed base image: $base_raw"
else
  zstd -dc --sparse "$base_image" > "$base_raw"
fi
work_img="$work/ly-route-orchestrator-${version}-amd64.img"
cp --reflink=auto --sparse=always "$base_raw" "$work_img"
[ "$base_raw" = "${base_image%.zst}" ] || rm -f "$base_raw"

pkg="$work/pkg"
mkdir -p "$pkg"
tar --zstd -xf "$package" -C "$pkg"
for required in usr/lib/ly-route/ly-route-control usr/lib/ly-route/vpp-apply \
                opt/ly-route/admin etc/ly-route/version.env; do
  [ -e "$pkg/$required" ] || { echo "package is missing $required" >&2; exit 1; }
done
grep -qx "LY_ROUTE_VERSION=$version" "$pkg/etc/ly-route/version.env" || {
  echo "package version does not match --version $version" >&2
  exit 1
}

echo "== overlay release package into image =="
mnt="$work/mnt"
mkdir -p "$mnt"
loop=$(as_root losetup -Pf --show "$work_img")
sleep 1
as_root mount "${loop}p3" "$mnt"
as_root install -m 0755 "$pkg/usr/lib/ly-route/ly-route-control" "$mnt/usr/lib/ly-route/ly-route-control"
as_root install -m 0755 "$pkg/usr/lib/ly-route/vpp-apply" "$mnt/usr/lib/ly-route/vpp-apply"
as_root rm -rf "$mnt/opt/ly-route/admin"
as_root mkdir -p "$mnt/opt/ly-route/admin"
as_root cp -a "$pkg/opt/ly-route/admin/." "$mnt/opt/ly-route/admin/"
as_root install -m 0644 "$pkg/etc/ly-route/version.env" "$mnt/etc/ly-route/version.env"
as_root sync
as_root umount "$mnt"
as_root losetup -d "$loop"
mnt=
loop=

echo "== compress release image =="
zstd -T0 -3 --sparse -f "$work_img" -o "$work_img.zst"
image_size=$(stat -c %s "$work_img")
image_hash=$(sha256sum "$work_img" | awk '{print $1}')

echo "== update squashfs payload =="
rm -f "$install_dir"/ly-route-orchestrator-x86_64.img.zst* \
      "$install_dir"/ly-route-gateway-x86_64.img.zst*
as_root install -m 0644 "$work_img.zst" "$install_dir/ly-route-orchestrator-x86_64.img.zst"
( cd "$install_dir" && sha256sum ly-route-orchestrator-x86_64.img.zst > ly-route-orchestrator-x86_64.img.zst.sha256 )
printf '%s\n' "$image_size" > "$install_dir/image.size"
printf '%s\n' "$image_hash" > "$install_dir/image.sha256"
rm -f "$install_dir/RELEASE-HASHES.txt"

echo "== repack squashfs =="
mksquashfs "$work/squashfs-root" "$work/new-filesystem.squashfs" \
  -comp xz -b 131072 -noappend -no-progress -all-root >/dev/null

echo "== build ISO =="
iso_name="ly-route-orchestrator-${version}-amd64.iso"
iso_path="$artifact_dir/$iso_name"
rm -f "$iso_path" "$iso_path.sha256"
xorriso -indev "$base_iso" -outdev "$iso_path" \
  -map "$work/new-filesystem.squashfs" /live/filesystem.squashfs \
  -boot_image any replay -commit >/dev/null

ls -lh "$iso_path"
sha256sum "$iso_path" | tee "$iso_path.sha256"
echo "ISO ready: $iso_path"
