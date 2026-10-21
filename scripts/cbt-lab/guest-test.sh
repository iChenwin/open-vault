#!/usr/bin/env bash
# Destructive operations are restricted to two serial-identified, disposable virtio disks.
set -Eeuo pipefail
umask 077
[[ $EUID == 0 ]] || { echo 'Guest tests require root.' >&2; exit 1; }
[[ $(systemd-detect-virt) == qemu || $(systemd-detect-virt) == kvm ]] || exit 1
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -r) == 6.8.*-generic ]] || exit 1
results=/home/lab/results
mkdir -p "$results"
exec > >(tee "$results/test.log") 2>&1
export OPEN_VAULT_CBT_LAB=1
mounted=''
trap 'if [[ -n $mounted ]]; then umount "$mounted" || true; fi' EXIT

find_test_disk() {
  local serial=$1 disk count=0 found=''
  for disk in /sys/class/block/vd*; do
    [[ -f $disk/serial ]] || continue
    if [[ $(cat "$disk/serial") == "$serial" ]]; then found=/dev/$(basename "$disk"); count=$((count + 1)); fi
  done
  [[ $count == 1 && -b $found ]] || { echo "No unique disposable disk for $serial" >&2; return 1; }
  [[ $(blockdev --getsize64 "$found") == 536870912 ]] || { echo 'Unexpected test disk size.' >&2; return 1; }
  # Must be a whole virtio disk, without partitions, mounts, or preexisting filesystem.
  [[ $(lsblk -nr -o NAME "$found" | wc -l) == 1 ]] || return 1
  ! lsblk -nr -o MOUNTPOINT "$found" | grep -q '[^[:space:]]' || return 1
  ! blkid "$found" >/dev/null 2>&1 || { echo 'Test disk already contains a filesystem.' >&2; return 1; }
  printf '%s\n' "$found"
}

manifest() {
  local root=$1 output=$2
  (cd "$root/payload" && find . -type f -print0 | sort -z | xargs -0 -r sha256sum) > "$output"
}

