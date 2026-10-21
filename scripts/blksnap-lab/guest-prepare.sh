#!/usr/bin/env bash
# Only the disposable file-backed QEMU guest may install/load this payload.
set -Eeuo pipefail
umask 077
[[ $EUID == 0 && $(uname -m) == x86_64 ]] || exit 1
virt=$(systemd-detect-virt)
[[ $virt == qemu || $virt == kvm ]] || exit 1
[[ $(cat /sys/class/dmi/id/product_name) == *QEMU* || $(cat /sys/class/dmi/id/sys_vendor) == QEMU ]] || exit 1
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -r) =~ ^6\.8\.0-[0-9]+-generic$ ]] || exit 1
results=/home/lab/results
mkdir -p "$results"
exec > >(tee "$results/prepare.log") 2>&1
printf 'Ubuntu=%s kernel=%s arch=%s\n' "$VERSION_ID" "$(uname -r)" "$(uname -m)" > "$results/platform.txt"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends xfsprogs e2fsprogs python3 kmod util-linux
cd /home/lab/payload/driver-bundle
sha256sum --check SHA256SUMS
[[ $(cat UPSTREAM_COMMIT) == a162bc2064dbb1dbadf12bd1fc628bf82338d14f && $(cat TARGET_KERNEL) == "$(uname -r)" ]] || exit 1
for module in bdevfilter veeamblksnap; do
  [[ $(modinfo -F vermagic "./$module.ko") == "$(uname -r) "* ]] || exit 1
  [[ ! -d /sys/module/$module ]] || { echo 'Guest module unexpectedly already loaded.' >&2; exit 1; }
  install -D -m 0644 "$module.ko" "/lib/modules/$(uname -r)/updates/$module.ko"
done
install -D -m 0755 blksnap /usr/local/bin/blksnap
install -d -m 0700 /var/lib/open-vault-blksnap-lab
cp LICENSE.KERNEL-AND-TOOLS LICENSE.LIB UPSTREAM.txt UPSTREAM_COMMIT SHA256SUMS "$results/"
depmod -a
modprobe bdevfilter
modprobe veeamblksnap
for module in bdevfilter veeamblksnap; do modinfo "$module" > "$results/modinfo-$module.txt"; done
blksnap version | tee "$results/blksnap-version.txt"
[[ $(cat "$results/blksnap-version.txt") == 7.0.0.0 ]] || exit 1
echo 'Pinned blksnap modules loaded only inside disposable QEMU guest.'
