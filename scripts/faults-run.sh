#!/bin/bash
# The machine side of faults.sh (root, blkmap installed, binaries in $bin). Puts the COW
# filesystem on device-mapper and makes its writes fail (dm-flakey error_writes) while a
# writer stores checksummed records over a random base. The writer's flush must fail; once
# the disk works again, and after a restart and a simulated power loss, every acknowledged
# record must be there and every other slot must still read as the base: a COW fsync that
# failed loses its pages, so no bitmap bit may ever describe them. Usage: faults-run.sh
set -uo pipefail
bin=/root/blkmap-test/bin
w=/var/tmp/blkmap-faults
id=flt
loop=""
failed=0

wait_dev() { for _ in $(seq 1 100); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
table() { # table linear|error: swap what backs the COW filesystem
  local t="0 $sectors linear $loop 0"
  [ "$1" = error ] && t="0 $sectors flakey $loop 0 0 3600 1 error_writes"
  sync -f $w/f; dmsetup suspend --nolockfs cowfs && dmsetup load cowfs --table "$t" && dmsetup resume cowfs
}
check() { # check WHAT: verify the device against every acknowledged span so far
  sync -f /dev/blkmap/$id 2>/dev/null; echo 3 > /proc/sys/vm/drop_caches
  if out=$($bin/powercut verify /dev/blkmap/$id -base $w/base.img "${spans[@]}" 2>&1); then
    echo "PASS $1"
  else
    echo "FAIL $1: $out"; failed=$((failed + 1))
  fi
}
writer() { # writer SECONDS [COUNT]: write until killed, failed or done; records the acknowledged span
  $bin/powercut write /dev/blkmap/$id -count ${2:-0} > $w/writer.out 2>&1 & local p=$!
  for _ in $(seq 1 $(($1 * 10))); do kill -0 $p 2>/dev/null || break; sleep 0.1; done
  kill $p 2>/dev/null; wait $p 2>/dev/null
  local first last
  first=$(awk '/^start /{print $2; exit}' $w/writer.out)
  last=$(awk '/^ack /{v=$2} END{print v}' $w/writer.out)
  [ -n "$first" ] && [ -n "$last" ] && spans+=("$first-$last")
  echo "  writer: from ${first:-?} acknowledged through ${last:-nothing}; $(grep -m1 '^powercut:' $w/writer.out || echo 'no error')"
}
cleanup() {
  systemctl stop blkmap@$id 2>/dev/null
  umount $w/f 2>/dev/null
  dmsetup remove cowfs 2>/dev/null
  [ -n "$loop" ] && losetup -d $loop 2>/dev/null
  losetup -j $w/cowfs.img 2>/dev/null | cut -d: -f1 | xargs -r losetup -d
  rm -f /etc/blkmap/$id.yml /run/blkmap/$id.*
}
trap cleanup EXIT
cleanup
systemctl reset-failed blkmap@$id 2>/dev/null
rm -rf $w && mkdir -p $w/f
head -c 64M /dev/urandom > $w/base.img
# No journal: a journaled filesystem aborts and goes read-only on the first failed commit,
# which hides what the COW file's own failed writeback does
truncate -s 256M $w/cowfs.img && mkfs.ext4 -q -F -O ^has_journal -e continue $w/cowfs.img
loop=$(losetup -f --show $w/cowfs.img) || exit 1
sectors=$(blockdev --getsz $loop)
dmsetup create cowfs --table "0 $sectors linear $loop 0" && mount /dev/mapper/cowfs $w/f || exit 1
printf 'cow:\n  file: %s\nsegments:\n  - type: file\n    path: %s\n' $w/f/d.cow $w/base.img > /etc/blkmap/$id.yml
t0=$(date +%s)
systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start"; exit 1; }
spans=()

echo "== writes fail on the COW filesystem mid-run"
# Few records first, so the failing writes land in chunks that still need their copy-up
writer 3 160
table error
writer 10
grep -q '^powercut:' $w/writer.out || { echo "FAIL: the writer never saw the failure"; failed=$((failed + 1)); }
sleep 2
table linear
# Let the flush timer retry against the healed disk
sleep 8
check "after the disk healed"
echo "== restart (the live bitmap is adopted)"
systemctl restart blkmap@$id && wait_dev $id || { echo "FAIL: restart"; exit 1; }
check "after a restart"
echo "== power loss (no live bitmap)"
systemctl stop blkmap@$id; rm -f /run/blkmap/$id.*
systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start after power loss"; exit 1; }
check "after a power loss"
echo "== after the COW filesystem is repaired, the device works again"
# Its own metadata writes failed as well, so it needs a fsck like any filesystem would
systemctl stop blkmap@$id; umount $w/f
e2fsck -fy /dev/mapper/cowfs >/dev/null 2>&1; [ $? -le 1 ] || echo "  fsck of the COW filesystem: exit $?"
mount /dev/mapper/cowfs $w/f && systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start after repair"; exit 1; }
check "after the repair"
writer 4
grep -q '^powercut:' $w/writer.out && { echo "FAIL: writes fail after the repair"; failed=$((failed + 1)); }
systemctl restart blkmap@$id && wait_dev $id || { echo "FAIL: restart"; exit 1; }
check "after writing again"
journalctl -u blkmap@$id --since "@$t0" --no-pager -o cat | grep -E "failed: cow file failed|restarting from" | head -3 | sed 's/^/  log: /'
if [ $failed = 0 ]; then echo "FAULTS OK"; else echo "FAULTS FAIL: $failed checks"; exit 1; fi
