#!/usr/bin/env bash
set -euo pipefail

source_dir=${LY_ROUTE_VPP_SRC:?LY_ROUTE_VPP_SRC is required}
output_dir=${LY_ROUTE_RUNTIME_DEBS_DIR:?LY_ROUTE_RUNTIME_DEBS_DIR is required}
dev_output_dir=${LY_ROUTE_VPP_DEV_DEBS_DIR:-$output_dir}
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mirror=${LY_ROUTE_MIRROR:-https://deb.debian.org/debian}
pip_index=${LY_ROUTE_PIP_INDEX_URL:-https://pypi.org/simple}
local_repo=${LY_ROUTE_LOCAL_DEB_REPO:-}
arch=${LY_ROUTE_RUNTIME_DEB_ARCH:-arm64}
suite=${LY_ROUTE_VPP_BUILD_SUITE:-bookworm}

case "$arch" in
  amd64|arm64) ;;
  *) echo "bookworm VPP builder supports amd64 and arm64, got: $arch" >&2; exit 2 ;;
esac
[ -f "$source_dir/Makefile" ] || { echo "VPP source tree not found: $source_dir" >&2; exit 1; }
vpp_cmake_layout=false
[ -d "$source_dir/src/plugins" ] && vpp_cmake_layout=true
command -v mmdebstrap >/dev/null 2>&1 || { echo "mmdebstrap is required" >&2; exit 1; }
command -v chroot >/dev/null 2>&1 || { echo "chroot is required" >&2; exit 1; }

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/ly-route-vpp-bookworm.XXXXXX")
rootfs="$work_dir/rootfs"
mounted=0
local_repo_mounted=0
cleanup() {
  if [ "$local_repo_mounted" -eq 1 ]; then
    umount "$rootfs$local_repo" 2>/dev/null || true
  fi
  if [ "$mounted" -eq 1 ]; then
    umount "$rootfs/sys" "$rootfs/proc" "$rootfs/dev/pts" "$rootfs/dev" 2>/dev/null || true
  fi
  if [ "${LY_ROUTE_KEEP_BUILD_ROOT:-0}" = 1 ]; then
    printf 'preserving failed Bookworm build root: %s\n' "$work_dir" >&2
  else
    rm -rf "$work_dir"
  fi
}
trap cleanup EXIT INT TERM

include='apt,ca-certificates,gpgv,build-essential,bash,git,sudo,curl,python3,python3-minimal,python3-pip,python3-venv,python3-ply,cmake,ninja-build,meson,clang,nasm'
include="$include,dpkg-dev,debhelper,devscripts,pkg-config,libssl-dev,libelf-dev,libpcap-dev,libnuma-dev"
include="$include,libnl-3-dev,libnl-route-3-dev,zlib1g-dev,libunwind-dev,libsystemd-dev,libcap-dev"
include="$include,libmnl-dev,liblz4-dev,liblzma-dev,libzstd-dev,libibverbs-dev,libxdp-dev,libbpf-dev,libdpdk-dev,chrpath,python3-all,python3-setuptools,python3-wheel,dh-python,flex,bison,gawk,perl,tar,rsync"
mmdebstrap_args=(--mode=root --architectures="$arch" --variant=minbase --components=main --include="$include")
if [ -n "$local_repo" ]; then
  mirror="deb [trusted=yes] file://$local_repo $suite main"
  mmdebstrap_args+=(--hook-dir=/usr/share/mmdebstrap/hooks/file-mirror-automount --aptopt=Acquire::AllowInsecureRepositories=true --aptopt=APT::Get::AllowUnauthenticated=true)
fi
mmdebstrap "${mmdebstrap_args[@]}" "$suite" "$rootfs" "$mirror"

# Make the host download cache visible to VPP's external dependency recipes.
# Those recipes look specifically in /root/Downloads before reaching the
# network, so copy the cache into the isolated Bookworm rootfs.
download_cache=${LY_ROUTE_DOWNLOAD_CACHE:-/root/Downloads}
if [ -d "$download_cache" ]; then
  mkdir -p "$rootfs/root/Downloads"
  cp -a "$download_cache/." "$rootfs/root/Downloads/"
fi

mkdir -p "$rootfs/src/vpp" "$output_dir" "$dev_output_dir"
mount --bind /dev "$rootfs/dev"
mount --bind /dev/pts "$rootfs/dev/pts"
mount -t proc proc "$rootfs/proc"
mount -t sysfs sysfs "$rootfs/sys"
mounted=1
if [ -n "$local_repo" ]; then
  mkdir -p "$rootfs$local_repo"
  mount --bind "$local_repo" "$rootfs$local_repo"
  local_repo_mounted=1
