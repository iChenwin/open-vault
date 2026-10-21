#!/usr/bin/env bash
# Application acceptance only inside the disposable QEMU guest. Never format.
set -Eeuo pipefail
umask 077
[[ $EUID == 0 && $(uname -m) == x86_64 ]] || exit 1
virt=$(systemd-detect-virt)
[[ $virt == qemu || $virt == kvm ]] || exit 1
[[ $(cat /sys/class/dmi/id/product_name) == *QEMU* || $(cat /sys/class/dmi/id/sys_vendor) == QEMU ]] || exit 1
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -r) =~ ^6\.8\.0-[0-9]+-generic$ ]] || exit 1
results=/home/lab/results
[[ -s $results/SUCCESS && ! -e $results/APP_SUCCESS ]] || { echo 'Successful primitive acceptance must precede a fresh application run.' >&2; exit 1; }
exec > >(tee "$results/app-test.log") 2>&1
export OPEN_VAULT_CBT_LAB=1
if ! command -v jq >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends jq
fi
APP=/usr/local/bin/open-vault-cbt
CLI=/usr/local/bin/blksnap
[[ -f /home/lab/payload/open-vault-cbt && ! -L /home/lab/payload/open-vault-cbt ]]
install -m 0755 /home/lab/payload/open-vault-cbt "$APP"
sha256sum "$APP" > "$results/app-binary.sha256"
[[ $($CLI version) == 7.0.0.0 && -d /sys/module/bdevfilter && -d /sys/module/veeamblksnap ]]
source_mount='' check_mount=''
cleanup() {
  [[ -z $check_mount ]] || umount "$check_mount" || true
  [[ -z $source_mount ]] || umount "$source_mount" || true
  dmesg > "$results/app-dmesg.log" || true
}
trap cleanup EXIT

find_disk() {
  local serial=$1 expected_fs=$2 disk found='' count=0
  for disk in /sys/class/block/vd*; do
    [[ -f $disk/serial ]] || continue
    if [[ $(cat "$disk/serial") == "$serial" ]]; then found=/dev/$(basename "$disk"); count=$((count + 1)); fi
  done
  [[ $count == 1 && -b $found && $(blockdev --getsize64 "$found") == 536870912 ]] || return 1
  [[ ! -e /sys/class/block/$(basename "$found")/partition && $(lsblk -nr -o NAME "$found" | wc -l) == 1 ]] || return 1
  ! lsblk -nr -o MOUNTPOINT "$found" | grep -q '[^[:space:]]' || return 1
  [[ -z $(ls -A "/sys/class/block/$(basename "$found")/holders") ]] || return 1
  [[ $(blkid -s TYPE -o value "$found") == "$expected_fs" ]] || return 1
  [[ $(lsblk -dn -o MAJ:MIN "$found") != "$(findmnt -n -o MAJ:MIN -T /)" ]] || return 1
  if "$CLI" cbtinfo --device "$found" >/dev/null 2>&1; then
    echo 'Primitive tracker was not detached; refusing to adopt it.' >&2; return 1
  fi
  printf '%s\n' "$found"
}
manifest() {
  (cd "$1/payload" && find . -type f -print0 | sort -z | xargs -0 -r sha256sum) > "$2"
}
validate_generation() {
  local label=$1 generation=$2
  local restored=$out/$label-restored.img oracle=$out/$label-oracle.img
  local inspection=$restored
  "$APP" -config "$config" -generation "$generation" verify > "$results/app-$fs-$label-verify.json"
  "$APP" -config "$config" -generation "$generation" -target "$restored" restore > "$results/app-$fs-$label-restore.json"
  sha256sum "$oracle" "$restored" > "$results/app-$fs-$label-image-hashes.txt"
  cmp -- "$oracle" "$restored"
  if [[ $fs == ext4 ]]; then
    e2fsck -fn "$restored" > "$results/app-$fs-$label-fsck.log" 2>&1
  else
    # Log replay is required for a valid frozen XFS image. Never modify the
    # restored/raw comparison image; inspect a disposable copy instead.
    inspection=$out/$label-inspection.img
    cp --reflink=auto -- "$restored" "$inspection"
    check_mount=$out/check-$label-replay
    mkdir -p "$check_mount"
    mount -o loop,nouuid "$inspection" "$check_mount"
    umount "$check_mount"
    check_mount=''
    xfs_repair -n -f "$inspection" > "$results/app-$fs-$label-fsck.log" 2>&1
  fi
  check_mount=$out/check-$label
  mkdir -p "$check_mount"
  if [[ $fs == ext4 ]]; then mount -o loop,ro,noload "$restored" "$check_mount";
  else mount -o loop,ro,norecovery,nouuid "$inspection" "$check_mount"; fi
  manifest "$check_mount" "$results/app-$fs-$label-restored-files.sha256"
  diff -u "$results/app-$fs-$label-expected-files.sha256" "$results/app-$fs-$label-restored-files.sha256"
  umount "$check_mount"
  check_mount=''
}
assert_finalized() {
  local label=$1 expected_sequence=$2
  "$CLI" cbtinfo --device "$device" > "$results/app-$fs-$label-tracker.info"
  [[ $("$CLI" snapshot_info --device "$device" --field error_code) == 0 ]]
  [[ $("$CLI" snapshot_info --device "$device" --field image) == /dev/ ]]
  python3 - "$results/app-$fs-$label-backup.json" "$results/app-$fs-$label-tracker.info" "$state/blksnap/tracker.json" "$expected_sequence" <<'PY'
import json, sys
from pathlib import Path
backup, tracker, journal, sequence = sys.argv[1:]
g = json.loads(Path(backup).read_text())['generation']
info = dict(line.split('=', 1) for line in Path(tracker).read_text().splitlines())
entry = json.loads(Path(journal).read_text())['entry']
assert int(info['changes_number']) == g['sequence'] == int(sequence)
assert info['generation_id'].replace('-', '') == g['tracker_uuid']
assert int(info['device_capacity']) == g['size'] == 536870912
assert entry['current']['mode'] == 'incremental' and entry['current']['snapshot_device'] == ''
assert entry['snapshot_id'] == '' and not entry.get('pending')
assert entry['current']['uuid'] == g['tracker_uuid'] and entry['current']['sequence'] == g['sequence']
PY
}

