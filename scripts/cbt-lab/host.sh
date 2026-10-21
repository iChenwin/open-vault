#!/usr/bin/env bash
# Runs only file-backed QEMU disks. Does not format, mount, or pass host block devices.
set -Eeuo pipefail
umask 077

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
profile=${CBT_LAB_PROFILE:-dattobd}
build=${CBT_LAB_BUILD:-host}
case "$profile" in
  dattobd) lab_scripts="$repo/scripts/cbt-lab" ;;
  blksnap)
    lab_scripts="$repo/scripts/blksnap-lab"
    [[ $build == host ]] || { echo 'blksnap lab requires the native host build profile.' >&2; exit 1; }
    ;;
  *) echo 'CBT_LAB_PROFILE must be dattobd or blksnap.' >&2; exit 1 ;;
esac
results=${CBT_LAB_RESULTS:-"$repo/artifacts/cbt-kernel-lab"}
mkdir -p -- "$results"
results=$(realpath -- "$results")
printf '%s\n' "$profile" > "$results/driver-profile.txt"
work=$(mktemp -d "${RUNNER_TEMP:-/tmp}/open-vault-cbt-lab.XXXXXXXX")
printf 'Isolated lab workspace: %s\nResults: %s\n' "$work" "$results"

for tool in qemu-system-x86_64 qemu-img cloud-localds curl ssh scp ssh-keygen sha256sum python3 go; do
  command -v "$tool" >/dev/null || { echo "Missing host dependency: $tool" >&2; exit 1; }
done
[[ $(uname -m) == x86_64 ]] || { echo 'An x86_64 Linux host is required.' >&2; exit 1; }

qemu_pid=''
stop_guest() {
  if [[ -n $qemu_pid ]] && kill -0 "$qemu_pid" 2>/dev/null; then
    kill "$qemu_pid" 2>/dev/null || true
    wait "$qemu_pid" 2>/dev/null || true
  fi
  cp -- "$work/serial.log" "$results/serial.log" 2>/dev/null || true
  cp -- "$work/qemu.log" "$results/qemu.log" 2>/dev/null || true
  # Keep the temporary file-backed disks for diagnosis. GitHub removes its VM after the job.
}
trap stop_guest EXIT

base=${CBT_LAB_IMAGE_BASE:-https://cloud-images.ubuntu.com/noble/current}
image=noble-server-cloudimg-amd64.img
curl --fail --location --retry 3 "$base/SHA256SUMS" -o "$work/SHA256SUMS"
curl --fail --location --retry 3 "$base/$image" -o "$work/$image"
awk -v img="$image" '$2 == img || $2 == "*" img { print $1 "  " img }' "$work/SHA256SUMS" > "$work/image.sha256"
[[ $(wc -l < "$work/image.sha256") == 1 ]] || { echo 'No unique image checksum found.' >&2; exit 1; }
(cd "$work" && sha256sum --check image.sha256)
cp -- "$work/image.sha256" "$results/image.sha256"
printf '%s/%s\n' "$base" "$image" > "$results/image-url.txt"

# Boot Canonical's matching kernel directly, so a firmware/GRUB failure cannot
# leave the lab waiting for SSH with an empty serial console. This tests data
# volume CBT, not bootloader or bare-metal recovery.
curl --fail --location --retry 3 "$base/unpacked/SHA256SUMS" -o "$work/kernel-SHA256SUMS"
for component in vmlinuz initrd; do
  name="noble-server-cloudimg-amd64-$component-generic"
  curl --fail --location --retry 3 "$base/unpacked/$name" -o "$work/$name"
  awk -v img="$name" '$2 == img || $2 == "*" img { print $1 "  " img }' "$work/kernel-SHA256SUMS" > "$work/$component.sha256"
  [[ $(wc -l < "$work/$component.sha256") == 1 ]] || { echo 'Missing unique kernel/initrd checksum.' >&2; exit 1; }
  (cd "$work" && sha256sum --check "$component.sha256")
  cp -- "$work/$component.sha256" "$results/$component.sha256"
done

qemu-img create -f qcow2 -F qcow2 -b "$work/$image" "$work/root.qcow2" 20G
qemu-img create -f raw "$work/ext4.raw" 512M
qemu-img create -f raw "$work/xfs.raw" 512M
ssh-keygen -q -t ed25519 -N '' -f "$work/id_ed25519"
pubkey=$(cat "$work/id_ed25519.pub")
cat > "$work/user-data" <<EOF
#cloud-config
users:
  - name: lab
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [sudo]
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - $pubkey
ssh_pwauth: false
disable_root: true
package_update: false
package_upgrade: false
EOF
printf 'instance-id: open-vault-cbt-%s\nlocal-hostname: open-vault-cbt-lab\n' "$(date +%s)" > "$work/meta-data"
cat > "$work/network-config" <<'EOF'
version: 2
ethernets:
  labnet0:
    match:
      macaddress: "52:54:00:12:34:56"
    set-name: labnet0
    dhcp4: true
EOF
cloud-localds --network-config "$work/network-config" "$work/seed.img" "$work/user-data" "$work/meta-data"

accel=${CBT_LAB_ACCEL:-auto}
if [[ $accel == auto ]]; then
  # Hosted runners may expose /dev/kvm without a functioning nested guest.
  # Prefer the reproducible software CPU unless an operator explicitly requests KVM.
  accel=tcg
fi
case "$accel" in
  kvm) cpu=host ;;
  tcg) cpu=max ;;
  *) echo 'CBT_LAB_ACCEL must be auto, kvm or tcg.' >&2; exit 1 ;;
