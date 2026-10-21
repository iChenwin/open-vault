#!/usr/bin/env bash
# Compile against the guest's exact headers; NEVER install or load on the host.
set -Eeuo pipefail
kernel=$1
workspace=$2
payload=$3
results=$4
[[ $kernel =~ ^6\.8\.0-[0-9]+-generic$ && $(uname -m) == x86_64 ]] || exit 1
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 ]] || { echo 'Host compilation requires Ubuntu 24.04.' >&2; exit 1; }
mkdir -p "$payload"
sudo apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends \
  build-essential git libelf-dev "linux-headers-$kernel"
if [[ -s $workspace/guest-System.map ]]; then
  sudo install -m 0644 "$workspace/guest-System.map" "/lib/modules/$kernel/System.map"
fi
commit=68bf21b8a9e74d1c8aac967238f81e189963c0a1
git clone --branch v0.12.0 --depth 1 https://github.com/datto/dattobd.git "$workspace/dattobd"
[[ $(git -C "$workspace/dattobd" rev-parse HEAD) == "$commit" ]] || exit 1
python3 "$(dirname -- "$0")/prepare-driver.py" "$workspace/dattobd"
printf '%s\n' "$commit" > "$results/dattobd-commit.txt"
printf 'native Ubuntu 24.04 host compiler; target guest kernel=%s; no host module install/load\n' "$kernel" > "$results/driver-build-location.txt"
cd "$workspace/dattobd"
git diff -- src/bio_helper.c src/dattobd.h > "$results/external-driver-build.diff"
make -j2 KERNELVERSION="$kernel"
grep -q '^#define HAVE_ENUM_REQ_OP_WRITE_ZEROES' src/kernel-config.h
cp src/kernel-config.h "$results/driver-kernel-config.h"
cp src/dattobd.ko lib/libdattobd.so.1 app/dbdctl "$payload/"
(cd "$payload" && sha256sum dattobd.ko libdattobd.so.1 dbdctl > SHA256SUMS)
cp "$payload/SHA256SUMS" "$results/driver-payload.sha256"