# Resolve and validate BOTH existing fixture filesystems before mounting either.
ext4_device=$(find_disk OPENVAULT_CBT_EXT4 ext4)
xfs_device=$(find_disk OPENVAULT_CBT_XFS xfs)
[[ $ext4_device != "$xfs_device" ]]
printf 'ext4=%s xfs=%s\n' "$ext4_device" "$xfs_device" > "$results/app-test-devices.txt"
for fs in ext4 xfs; do
  if [[ $fs == ext4 ]]; then device=$ext4_device; else device=$xfs_device; fi
  out=/var/lib/open-vault-blksnap-app/$fs
  [[ ! -e $out ]] || { echo 'Application fixture state already exists.' >&2; exit 1; }
  state=$out/state
  mkdir -p "$out/diff" "$out/repository" "$state/blksnap"
  [[ $(findmnt -n -o MAJ:MIN -T "$out") == "$(findmnt -n -o MAJ:MIN -T /)" ]]
  for fixture in "$ext4_device" "$xfs_device"; do [[ $(lsblk -dn -o MAJ:MIN "$fixture") != "$(findmnt -n -o MAJ:MIN -T "$out")" ]]; done
  [[ $(cat "/sys/class/block/$(basename "$device")/queue/discard_max_bytes") == 0 ]]
  source_mount=/mnt/open-vault-blksnap-app-$fs
  mkdir -p "$source_mount"
  mount -o nodiscard "$device" "$source_mount"
  mkdir -p "$source_mount/payload/app"
  python3 - "$source_mount/payload/app" "$out/key" <<'PY'