validate_restore() {
  local fs=$1 config=$2 generation=$3 oracle=$4 expected=$5 label=$6 out=$7
  local restored="$out/$label-restored.img" mountpoint="$out/check-$label" digest_oracle digest_restored
  local raw_equal=true fs_clean=true
  open-vault-cbt -config "$config" -generation "$generation" verify > "$results/$fs-$label-verify.json"
  open-vault-cbt -config "$config" -generation "$generation" -target "$restored" restore > "$results/$fs-$label-restore.json"
  digest_oracle=$(sha256sum "$oracle" | cut -d' ' -f1)
  digest_restored=$(sha256sum "$restored" | cut -d' ' -f1)
  printf '%s oracle=%s reconstructed=%s\n' "$label" "$digest_oracle" "$digest_restored" | tee "$results/$fs-$label-image-hashes.txt"
  if [[ $digest_oracle != "$digest_restored" ]] || ! cmp -- "$oracle" "$restored"; then
    raw_equal=false
    python3 /home/lab/payload/image-diff.py "$oracle" "$restored" "$results/$fs-$label-image-diff.json" "$out/agent-state/driver.json"
    # Only synthetic fixture volumes exist in this disposable guest.
    filefrag -v -b4096 /mnt/open-vault-lab-"$fs"/.open-vault-cbt/* > "$results/$fs-$label-cow-extents.txt" 2>&1 || true
    cp "$out/agent-state/driver.json" "$results/$fs-$label-driver-journal.json"
  fi
  # These are read-only checks against the byte-identical reconstruction.
  if [[ $fs == ext4 ]]; then
    e2fsck -fn "$restored" > "$results/$fs-$label-fsck.log" 2>&1 || fs_clean=false
  else
    xfs_repair -n -f "$restored" > "$results/$fs-$label-fsck.log" 2>&1 || fs_clean=false
  fi
  mkdir -p "$mountpoint"
  if [[ $fs == ext4 ]]; then
    mount -o loop,ro,noload "$restored" "$mountpoint"
  else
    mount -o loop,ro,norecovery,nouuid "$restored" "$mountpoint"
  fi
  mounted=$mountpoint
  manifest "$mountpoint" "$results/$fs-$label-restored-files.sha256"
  diff -u "$expected" "$results/$fs-$label-restored-files.sha256"
  umount "$mountpoint"
  mounted=''
  # File recovery evidence cannot override a raw-image or filesystem mismatch.
  [[ $raw_equal == true && $fs_clean == true ]]
}

# Identify BOTH pristine disks before formatting either one.
ext4_device=$(find_test_disk OPENVAULT_CBT_EXT4)
xfs_device=$(find_test_disk OPENVAULT_CBT_XFS)
[[ $ext4_device != "$xfs_device" ]]
printf 'ext4=%s xfs=%s\n' "$ext4_device" "$xfs_device" | tee "$results/test-devices.txt"

for fs in ext4 xfs; do
  if [[ $fs == ext4 ]]; then device=$ext4_device; minor=0; else device=$xfs_device; minor=1; fi
  mountpoint=/mnt/open-vault-lab-$fs
  out=/var/lib/open-vault-lab/$fs
  mkdir -p "$mountpoint" "$out/vault" "$out/agent-state"
  queue=/sys/class/block/$(basename "$device")/queue/discard_max_bytes
  printf '0\n' > "$queue"
  [[ $(cat "$queue") == 0 ]] || { echo 'Discard remains enabled.' >&2; exit 1; }
  cat "$queue" > "$results/$fs-discard-max-bytes.txt"
  if [[ $fs == ext4 ]]; then mkfs.ext4 -F "$device"; else mkfs.xfs -f "$device"; fi
  mount -o nodiscard "$device" "$mountpoint"
  mkdir -p "$mountpoint/payload/nested"
  python3 - "$mountpoint/payload" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
(p / 'large.bin').write_bytes(bytes(range(256)) * (64 * 1024))
(p / 'change.bin').write_bytes(b'A' * 102400)
(p / 'delete.txt').write_text('remove this in generation two\n')
(p / 'rename.txt').write_text('preserve this across a rename\n')
(p / 'nested' / 'keep.txt').write_text('unchanged nested payload\n')
PY
  openssl rand -hex 32 > "$out/key"
  chmod 0600 "$out/key"
  mkdir -p "$mountpoint/.open-vault-cbt"
  chmod 0700 "$mountpoint/.open-vault-cbt"
  config=$out/cbt.json
  python3 - "$config" "$out" "$device" "$mountpoint" "$minor" "$(command -v dbdctl)" <<'PY'
import json, sys
config, out, device, mount, minor, dbdctl = sys.argv[1:]
with open(config, 'w') as f:
    json.dump({'repository': out + '/vault', 'key_file': out + '/key',
               'state_dir': out + '/agent-state', 'dbdctl': dbdctl,
               'volume': {'id': 'data-' + mount.rsplit('-', 1)[-1], 'device': device,
                          'mount_point': mount, 'cow_dir': mount + '/.open-vault-cbt',
                          'minor': int(minor)}}, f, indent=2)
PY
  cp "$config" "$results/$fs-config.json"
  open-vault-cbt -config "$config" diagnose > "$results/$fs-diagnose.json"
  sync
  manifest "$mountpoint" "$results/$fs-full-expected-files.sha256"
  open-vault-cbt -config "$config" -oracle-target "$out/full-oracle.img" backup > "$results/$fs-full-backup.json"
  full=$(jq -er '.generation.id' "$results/$fs-full-backup.json")
  jq -e '.generation.size == 536870912 and .generation.source_read_bytes == .generation.size' "$results/$fs-full-backup.json"
  cat /proc/datto-info > "$results/$fs-after-full-driver.json"
  validate_restore "$fs" "$config" "$full" "$out/full-oracle.img" "$results/$fs-full-expected-files.sha256" full "$out"

  python3 - "$mountpoint/payload" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
(p / 'change.bin').write_bytes(b'B' * 102400)
(p / 'new.bin').write_bytes(b'N' * 65536)
(p / 'delete.txt').unlink()
(p / 'rename.txt').rename(p / 'renamed.txt')
with (p / 'large.bin').open('r+b') as f:
    f.seek(8 * 1024 * 1024)
    f.write(b'patch-during-cbt' * 128)
PY
  sync
  manifest "$mountpoint" "$results/$fs-incremental-expected-files.sha256"
  open-vault-cbt -config "$config" -oracle-target "$out/incremental-oracle.img" backup > "$results/$fs-incremental-backup.json"
  incremental=$(jq -er '.generation.id' "$results/$fs-incremental-backup.json")
  jq -e --arg full "$full" '.generation.parent == $full and .generation.source_read_bytes > 0 and .generation.source_read_bytes < .generation.size' "$results/$fs-incremental-backup.json"
  cat /proc/datto-info > "$results/$fs-after-incremental-driver.json"
  validate_restore "$fs" "$config" "$incremental" "$out/incremental-oracle.img" "$results/$fs-incremental-expected-files.sha256" incremental "$out"
  # Reverify the old generation after committing the increment.
  open-vault-cbt -config "$config" -generation "$full" verify > "$results/$fs-full-verify-after-incremental.json"
  open-vault-cbt -config "$config" status > "$results/$fs-status.json"
  printf '%s full=%s incremental=%s: raw equality, filesystem check, files and CBT read reduction passed\n' "$fs" "$full" "$incremental" | tee -a "$results/summary.txt"
done

# Do not unload the driver between filesystems; the disposable guest is discarded as a whole.
printf 'Real kernel Ubuntu 24.04 GA 6.8 ext4/XFS CBT checks passed.\n' > "$results/SUCCESS"
