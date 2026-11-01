#!/usr/bin/env bash
set -euo pipefail
BASEISO=/root/work/dist-iso-f3bf3d6/generic/ly-route-gateway-x86_64-installer.iso
WORK=/root/work/orch-iso/iso-build
SQ=/root/work/orch-iso/sq/squashfs-root
IMG=/root/work/orch-iso/ly-route-orchestrator-1.0.0-amd64.img
IMGZ=/root/work/orch-iso/ly-route-orchestrator-1.0.0-amd64.img.zst
OUT=/root/work/ly-orchestrator/dist/iso/ly-route-orchestrator-1.0.0-amd64.iso
mkdir -p "$WORK" "$(dirname "$OUT")"

echo "== payload =="
rm -f "$SQ"/opt/ly-route/install/ly-route-gateway-x86_64.img.zst*
install -m 0644 "$IMGZ" "$SQ/opt/ly-route/install/ly-route-orchestrator-x86_64.img.zst"
( cd "$SQ/opt/ly-route/install" && sha256sum ly-route-orchestrator-x86_64.img.zst > ly-route-orchestrator-x86_64.img.zst.sha256 )
stat -c %s "$IMG" > "$SQ/opt/ly-route/install/image.size"
sha256sum "$IMG" | awk '{print $1}' > "$SQ/opt/ly-route/install/image.sha256"
rm -f "$SQ/opt/ly-route/install/RELEASE-HASHES.txt"
sed -i \
  -e 's/ly-route-gateway-x86_64\.img\.zst/ly-route-orchestrator-x86_64.img.zst/g' \
  -e 's/\[Ly Route\]/[ly Orchestrator]/g' \
  -e 's/Ly Route/ly Orchestrator/g' \
  "$SQ/usr/local/sbin/ly-route-auto-install"
grep -n 'payload=\|ly Orchestrator' "$SQ/usr/local/sbin/ly-route-auto-install" | head -5

echo "== squashfs =="
rm -f "$WORK/filesystem.squashfs"
mksquashfs "$SQ" "$WORK/filesystem.squashfs" -comp xz -b 131072 -noappend -no-progress -all-root >/dev/null
ls -lh "$WORK/filesystem.squashfs"

echo "== boot menus =="
mkdir -p "$WORK/boot"
for p in /isolinux/isolinux.cfg /isolinux/live.cfg /isolinux/menu.cfg /isolinux/stdmenu.cfg /isolinux/utilities.cfg /boot/grub/grub.cfg; do
  name=$(printf '%s' "$p" | tr '/' '_')
  xorriso -osirrox on -indev "$BASEISO" -extract "$p" "$WORK/boot/$name" >/dev/null 2>&1 || true
done
for f in "$WORK/boot"/*; do
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
args=(-indev "$BASEISO" -outdev "$OUT" -map "$WORK/filesystem.squashfs" /live/filesystem.squashfs)
[ -f "$WORK/boot/_isolinux_isolinux.cfg" ] && args+=(-map "$WORK/boot/_isolinux_isolinux.cfg" /isolinux/isolinux.cfg)
[ -f "$WORK/boot/_isolinux_live.cfg" ] && args+=(-map "$WORK/boot/_isolinux_live.cfg" /isolinux/live.cfg)
[ -f "$WORK/boot/_isolinux_menu.cfg" ] && args+=(-map "$WORK/boot/_isolinux_menu.cfg" /isolinux/menu.cfg)
[ -f "$WORK/boot/_isolinux_stdmenu.cfg" ] && args+=(-map "$WORK/boot/_isolinux_stdmenu.cfg" /isolinux/stdmenu.cfg)
[ -f "$WORK/boot/_isolinux_utilities.cfg" ] && args+=(-map "$WORK/boot/_isolinux_utilities.cfg" /isolinux/utilities.cfg)
[ -f "$WORK/boot/_boot_grub_grub.cfg" ] && args+=(-map "$WORK/boot/_boot_grub_grub.cfg" /boot/grub/grub.cfg)
args+=(-boot_image any replay -commit)
xorriso "${args[@]}" >/dev/null
ls -lh "$OUT"
sha256sum "$OUT" | tee "$OUT.sha256"