esac
printf '%s\n' "$accel" > "$results/acceleration.txt"
printf 'QEMU acceleration: %s\n' "$accel"
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
qemu-system-x86_64 \
  -machine "q35,accel=$accel" -cpu "$cpu" -smp 2 -m 2048 \
  -display none -monitor none -serial "file:$work/serial.log" \
  -device virtio-rng-pci \
  -kernel "$work/noble-server-cloudimg-amd64-vmlinuz-generic" \
  -initrd "$work/noble-server-cloudimg-amd64-initrd-generic" \
  -append 'root=LABEL=cloudimg-rootfs ro console=ttyS0,115200n8 loglevel=6' \
  -no-reboot \
  -drive "file=$work/root.qcow2,format=qcow2,if=virtio,discard=ignore" \
  -drive "file=$work/seed.img,format=raw,if=virtio,readonly=on" \
  -drive "file=$work/ext4.raw,format=raw,if=none,id=ext4,discard=ignore" \
  -device virtio-blk-pci,drive=ext4,serial=OPENVAULT_CBT_EXT4 \
  -drive "file=$work/xfs.raw,format=raw,if=none,id=xfs,discard=ignore" \
  -device virtio-blk-pci,drive=xfs,serial=OPENVAULT_CBT_XFS \
  -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$port-:22" \
  -device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56 > "$work/qemu.log" 2>&1 &
qemu_pid=$!
ssh_opts=(-i "$work/id_ed25519" -o BatchMode=yes -o StrictHostKeyChecking=accept-new
  -o UserKnownHostsFile="$work/known_hosts" -o ConnectTimeout=5 -o ServerAliveInterval=15 -o ServerAliveCountMax=8)
guest=(ssh "${ssh_opts[@]}" -p "$port" lab@127.0.0.1)
ready=false
for attempt in $(seq 1 120); do
  kill -0 "$qemu_pid" 2>/dev/null || { cat "$work/qemu.log" >&2; exit 1; }
  # Persist boot evidence while waiting, including when the runner cancels the
  # shell before its EXIT trap can finish. Bound the entire SSH handshake.
  cp -- "$work/serial.log" "$results/serial.log" 2>/dev/null || true
  cp -- "$work/qemu.log" "$results/qemu.log" 2>/dev/null || true
  if timeout 8s "${guest[@]}" true 2>"$results/ssh-readiness.log"; then ready=true; break; fi
  if (( attempt % 12 == 0 )); then
    printf 'Waiting for guest SSH, attempt %s; recent serial output:\n' "$attempt"
    tail -n 5 "$work/serial.log" || true
  fi
  sleep 2
