#!/usr/bin/env bash
# Runs only file-backed QEMU disks. Does not format, mount, or pass host block devices.
set -Eeuo pipefail
umask 077

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
results=${CBT_LAB_RESULTS:-"$repo/artifacts/cbt-kernel-lab"}
mkdir -p -- "$results"
results=$(realpath -- "$results")
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
cloud-localds "$work/seed.img" "$work/user-data" "$work/meta-data"

accel=${CBT_LAB_ACCEL:-auto}
if [[ $accel == auto ]]; then
  if [[ -r /dev/kvm && -w /dev/kvm ]]; then accel=kvm; else accel=tcg; fi
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
  -machine "q35,accel=$accel" -cpu "$cpu" -smp 2 -m 3072 \
  -display none -monitor none -serial "file:$work/serial.log" \
  -drive "file=$work/root.qcow2,format=qcow2,if=virtio,discard=ignore" \
  -drive "file=$work/seed.img,format=raw,if=virtio,readonly=on" \
  -drive "file=$work/ext4.raw,format=raw,if=none,id=ext4,discard=ignore" \
  -device virtio-blk-pci,drive=ext4,serial=OPENVAULT_CBT_EXT4 \
  -drive "file=$work/xfs.raw,format=raw,if=none,id=xfs,discard=ignore" \
  -device virtio-blk-pci,drive=xfs,serial=OPENVAULT_CBT_XFS \
  -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$port-:22" \
  -device virtio-net-pci,netdev=net0 > "$work/qemu.log" 2>&1 &
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

(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$work/open-vault-cbt" ./cmd/open-vault-cbt)
"${guest[@]}" 'mkdir -p /home/lab/payload /home/lab/results'
scp "${ssh_opts[@]}" -P "$port" "$work/open-vault-cbt" \
  "$repo/scripts/cbt-lab/guest-prepare.sh" "$repo/scripts/cbt-lab/guest-test.sh" lab@127.0.0.1:/home/lab/payload/

# Preserve diagnostics even when compilation, module loading, or a real backup fails.
set +e
"${guest[@]}" 'sudo bash /home/lab/payload/guest-prepare.sh && sudo bash /home/lab/payload/guest-test.sh' 2>&1 | tee "$results/guest-console.log"
rc=${PIPESTATUS[0]}
"${guest[@]}" 'sudo dmesg > /home/lab/results/dmesg.log; sudo journalctl -b --no-pager > /home/lab/results/journal.log; sudo cp /proc/datto-info /home/lab/results/datto-final.json 2>/dev/null; sudo chown -R lab:lab /home/lab/results' 2>/dev/null
scp "${ssh_opts[@]}" -P "$port" -r lab@127.0.0.1:/home/lab/results/. "$results/"
copy_rc=$?
set -e
[[ $rc == 0 && $copy_rc == 0 ]] || { echo "Real kernel lab failed: guest=$rc artifact-copy=$copy_rc" >&2; exit 1; }
test -f "$results/SUCCESS"
echo 'Real ext4 + XFS kernel CBT verification passed.'
