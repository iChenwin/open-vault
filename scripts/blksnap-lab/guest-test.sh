#!/usr/bin/env bash
# Format only BOTH pristine, serial-identified 512MiB QEMU fixture disks.
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
exec > >(tee "$results/test.log") 2>&1
CLI=/usr/local/bin/blksnap
[[ $($CLI version) == 7.0.0.0 && -d /sys/module/bdevfilter && -d /sys/module/veeamblksnap ]] || exit 1
active_id='' check_mount='' source_mount=''
cleanup() {
  [[ -z $check_mount ]] || umount "$check_mount" || true
  [[ -z $active_id ]] || "$CLI" snapshot_destroy --id "$active_id" || true
  [[ -z $source_mount ]] || umount "$source_mount" || true
  dmesg > "$results/dmesg.log" || true
}
trap cleanup EXIT
find_disk() {
  local serial=$1 disk found='' count=0
  for disk in /sys/class/block/vd*; do
    [[ -f $disk/serial ]] || continue
    if [[ $(cat "$disk/serial") == "$serial" ]]; then found=/dev/$(basename "$disk"); count=$((count + 1)); fi
  done
  [[ $count == 1 && -b $found && $(blockdev --getsize64 "$found") == 536870912 ]] || return 1
  [[ ! -e /sys/class/block/$(basename "$found")/partition && $(lsblk -nr -o NAME "$found" | wc -l) == 1 ]] || return 1
  ! lsblk -nr -o MOUNTPOINT "$found" | grep -q '[^[:space:]]' || return 1
  ! blkid "$found" >/dev/null 2>&1 || return 1
  [[ -z $(ls -A "/sys/class/block/$(basename "$found")/holders") ]] || return 1
  python3 - "$found" <<'PY'
import sys
with open(sys.argv[1], 'rb', buffering=0) as disk:
    while data := disk.read(1 << 20):
        if any(data): raise SystemExit('Fixture disk is not pristine zero-filled storage')
PY
  printf '%s\n' "$found"
}
manifest() {
  (cd "$1/payload" && find . -type f -print0 | sort -z | xargs -0 -r sha256sum) > "$2"
}
info() {
  "$CLI" snapshot_info --device "$device" > "$results/$fs-$1-snapshot.info"
  [[ $("$CLI" snapshot_info --device "$device" --field error_code) == 0 ]] || { echo 'Snapshot is corrupted.' >&2; return 1; }
}
validate_image() {
  local label=$1 image=$2 oracle=$3 expected=$4
  local inspection=$image
  sha256sum "$oracle" "$image" > "$results/$fs-$label-image-hashes.txt"
  cmp -- "$oracle" "$image"
  if [[ $fs == ext4 ]]; then
    e2fsck -fn "$image" > "$results/$fs-$label-fsck.log" 2>&1
  else
    # A frozen XFS image can contain a valid log requiring replay. Keep both
    # raw comparison images immutable and replay only a disposable copy.
    inspection=$out/$label-inspection.img
    cp --reflink=auto -- "$image" "$inspection"
    check_mount=$out/check-$label-replay
    mkdir -p "$check_mount"
    mount -o loop,nouuid "$inspection" "$check_mount"
    umount "$check_mount"
    check_mount=''
    xfs_repair -n -f "$inspection" > "$results/$fs-$label-fsck.log" 2>&1
  fi
  check_mount=$out/check-$label
  mkdir -p "$check_mount"
  if [[ $fs == ext4 ]]; then mount -o loop,ro,noload "$image" "$check_mount";
  else mount -o loop,ro,norecovery,nouuid "$inspection" "$check_mount"; fi
  manifest "$check_mount" "$results/$fs-$label-restored-files.sha256"
  diff -u "$expected" "$results/$fs-$label-restored-files.sha256"
  umount "$check_mount"
  check_mount=''
}