done
[[ $ready == true ]] || { echo 'Guest SSH readiness timed out.' >&2; tail -n 100 "$work/serial.log" >&2; cat "$results/ssh-readiness.log" >&2; exit 1; }
"${guest[@]}" 'sudo cloud-init status --wait'

"${guest[@]}" 'mkdir -p /home/lab/payload /home/lab/results'
if [[ $profile == dattobd ]]; then
  (cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$work/open-vault-cbt" ./cmd/open-vault-cbt)
  scp "${ssh_opts[@]}" -P "$port" "$work/open-vault-cbt" \
    "$lab_scripts/guest-prepare.sh" "$lab_scripts/guest-test.sh" \
    "$lab_scripts/prepare-driver.py" "$lab_scripts/image-diff.py" lab@127.0.0.1:/home/lab/payload/
else
  (cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$work/open-vault-cbt" ./cmd/open-vault-cbt)
  scp "${ssh_opts[@]}" -P "$port" "$work/open-vault-cbt" "$lab_scripts/guest-prepare.sh" "$lab_scripts/guest-test.sh" "$lab_scripts/guest-app-test.sh" lab@127.0.0.1:/home/lab/payload/
fi

# Native compilation avoids spending most TCG time compiling feature tests.
# Only the disposable guest installs and loads the resulting kernel module.
if [[ $build == host ]]; then
  kernel=$("${guest[@]}" uname -r)
  [[ $kernel =~ ^6\.8\.0-[0-9]+-generic$ ]] || exit 1
  if "${guest[@]}" "sudo test -r /boot/System.map-$kernel"; then
    "${guest[@]}" "sudo cat /boot/System.map-$kernel" > "$work/guest-System.map"
  fi
  bash "$lab_scripts/build-driver-host.sh" "$kernel" "$work" "$work/driver-bundle" "$results" 2>&1 | tee "$results/host-driver-build.log"
  scp "${ssh_opts[@]}" -P "$port" -r "$work/driver-bundle" lab@127.0.0.1:/home/lab/payload/
elif [[ $build != guest ]]; then
  echo 'CBT_LAB_BUILD must be host or guest.' >&2
  exit 1
fi

# Preserve diagnostics even when compilation, module loading, or a real backup fails.
set +e
if [[ $profile == blksnap ]]; then
  "${guest[@]}" 'sudo bash /home/lab/payload/guest-prepare.sh && sudo bash /home/lab/payload/guest-test.sh && sudo bash /home/lab/payload/guest-app-test.sh' 2>&1 | tee "$results/guest-console.log"
  rc=${PIPESTATUS[0]}
else
  "${guest[@]}" 'sudo bash /home/lab/payload/guest-prepare.sh && sudo bash /home/lab/payload/guest-test.sh' 2>&1 | tee "$results/guest-console.log"
  rc=${PIPESTATUS[0]}
fi
"${guest[@]}" 'sudo dmesg > /home/lab/results/dmesg.log; sudo journalctl -b --no-pager > /home/lab/results/journal.log; sudo cp /proc/datto-info /home/lab/results/datto-final.json 2>/dev/null; sudo chown -R lab:lab /home/lab/results' 2>/dev/null
scp "${ssh_opts[@]}" -P "$port" -r lab@127.0.0.1:/home/lab/results/. "$results/"
copy_rc=$?
set -e
[[ $rc == 0 && $copy_rc == 0 ]] || { echo "Real kernel lab failed: guest=$rc artifact-copy=$copy_rc" >&2; exit 1; }
test -f "$results/SUCCESS"
if [[ $profile == blksnap ]]; then test -f "$results/APP_SUCCESS"; fi
echo 'Real ext4 + XFS kernel CBT verification passed.'