from pathlib import Path
import os, sys
p, key = map(Path, sys.argv[1:])
(p/'large.bin').write_bytes(bytes(range(256)) * 65536)
(p/'change.bin').write_bytes(b'A' * 102400)
(p/'delete.txt').write_text('application delete fixture\n')
(p/'rename.txt').write_text('application rename fixture\n')
with key.open('xb') as f: f.write(os.urandom(32).hex().encode()+b'\n')
key.chmod(0o600)
PY
  config=$out/app.json
  # The private configuration contains only a key pathname; never publish it
  # or the key contents with the acceptance artifacts.
  python3 - "$config" "$out" "$device" "$source_mount" "$fs" <<'PY'
import json, sys
from pathlib import Path
config, out, device, mount, fs = sys.argv[1:]
Path(config).write_text(json.dumps({'provider':'blksnap', 'blksnap':'/usr/local/bin/blksnap', 'difference_limit':'1G',
    'repository':out+'/repository', 'key_file':out+'/key', 'state_dir':out+'/state',
    'volume':{'id':'app-'+fs, 'device':device, 'mount_point':mount, 'cow_dir':out+'/diff', 'minor':0}}, indent=2))
Path(config).chmod(0o600)
PY
  "$APP" -config "$config" diagnose > "$results/app-$fs-diagnose.json"
  jq -e '.provider == "blksnap" and .supported == true and .driver_loaded == true and ((.problems // []) | length == 0)' "$results/app-$fs-diagnose.json"
  sync
  manifest "$source_mount" "$results/app-$fs-full-expected-files.sha256"
  "$APP" -config "$config" -oracle-target "$out/full-oracle.img" backup > "$results/app-$fs-full-backup.json"
  full=$(jq -er '.generation.id | select(test("^[a-f0-9]{32}$"))' "$results/app-$fs-full-backup.json")
  jq -e '.generation.size == 536870912 and .generation.sequence == 1 and .this_run_source_read_bytes == .generation.size and .oracle_read_bytes == .generation.size and (.generation.parent // "") == ""' "$results/app-$fs-full-backup.json"
  assert_finalized full 1
  validate_generation full "$full"

  python3 - "$source_mount/payload/app" <<'PY'
from pathlib import Path
import os, sys
p = Path(sys.argv[1])
(p/'change.bin').write_bytes(b'B' * 102400)
(p/'new.bin').write_bytes(b'N' * 65536)
(p/'delete.txt').unlink()
(p/'rename.txt').rename(p/'renamed.txt')
with (p/'large.bin').open('r+b') as f:
    f.seek(8 * 1024 * 1024)
    f.write(b'application tracked patch' * 128)
    f.flush()
    os.fsync(f.fileno())
PY
  fallocate --zero-range --offset 1048576 --length 1048576 "$source_mount/payload/app/large.bin"
  sync
  manifest "$source_mount" "$results/app-$fs-incremental-expected-files.sha256"
  "$APP" -config "$config" -oracle-target "$out/incremental-oracle.img" backup > "$results/app-$fs-incremental-backup.json"
  incremental=$(jq -er '.generation.id | select(test("^[a-f0-9]{32}$"))' "$results/app-$fs-incremental-backup.json")
  jq -e --arg parent "$full" '.generation.parent == $parent and .generation.sequence == 2 and .this_run_source_read_bytes > 0 and .this_run_source_read_bytes < .generation.size and .tracking_index_read_bytes > 0 and .oracle_read_bytes == .generation.size' "$results/app-$fs-incremental-backup.json"
  assert_finalized incremental 2
  validate_generation incremental "$incremental"
  "$APP" -config "$config" -generation "$full" verify > "$results/app-$fs-full-verify-after-incremental.json"
  "$APP" -config "$config" generations > "$results/app-$fs-generations.json"
  jq -e '.generations | length == 2' "$results/app-$fs-generations.json"
  "$CLI" detach --device "$device"
  umount "$source_mount"
  source_mount=''
  # Repository verification must also work after the source is unmounted.
  "$APP" -config "$config" -generation "$incremental" verify > "$results/app-$fs-offline-verify.json"
  printf '%s: Go CLI full/incremental restore, raw equality, fsck, files, retained tracker and read reduction passed\n' "$fs" | tee -a "$results/app-summary.txt"
done
printf 'Go CLI blksnap ext4/XFS full and incremental acceptance passed.\n' > "$results/APP_SUCCESS"
