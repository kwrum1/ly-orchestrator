#!/usr/bin/env bash
# Build the Orchestrator installer ISO by overlaying a release upgrade package
# into a base appliance disk image and replacing the payload of a cached
# squashfs root. This avoids the live-build/debootstrap path, which is not
# reliable on the CI runner.
#
# The base ISO is used only as the boot skeleton and as the source of the
# squashfs root when the cache is empty. The base image must be the appliance
# disk image (compressed or raw); the package supplies the release control
# binary, vpp-apply, UI and version.env.
#
# Usage:
#   scripts/build-orchestrator-installer-iso-from-base.sh \
#     --version 1.0.3 \
#     --image /path/base.img.zst \
#     --package /path/ly-route-orchestrator-1.0.3-amd64.tar.zst \
#     --out dist/release \
#     [--base-iso /path/base.iso] \
#     [--squashfs-root /path/squashfs-root] \
#     [--work /path/work]
#
# Optional environment:
#   LY_ROUTE_ORCH_BASE_ISO          default base ISO
#   LY_ROUTE_ORCH_SQUASHFS_CACHE    default squashfs root cache
#   LY_ROUTE_ORCH_ISO_WORK          default work directory
#   LY_ROUTE_ORCH_KEEP_WORK=1       keep the work directory for debugging
set -euo pipefail

usage() {
  sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'
}

version=
base_image=
package=
out_dir=
base_iso=${LY_ROUTE_ORCH_BASE_ISO:-/root/work/orch-base/ly-route-orchestrator-base.iso}
squashfs_root=${LY_ROUTE_ORCH_SQUASHFS_CACHE:-/root/work/orch-iso/sq/squashfs-root}
work=${LY_ROUTE_ORCH_ISO_WORK:-}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --image) base_image=${2:-}; shift 2 ;;
    --package) package=${2:-}; shift 2 ;;
    --out) out_dir=${2:-}; shift 2 ;;
    --base-iso) base_iso=${2:-}; shift 2 ;;
    --squashfs-root) squashfs_root=${2:-}; shift 2 ;;
    --work) work=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required" >&2; exit 2; }
case "$version" in *[!0-9A-Za-z.+~-]*) echo "invalid version: $version" >&2; exit 2 ;; esac
[ -f "$base_image" ] || { echo "--image must point to an existing base image: $base_image" >&2; exit 2; }
[ -f "$package" ] || { echo "--package must point to an existing upgrade package: $package" >&2; exit 2; }
[ -f "$base_iso" ] || { echo "--base-iso must point to an existing ISO: $base_iso" >&2; exit 2; }
[ -n "$out_dir" ] || { echo "--out is required" >&2; exit 2; }

