#!/usr/bin/env bash
# Run only inside the disposable VM created by host.sh.
set -Eeuo pipefail
umask 077
[[ $EUID == 0 ]] || { echo 'Guest preparation requires root.' >&2; exit 1; }
[[ $(systemd-detect-virt) == qemu || $(systemd-detect-virt) == kvm ]] || { echo 'Refusing preparation outside QEMU/KVM.' >&2; exit 1; }
[[ $(cat /sys/class/dmi/id/product_name) == *QEMU* || $(cat /sys/class/dmi/id/sys_vendor) == QEMU ]] || { echo 'Expected a disposable QEMU guest.' >&2; exit 1; }
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -r) == 6.8.*-generic ]] || { echo 'Only Ubuntu 24.04 GA 6.8 generic is accepted.' >&2; exit 1; }
results=/home/lab/results
mkdir -p "$results"
exec > >(tee "$results/prepare.log") 2>&1
printf 'Ubuntu=%s kernel=%s arch=%s\n' "$VERSION_ID" "$(uname -r)" "$(uname -m)" | tee "$results/platform.txt"

export DEBIAN_FRONTEND=noninteractive
# Do not install a kernel metapackage or upgrade the image to HWE.
apt-get update
apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends build-essential git ca-certificates \
  "linux-headers-$(uname -r)" libelf-dev xfsprogs e2fsprogs openssl python3 jq

commit=68bf21b8a9e74d1c8aac967238f81e189963c0a1
git clone --branch v0.12.0 --depth 1 https://github.com/datto/dattobd.git /home/lab/dattobd
cd /home/lab/dattobd
[[ $(git rev-parse HEAD) == "$commit" ]] || { echo 'dattobd tag did not resolve to the audited commit.' >&2; exit 1; }
printf '%s\n' "$commit" > "$results/dattobd-commit.txt"
# Upstream installs its shared library under /usr/local/lib64.
install -d /usr/local/lib64
make -j2
make install
printf '/usr/local/lib64\n' > /etc/ld.so.conf.d/open-vault-dattobd-lab.conf
ldconfig
depmod -a
modprobe dattobd
modinfo dattobd | tee "$results/modinfo.txt"
cat /proc/datto-info | tee "$results/datto-initial.json"
install -m 0755 /home/lab/payload/open-vault-cbt /usr/local/bin/open-vault-cbt
echo 'Pinned real kernel driver loaded inside disposable guest.'