ext4_device=$(find_disk OPENVAULT_CBT_EXT4)
xfs_device=$(find_disk OPENVAULT_CBT_XFS)
[[ $ext4_device != "$xfs_device" ]]
printf 'ext4=%s xfs=%s\n' "$ext4_device" "$xfs_device" > "$results/test-devices.txt"
for fs in ext4 xfs; do
  if [[ $fs == ext4 ]]; then device=$ext4_device; else device=$xfs_device; fi
  out=/var/lib/open-vault-blksnap-lab/$fs
  source_mount=/mnt/open-vault-blksnap-$fs
  mkdir -p "$out/diff" "$source_mount"
  # All images and difference files must live on guest rootfs, on neither source.
  root_id=$(findmnt -n -o MAJ:MIN -T /)
  [[ $(findmnt -n -o MAJ:MIN -T "$out") == "$root_id" ]]
  for fixture in "$ext4_device" "$xfs_device"; do [[ $(lsblk -dn -o MAJ:MIN "$fixture") != "$root_id" ]]; done
  printf '0\n' > "/sys/class/block/$(basename "$device")/queue/discard_max_bytes"
  [[ $(cat "/sys/class/block/$(basename "$device")/queue/discard_max_bytes") == 0 ]]
  if [[ $fs == ext4 ]]; then mkfs.ext4 -F "$device"; else mkfs.xfs -f "$device"; fi
  mount -o nodiscard "$device" "$source_mount"
  mkdir -p "$source_mount/payload/nested"
  python3 - "$source_mount/payload" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
