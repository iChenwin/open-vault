#!/usr/bin/env bash
# Compile only. NEVER install or load kernel modules on the host.
set -Eeuo pipefail
umask 077
[[ $# == 4 ]] || { echo 'Usage: build-driver-host.sh KERNEL WORKSPACE PAYLOAD RESULTS' >&2; exit 1; }
kernel=$1 workspace=$2 payload=$3 results=$4
[[ $kernel =~ ^6\.8\.0-[0-9]+-generic$ && $(uname -m) == x86_64 ]] || exit 1
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 ]] || exit 1
for path in "$workspace" "$payload" "$results"; do [[ $path == /* ]] || exit 1; done
mkdir -p "$workspace" "$payload" "$results"
sudo apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends \
  build-essential git cmake binutils libelf-dev uuid-dev libboost-program-options-dev \
  libboost-filesystem-dev libssl-dev "linux-headers-$kernel"
commit=a162bc2064dbb1dbadf12bd1fc628bf82338d14f
source_dir=$workspace/blksnap
git clone --branch VAL-13.1 --depth 1 https://github.com/veeam/blksnap.git "$source_dir"
[[ $(git -C "$source_dir" rev-parse HEAD) == "$commit" ]] || { echo 'Upstream pin mismatch.' >&2; exit 1; }
[[ -s $source_dir/LICENSE.md && -s $source_dir/lib/LICENSE.md ]] || { echo 'Pinned upstream license files missing.' >&2; exit 1; }
printf '%s\n' "$commit" > "$results/blksnap-commit.txt"
printf 'native Ubuntu24.04 compiler; guest kernel=%s; no host install/load\n' "$kernel" > "$results/driver-build-location.txt"
make -j2 -C "/lib/modules/$kernel/build" M="$source_dir/module" modules
uuid_static=/usr/lib/x86_64-linux-gnu/libuuid.a
[[ -f $uuid_static ]] || exit 1
cmake -S "$source_dir" -B "$workspace/blksnap-build" \
  -DLIBUUID_LIBRARY="$uuid_static" -DCMAKE_EXE_LINKER_FLAGS=-static
cmake --build "$workspace/blksnap-build" --target blksnap-tools -j2
cp "$source_dir/module/bdevfilter.ko" "$source_dir/module/veeamblksnap.ko" "$payload/"
cp "$workspace/blksnap-build/tools/blksnap/blksnap" "$payload/blksnap"
# A standalone payload must not silently depend on guest shared libraries.
readelf -l "$payload/blksnap" > "$results/cli-program-headers.txt"
readelf -d "$payload/blksnap" > "$results/cli-dynamic.txt"
! grep -q INTERP "$results/cli-program-headers.txt"
! grep -q NEEDED "$results/cli-dynamic.txt"
cp "$source_dir/LICENSE.md" "$payload/LICENSE.KERNEL-AND-TOOLS"
cp "$source_dir/lib/LICENSE.md" "$payload/LICENSE.LIB"
printf '%s\n' "$commit" > "$payload/UPSTREAM_COMMIT"
printf '%s\n' "$kernel" > "$payload/TARGET_KERNEL"
printf '%s\n' 'https://github.com/veeam/blksnap' 'VAL-13.1' \
  'Kernel: GPL-2.0; tools/tests: GPL-2.0+; library/include: LGPL-3.0+' > "$payload/UPSTREAM.txt"
(cd "$payload" && sha256sum bdevfilter.ko veeamblksnap.ko blksnap LICENSE.KERNEL-AND-TOOLS LICENSE.LIB UPSTREAM_COMMIT TARGET_KERNEL UPSTREAM.txt > SHA256SUMS)
cp "$payload/SHA256SUMS" "$results/driver-payload.sha256"