fi
cp -a "$source_dir/." "$rootfs/src/vpp/"
source_version=$(git -C "$source_dir" describe --long --match 'v*' HEAD 2>/dev/null || true)
if [ -z "$source_version" ]; then
  source_commit=$(git -C "$source_dir" rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
  source_version="$(if [ "$vpp_cmake_layout" = true ]; then printf v26.06; else printf v25.10; fi)-0-g$source_commit"
fi
version_dir=src/scripts
mkdir -p "$rootfs/src/vpp/$version_dir"
printf '%s\n' "$source_version" > "$rootfs/src/vpp/$version_dir/.version"

patch_dir="$repo_root/packaging/vpp-patches"
if [ "$vpp_cmake_layout" = true ] && [ -d "$repo_root/packaging/vpp-patches/v26.06" ]; then
  patch_dir="$repo_root/packaging/vpp-patches/v26.06"
fi

# Apply repository-owned VPP fixes to the copied build tree. The source cache
# stays untouched so a failed build cannot leave a partially patched checkout.
for patch_path in "$patch_dir"/*.patch; do
  [ -f "$patch_path" ] || continue
  if git -C "$rootfs/src/vpp" apply --check --ignore-space-change --ignore-whitespace "$patch_path" >/dev/null 2>&1; then
    git -C "$rootfs/src/vpp" apply --ignore-space-change --ignore-whitespace --whitespace=nowarn "$patch_path"
  elif git -C "$rootfs/src/vpp" apply --reverse --check --ignore-space-change --ignore-whitespace "$patch_path" >/dev/null 2>&1; then
    : # The source cache already contains this patch.
  else
    echo "VPP source patch cannot be applied: $patch_path" >&2
    exit 1
  fi
done

rm -rf "$rootfs/src/vpp/build-root"/build-* \
  "$rootfs/src/vpp/build-root"/install-* \
  "$rootfs/src/vpp/build-root"/.deps.ok
rm -f "$rootfs/src/vpp/build-root"/*.deb \
  "$rootfs/src/vpp/build-root"/*.buildinfo \
  "$rootfs/src/vpp/build-root"/*.changes
make_args=${LY_ROUTE_VPP_MAKE_ARGS:-DPDK_MLX_IBV_LINK=dlopen}
if [ "$vpp_cmake_layout" = true ]; then
  build_command='export DEBIAN_FRONTEND=noninteractive PIP_NO_BUILD_ISOLATION=1 PYTHONPATH=/usr/lib/python3/dist-packages:${PYTHONPATH:-}; cd /src/vpp && make UNATTENDED=y install-dep; external_install=/src/vpp/build/external/_install; external_stage=/src/vpp/build-root/install-vpp-native/external; if [ -d "$external_install/include" ]; then rm -rf "$external_stage/include" "$external_stage/lib" "$external_stage/lib64"; mkdir -p "$external_stage/include" "$external_stage/lib"; cp -a "$external_install/include/." "$external_stage/include/"; cp -a "$external_install/lib/." "$external_stage/lib/" 2>/dev/null || true; if [ -d "$external_install/lib64" ]; then mkdir -p "$external_stage/lib64"; cp -a "$external_install/lib64/." "$external_stage/lib64/"; fi; rm -f "$external_stage/include/unistd.h" "$external_stage/include/getopt.h" "$external_stage/include/sched.h" "$external_stage/include/pthread.h" "$external_stage/include/signal.h" "$external_stage/include/stdio.h" "$external_stage/include/stdlib.h" "$external_stage/include/string.h" "$external_stage/include/strings.h" "$external_stage/include/time.h" "$external_stage/include/errno.h" "$external_stage/include/fcntl.h" "$external_stage/include/stdint.h" "$external_stage/include/inttypes.h" "$external_stage/include/rte_windows.h"; fi; export VPP_EXTRA_CMAKE_ARGS="-DDPDK_INCLUDE_DIR:PATH=$external_stage/include -DDPDK_LIB:FILEPATH=$external_stage/lib/libdpdk.a"; make UNATTENDED=y SOURCE_PATH=/src/vpp PLATFORM=vpp vpp_arch=native ${LY_ROUTE_VPP_MAKE_ARGS:-DPDK_MLX_IBV_LINK=dlopen} pkg-deb'
else
  build_command='export DEBIAN_FRONTEND=noninteractive; git config --global --add safe.directory /src/vpp && cd /src/vpp && make UNATTENDED=y install-dep && make UNATTENDED=y SOURCE_PATH=/src/vpp PLATFORM=vpp vpp_arch=native ${LY_ROUTE_VPP_MAKE_ARGS:-DPDK_MLX_IBV_LINK=dlopen} pkg-deb'
fi
LY_ROUTE_VPP_MAKE_ARGS="$make_args" PIP_INDEX_URL="$pip_index" PIP_DEFAULT_TIMEOUT=120 PIP_RETRIES=10 \
  PIP_DISABLE_PIP_VERSION_CHECK=1 BUILD_COMMAND="$build_command" chroot "$rootfs" /bin/bash -lc 'eval "$BUILD_COMMAND"'

found=0
package_list="$work_dir/packages.list"
find "$rootfs/src/vpp" -type f -name '*.deb' \
  \( -path '*/build-root/*.deb' -o -path '*/build-root/packages/*.deb' -o -path '*/build-root/install-vpp-native/*.deb' \) > "$package_list"

sanitize_deb_metadata() {
  local package_path=$1 package_root rebuilt
  package_root=$(mktemp -d "${TMPDIR:-/tmp}/ly-route-deb-sanitize.XXXXXX")
  rebuilt="$package_root/rebuilt.deb"
  dpkg-deb -R "$package_path" "$package_root/tree" >/dev/null
  if [ -f "$package_root/tree/DEBIAN/triggers" ]; then
    sed -i -E '/^[0-9]+$/d' "$package_root/tree/DEBIAN/triggers"
  fi
  dpkg-deb --build --root-owner-group "$package_root/tree" "$rebuilt" >/dev/null
  mv "$rebuilt" "$package_path"
  rm -rf "$package_root"
}

while IFS= read -r package; do
  [ -n "$package" ] || continue
  destination="$output_dir/$(basename "$package")"
  cp "$package" "$destination"
  sanitize_deb_metadata "$destination"
  package_name=$(dpkg-deb -f "$destination" Package)
  case "$package_name" in
    vpp-dev|libvppinfra-dev) cp "$destination" "$dev_output_dir/" ;;
  esac
  found=1
done < "$package_list"

if [ "$found" -ne 1 ]; then
  echo "Bookworm VPP build completed but no .deb packages were found" >&2
  exit 1
fi
if [ "$vpp_cmake_layout" = true ]; then
  dpdk_package=$(find "$output_dir" -maxdepth 1 -type f -name 'vpp-plugin-dpdk_*.deb' -print -quit)
  core_package=$(find "$output_dir" -maxdepth 1 -type f -name 'vpp-plugin-core_*.deb' -print -quit)
  [ -n "$dpdk_package" ] && dpkg-deb -c "$dpdk_package" | grep '/dpdk_plugin\.so$' >/dev/null || {
    echo "VPP 26.06 package is missing dpdk_plugin.so" >&2
    exit 1
  }
  [ -n "$core_package" ] || { echo "VPP 26.06 package is missing vpp-plugin-core" >&2; exit 1; }
  for plugin in af_xdp pppoe nat44_ei; do
    dpkg-deb -c "$core_package" | grep "/${plugin}_plugin\\.so$" >/dev/null || {
      echo "VPP 26.06 package is missing ${plugin}_plugin.so" >&2
      exit 1
    }
  done
fi
for package in "$output_dir"/*.deb; do
  [ -f "$package" ] || continue
  package_arch=$(dpkg-deb -f "$package" Architecture)
  [ "$package_arch" = "$arch" ] || {
    echo "VPP package architecture mismatch: $package ($package_arch, expected $arch)" >&2
    exit 1
  }
  package_deps=$(dpkg-deb -f "$package" Pre-Depends Depends Recommends Suggests 2>/dev/null || true)
  case "$package_deps" in
    *'libc6 (>= 2.38)'*|*'libc6 (>= 2.39)'*|*'libc6 (>= 2.40)'*|*'libc6 (>= 2.41)'*|*'libc6 (>= 2.42)'*|*libssl3t64*)
      echo "VPP package is not Bookworm-compatible: $package" >&2
      printf '%s\n' "$package_deps" >&2
      exit 1
      ;;
  esac
done
printf 'Bookworm-compatible VPP packages written to %s\n' "$output_dir"