(p/'large.bin').write_bytes(bytes(range(256)) * 65536)
(p/'change.bin').write_bytes(b'A' * 102400)
(p/'delete.txt').write_text('delete in generation two\n')
(p/'rename.txt').write_text('preserve across rename\n')
(p/'nested'/'keep.txt').write_text('unchanged nested content\n')
PY
  sync
  manifest "$source_mount" "$results/$fs-full-expected-files.sha256"
  "$CLI" attach --device "$device"
  "$CLI" cbtinfo --device "$device" > "$results/$fs-before.info"
  active_id=$("$CLI" snapshot_create --device "$device" --file "$out/diff" --limit 1G)
  [[ $active_id =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]]
  "$CLI" snapshot_take --id "$active_id"
  printf '%s\n' "$active_id" > "$results/$fs-full-snapshot-id.txt"
  info full-before
  snapshot=$("$CLI" snapshot_info --device "$device" --field image)
  [[ $snapshot == /dev/* && -b $snapshot && $(blockdev --getsize64 "$snapshot") == 536870912 ]]
  "$CLI" cbtinfo --device "$device" > "$results/$fs-full.info"
  "$CLI" readcbtmap --device "$device" --file "$results/$fs-full.map"
  dd if="$snapshot" of="$out/full-oracle.img" bs=1M status=none
  # Force real COW traffic, then read the same snapshot again. Its image and
  # expected file set must remain unchanged while the live source changes.
  python3 - "$source_mount/payload" <<'PY'
from pathlib import Path
import os, sys
p = Path(sys.argv[1])
(p/'written-during-full.txt').write_text('live source changes after full snapshot\n')
with (p/'large.bin').open('r+b') as f:
    f.seek(4 * 1024 * 1024)
    f.write(b'C' * 65536)
    f.flush()
    os.fsync(f.fileno())
PY
  sync
  dd if="$snapshot" of="$out/full.img" bs=1M status=none
  "$CLI" cbtinfo --device "$device" > "$results/$fs-full-after-read.info"
  cmp "$results/$fs-full.info" "$results/$fs-full-after-read.info"
  info full-after
  validate_image full "$out/full.img" "$out/full-oracle.img" "$results/$fs-full-expected-files.sha256"
  "$CLI" snapshot_destroy --id "$active_id"
  active_id=''
  # Deliberately keep the tracker attached across snapshot destruction.
  python3 - "$source_mount/payload" <<'PY'
from pathlib import Path
import os, sys
p = Path(sys.argv[1])
(p/'change.bin').write_bytes(b'B' * 102400)
(p/'new.bin').write_bytes(b'N' * 65536)
(p/'delete.txt').unlink()
(p/'rename.txt').rename(p/'renamed.txt')
with (p/'large.bin').open('r+b') as f:
    f.seek(8 * 1024 * 1024)
    f.write(b'patched-during-tracking' * 128)
    f.flush()
    os.fsync(f.fileno())
PY
  # Exercise filesystem zero-range handling while the tracker remains active.
  fallocate --zero-range --offset 1048576 --length 1048576 "$source_mount/payload/large.bin"
  sync
  manifest "$source_mount" "$results/$fs-incremental-expected-files.sha256"
  active_id=$("$CLI" snapshot_create --device "$device" --file "$out/diff" --limit 1G)
  [[ $active_id =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]]
  "$CLI" snapshot_take --id "$active_id"
  printf '%s\n' "$active_id" > "$results/$fs-incremental-snapshot-id.txt"
  info incremental-before
  snapshot=$("$CLI" snapshot_info --device "$device" --field image)
  [[ $snapshot == /dev/* && -b $snapshot && $(blockdev --getsize64 "$snapshot") == 536870912 ]]
  "$CLI" cbtinfo --device "$device" > "$results/$fs-incremental.info"
  "$CLI" readcbtmap --device "$device" --file "$results/$fs-incremental.map"
  dd if="$snapshot" of="$out/incremental-oracle.img" bs=1M status=none
  # Overwrite a block known to be selected by the increment; reconstruction
  # must still obtain the earlier contents from the external difference store.
  python3 - "$source_mount/payload/change.bin" <<'PY'
import os, sys
with open(sys.argv[1], 'r+b') as f:
    f.write(b'live-after-incremental' * 128)
    f.flush()
    os.fsync(f.fileno())
PY
  sync
  # Independently decode the public byte-map format and reconstruct from baseline.
  python3 - "$results" "$fs" "$out" "$snapshot" <<'PY'
import hashlib, json, os, re, shutil, sys
from pathlib import Path
results, fs, out, snapshot = sys.argv[1:]
r, o = Path(results), Path(out)
def info(label):
    fields = {}
    for line in (r/f'{fs}-{label}.info').read_text().splitlines():
        k, v = line.split('=', 1)
        if k in fields: raise ValueError('duplicate CBT field')
        fields[k] = v
    assert set(fields) == {'block_size','device_capacity','block_count','generation_id','changes_number'}
    assert re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', fields['generation_id'])
    for k in ('block_size','device_capacity','block_count','changes_number'): fields[k] = int(fields[k])
    return fields
pre, first, current = info('before'), info('full'), info('incremental')
assert pre['changes_number'] == 0 and first['changes_number'] == 1 and current['changes_number'] == 2
for k in ('generation_id','block_size','block_count','device_capacity'):
    assert pre[k] == first[k] == current[k], f'CBT {k} changed'
size, block = current['device_capacity'], current['block_size']
assert size == 536870912 and block >= 512 and block & (block-1) == 0
assert current['block_count'] == (size+block-1)//block
bitmap = (r/f'{fs}-incremental.map').read_bytes()
baseline_map = (r/f'{fs}-full.map').read_bytes()
assert len(bitmap) == len(baseline_map) == current['block_count']
assert all(v <= 1 for v in baseline_map) and all(v <= 2 for v in bitmap)
assert o.joinpath('full.img').stat().st_size == size
shutil.copyfile(o/'full.img', o/'incremental.img')
read_bytes = 0
with open(snapshot, 'rb', buffering=0) as src, open(o/'incremental.img', 'r+b', buffering=0) as dst:
    for n, epoch in enumerate(bitmap):
        if epoch <= first['changes_number']: continue
        offset = n * block
        length = min(block, size-offset)
        data = os.pread(src.fileno(), length, offset)
        assert len(data) == length
        assert os.pwrite(dst.fileno(), data, offset) == length
        read_bytes += length
    os.fsync(dst.fileno())
assert 0 < read_bytes < size, 'incremental must read a proper nonempty subset'
(r/f'{fs}-incremental-metrics.json').write_text(json.dumps({'source_read_bytes':read_bytes,'size':size,'block_size':block,'generation_id':current['generation_id'],'sequence':2}, indent=2)+'\n')
PY
  info incremental-after
  "$CLI" readcbtmap --device "$device" --file "$results/$fs-incremental-after-read.map"
  cmp "$results/$fs-incremental.map" "$results/$fs-incremental-after-read.map"
  "$CLI" cbtinfo --device "$device" > "$results/$fs-incremental-after-read.info"
  cmp "$results/$fs-incremental.info" "$results/$fs-incremental-after-read.info"
  validate_image incremental "$out/incremental.img" "$out/incremental-oracle.img" "$results/$fs-incremental-expected-files.sha256"
  "$CLI" snapshot_destroy --id "$active_id"
  active_id=''
  "$CLI" cbtinfo --device "$device" > "$results/$fs-after-destroy.info"
  cmp "$results/$fs-incremental.info" "$results/$fs-after-destroy.info"
  # The experiment is over for this source; detach only after both snapshots.
  "$CLI" detach --device "$device"
  umount "$source_mount"
  source_mount=''
  printf '%s: full and CBT incremental raw equality, filesystem and files passed\n' "$fs" | tee -a "$results/summary.txt"
done
printf 'Pinned blksnap Ubuntu24.04 GA6.8 ext4/XFS external-COW checks passed.\n' > "$results/SUCCESS"