for command in zstd xorriso mksquashfs unsquashfs losetup mountpoint sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
case "$out_dir" in /*) artifact_dir=$out_dir ;; *) artifact_dir="$repo_root/$out_dir" ;; esac
mkdir -p "$artifact_dir"

if [ -z "$work" ]; then
  work=$(mktemp -d "${LY_ROUTE_ORCH_ISO_WORK_ROOT:-/root/work/orch-iso}/ly-route-iso-from-base.XXXXXX")
else
  mkdir -p "$work"
fi
work=$(CDPATH= cd -- "$work" && pwd)
if findmnt -no OPTIONS -T "$work" 2>/dev/null | tr ',' '\n' | grep -qx nodev; then
  echo "ISO work directory is mounted nodev; set LY_ROUTE_ORCH_ISO_WORK_ROOT to a device-capable filesystem" >&2
  exit 1
fi

mnt=
loop=
cleanup() {
  set +e
  if [ -n "$mnt" ] && mountpoint -q "$mnt" 2>/dev/null; then
    umount "$mnt"
  fi
  if [ -n "$loop" ]; then
    losetup -d "$loop" 2>/dev/null
  fi
  if [ "${LY_ROUTE_ORCH_KEEP_WORK:-0}" != "1" ]; then
    rm -rf "$work"
  fi
}
trap cleanup EXIT INT TERM

# The published base image checksum file may carry the historical artifact
# name instead of the renamed file. Compare the digest, not the filename.
if [ -f "$base_image.sha256" ]; then
  expected=$(awk '{print $1}' "$base_image.sha256")
  actual=$(sha256sum "$base_image" | awk '{print $1}')
  [ -n "$expected" ] && [ "$expected" = "$actual" ] || {
    echo "base image checksum mismatch: expected $expected, got $actual" >&2
    exit 1
  }
fi

echo "== overlay release package =="
base_raw="$work/base.img"
if [ -f "${base_image%.zst}" ]; then
  base_raw=${base_image%.zst}
  echo "using uncompressed base image: $base_raw"
else
  echo "decompressing base image: $base_image"
  zstd -dc --sparse "$base_image" > "$base_raw"
fi
echo "copying base image to the release image"
work_img="$work/ly-route-orchestrator-${version}-amd64.img"
cp --reflink=auto --sparse=always "$base_raw" "$work_img"
[ "$base_raw" = "${base_image%.zst}" ] || rm -f "$base_raw"

pkg="$work/pkg"
mkdir -p "$pkg"
tar --zstd -xf "$package" -C "$pkg"
mnt="$work/mnt"
mkdir -p "$mnt"
echo "mounting release image"
loop=$(losetup -Pf --show "$work_img")
sleep 1
mount "${loop}p3" "$mnt"
echo "installing release payload into the image"
install -m 0755 "$pkg/usr/lib/ly-route/ly-route-control" "$mnt/usr/lib/ly-route/ly-route-control"
install -m 0755 "$pkg/usr/lib/ly-route/vpp-apply" "$mnt/usr/lib/ly-route/vpp-apply"
rm -rf "$mnt/opt/ly-route/admin"
mkdir -p "$mnt/opt/ly-route/admin"
cp -a "$pkg/opt/ly-route/admin/." "$mnt/opt/ly-route/admin/"
install -m 0644 "$pkg/etc/ly-route/version.env" "$mnt/etc/ly-route/version.env"
sync
umount "$mnt"
losetup -d "$loop"
mnt=
loop=

echo "compressing release image"
zstd -T0 -3 --sparse -f "$work_img" -o "$work_img.zst"
image_size=$(stat -c %s "$work_img")
image_hash=$(sha256sum "$work_img" | awk '{print $1}')
payload_hash=$(sha256sum "$work_img.zst" | awk '{print $1}')
echo "image: size=$image_size sha256=$image_hash payload_sha256=$payload_hash"

echo "== squashfs =="
if [ ! -d "$squashfs_root/opt/ly-route/install" ]; then
  echo "extracting squashfs root from $base_iso"
  rm -rf "$squashfs_root"
  mkdir -p "$(dirname "$squashfs_root")"
  xorriso -osirrox on -indev "$base_iso" -extract /live/filesystem.squashfs "$work/base-filesystem.squashfs" >/dev/null
  unsquashfs -d "$squashfs_root" "$work/base-filesystem.squashfs" >/dev/null
  rm -f "$work/base-filesystem.squashfs"
fi
chmod -R u+w "$squashfs_root"

install_dir="$squashfs_root/opt/ly-route/install"
mkdir -p "$install_dir"
rm -f "$install_dir"/ly-route-orchestrator-x86_64.img.zst* \
      "$install_dir"/ly-route-gateway-x86_64.img.zst*
install -m 0644 "$work_img.zst" "$install_dir/ly-route-orchestrator-x86_64.img.zst"
( cd "$install_dir" && sha256sum ly-route-orchestrator-x86_64.img.zst > ly-route-orchestrator-x86_64.img.zst.sha256 )
printf '%s\n' "$image_size" > "$install_dir/image.size"
printf '%s\n' "$image_hash" > "$install_dir/image.sha256"
rm -f "$install_dir/RELEASE-HASHES.txt"

installer="$squashfs_root/usr/local/sbin/ly-route-auto-install"
if [ -f "$installer" ]; then
  sed -i \
    -e 's/ly-route-gateway-x86_64\.img\.zst/ly-route-orchestrator-x86_64.img.zst/g' \
    -e 's/\[Ly Route\]/[ly Orchestrator]/g' \
    -e 's/Ly Route/ly Orchestrator/g' \
    "$installer"
  grep -n 'payload=\|ly Orchestrator' "$installer" | head -5
else
  echo "warning: installer script is missing from the squashfs root: $installer" >&2
fi

rm -f "$work/filesystem.squashfs"
mksquashfs "$squashfs_root" "$work/filesystem.squashfs" -comp xz -b 131072 -noappend -no-progress -all-root >/dev/null
ls -lh "$work/filesystem.squashfs"

echo "== boot menus =="
mkdir -p "$work/boot"
for p in /isolinux/isolinux.cfg /isolinux/live.cfg /isolinux/menu.cfg /isolinux/stdmenu.cfg /isolinux/utilities.cfg /boot/grub/grub.cfg; do
  name=$(printf '%s' "$p" | tr '/' '_')
  xorriso -osirrox on -indev "$base_iso" -extract "$p" "$work/boot/$name" >/dev/null 2>&1 || true
done
for f in "$work/boot"/*; do
  [ -f "$f" ] || continue
  sed -i \
    -e 's/Ly Route Appliance/ly Orchestrator/g' \
    -e 's/Start Ly Route Installer/Start ly Orchestrator Installer/g' \
    -e 's/Ly Route Safe Boot/ly Orchestrator Safe Boot/g' \
    -e 's/Ly Route Hardware Detection/ly Orchestrator Hardware Detection/g' \
    -e 's/Ly Route Installer/ly Orchestrator Installer/g' \
    -e 's/Ly Route/ly Orchestrator/g' "$f"
done

echo "== iso =="
iso_name="ly-route-orchestrator-${version}-amd64.iso"
iso_path="$artifact_dir/$iso_name"
rm -f "$iso_path" "$iso_path.sha256"
args=(-indev "$base_iso" -outdev "$iso_path" -map "$work/filesystem.squashfs" /live/filesystem.squashfs)
[ -f "$work/boot/_isolinux_isolinux.cfg" ] && args+=(-map "$work/boot/_isolinux_isolinux.cfg" /isolinux/isolinux.cfg)
[ -f "$work/boot/_isolinux_live.cfg" ] && args+=(-map "$work/boot/_isolinux_live.cfg" /isolinux/live.cfg)
[ -f "$work/boot/_isolinux_menu.cfg" ] && args+=(-map "$work/boot/_isolinux_menu.cfg" /isolinux/menu.cfg)
[ -f "$work/boot/_isolinux_stdmenu.cfg" ] && args+=(-map "$work/boot/_isolinux_stdmenu.cfg" /isolinux/stdmenu.cfg)
[ -f "$work/boot/_isolinux_utilities.cfg" ] && args+=(-map "$work/boot/_isolinux_utilities.cfg" /isolinux/utilities.cfg)
[ -f "$work/boot/_boot_grub_grub.cfg" ] && args+=(-map "$work/boot/_boot_grub_grub.cfg" /boot/grub/grub.cfg)
args+=(-boot_image any replay -commit)
xorriso "${args[@]}" >/dev/null
ls -lh "$iso_path"
sha256sum "$iso_path" | tee "$iso_path.sha256"
