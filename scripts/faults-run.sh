#!/bin/bash
# The machine side of faults.sh (root, blkmap installed, binaries in $bin). Puts the COW
# filesystem on device-mapper, injects faults under it while a writer stores checksummed
# records over a random base, and checks that every acknowledged record survives and every
# other slot still reads as the base. Scenarios (default: all):
#   writeback: writes fail (dm-flakey error_writes) on ext4 without a journal, so only the
#              COW file's own writeback fails: a failed fsync loses its pages, so no bitmap
#              bit may ever describe them; the server restarts from the last flushed state.
#   journaled: the same on ext4 with a journal, which aborts and goes read-only: the server
#              crash-loops until the reap fails the device's I/O (nothing may hang); after a
#              fsck and a start everything acknowledged is there.
#   readerr:   a bad block (a dm "error" segment) under the COW file: reads of it fail, never
#              zeros or base bytes, the device keeps serving, and reads are right again once
#              the blocks are good.
# Usage: faults-run.sh [writeback|journaled|readerr]...
set -uo pipefail
bin=/root/blkmap-test/bin
w=/var/tmp/blkmap-faults
id=flt
loop=""
failed=0
scenarios=("${@:-writeback journaled readerr}")
scenarios=(${scenarios[*]})

wait_dev() { for _ in $(seq 1 100); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
table() { # table linear|error: swap what backs the COW filesystem
  local t="0 $sectors linear $loop 0"
  [ "$1" = error ] && t="0 $sectors flakey $loop 0 0 3600 1 error_writes"
  sync -f $w/f 2>/dev/null; dmsetup suspend --nolockfs cowfs && dmsetup load cowfs --table "$t" && dmsetup resume cowfs
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
teardown() {
  systemctl stop blkmap@$id 2>/dev/null
  umount $w/f 2>/dev/null
  dmsetup remove cowfs 2>/dev/null
  [ -n "$loop" ] && losetup -d $loop 2>/dev/null
  losetup -j $w/cowfs.img 2>/dev/null | cut -d: -f1 | xargs -r losetup -d
  rm -f /etc/blkmap/$id.yml /run/blkmap/$id.*
  systemctl reset-failed blkmap@$id blkmap-reap@$id 2>/dev/null
  loop=""
}
setup() { # setup MKFS-OPTIONS...: a fresh COW filesystem on a linear dm device
  teardown
  rm -rf $w && mkdir -p $w/f
  head -c 64M /dev/urandom > $w/base.img
  truncate -s 256M $w/cowfs.img && mkfs.ext4 -q -F "$@" $w/cowfs.img
  loop=$(losetup -f --show $w/cowfs.img) || exit 1
  sectors=$(blockdev --getsz $loop)
  dmsetup create cowfs --table "0 $sectors linear $loop 0" && mount /dev/mapper/cowfs $w/f || exit 1
  printf 'cow:\n  file: %s\nsegments:\n  - type: file\n    path: %s\n' $w/f/d.cow $w/base.img > /etc/blkmap/$id.yml
  t0=$(date +%s)
  systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start"; exit 1; }
  spans=()
}
repair() { # stop, fsck the COW filesystem and start again, as an operator would
  systemctl stop blkmap@$id; umount $w/f
  e2fsck -fy /dev/mapper/cowfs >/dev/null 2>&1; local rc=$?; [ $rc -le 1 ] || echo "  fsck of the COW filesystem: exit $rc"
  systemctl reset-failed blkmap@$id blkmap-reap@$id 2>/dev/null
  mount /dev/mapper/cowfs $w/f && systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start after the repair"; failed=$((failed + 1)); return 1; }
}
write_again() {
  writer 4
  grep -q '^powercut:' $w/writer.out && { echo "FAIL: writes fail after the repair"; failed=$((failed + 1)); }
  systemctl restart blkmap@$id && wait_dev $id || { echo "FAIL: restart"; exit 1; }
  check "after writing again"
}
trap teardown EXIT

sc_writeback() {
  # No journal: a journaled filesystem aborts on the first failed commit (see journaled)
  setup -O ^has_journal -e continue
  # Few records first, so the failing writes land in chunks that still need their copy-up
  writer 3 160
  table error
  writer 10
  grep -q '^powercut:' $w/writer.out || { echo "FAIL: the writer never saw the failure"; failed=$((failed + 1)); }
  sleep 2; table linear
  sleep 8 # the server restarted itself; let things settle
  check "after the disk healed"
  systemctl restart blkmap@$id && wait_dev $id || { echo "FAIL: restart"; exit 1; }
  check "after a restart (the live bitmap is adopted)"
  systemctl stop blkmap@$id; rm -f /run/blkmap/$id.*
  systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: start after power loss"; exit 1; }
  check "after a power loss"
  repair && check "after the repair" && write_again
  journalctl -u blkmap@$id --since "@$t0" --no-pager -o cat | grep -m1 "restarting from" | sed 's/^/  log: /'
}

sc_journaled() {
  setup
  writer 3 160
  table error
  writer 10
  grep -q '^powercut:' $w/writer.out || { echo "FAIL: the writer never saw the failure"; failed=$((failed + 1)); }
  # A metadata change forces a journal commit while writes fail: ext4 aborts, read-only
  touch $w/f/poke; sync -f $w/f 2>/dev/null
  sleep 2; table linear
  # If ext4 aborted, the filesystem stays read-only: the server cannot start, and after the
  # restart limit the reap must fail the device's I/O rather than leave readers hanging
  # (an aborted ext4 refuses writes even where /proc/mounts still says rw)
  if touch $w/f/probe 2>/dev/null; then
    echo "  ext4 did not abort (nothing to commit in the window); the server restarted from the last flushed state"
  elif timeout 60 dd if=/dev/blkmap/$id of=/dev/null bs=4k count=1 skip=$((RANDOM % 16000)) iflag=direct status=none 2>/dev/null; then
    echo "  a read succeeded while the COW filesystem was read-only (served from before the failure)"
  elif [ $? = 124 ]; then
    echo "FAIL: a read hung for a minute while the COW filesystem was read-only"; failed=$((failed + 1))
  else
    echo "PASS reads fail instead of hanging while the COW filesystem is read-only"
  fi
  repair && check "after the repair" && write_again
  journalctl -u blkmap@$id -u blkmap-reap@$id --since "@$t0" --no-pager -o cat | grep -E "restarting from|read-only|reap" | sort -u | head -4 | sed 's/^/  log: /'
}

sc_readerr() {
  setup
  writer 3 160
  sync -f /dev/blkmap/$id; sync -f $w/f
  check "before the bad blocks"
  # A bad block under the COW file's copy of an acknowledged record
  local slots=16384 slot off pblk
  slot=$($bin/powercut slot /dev/blkmap/$id 100)
  off=$((slot * 4096))
  # filefrag -v rows: "N:  LSTART..  LEND:  PSTART..  PEND:  LEN: ..."
  pblk=$(filefrag -v -b4096 $w/f/d.cow | awk -v l=$((off / 4096)) '$1 ~ /^[0-9]+:$/ { s = $2 + 0; e = $3 + 0; if (l >= s && l <= e) { print $4 + l - s; exit } }')
  [ -n "$pblk" ] || { echo "FAIL: no physical block for offset $off"; failed=$((failed + 1)); return; }
  bad() { # bad on|off: a one-block error segment at pblk, or the plain linear table
    local b=$((pblk * 8)) t="0 $sectors linear $loop 0"
    [ "$1" = on ] && t="0 $b linear $loop 0
$b 8 error
$((b + 8)) $((sectors - b - 8)) linear $loop $((b + 8))"
    dmsetup suspend --nolockfs cowfs && dmsetup load cowfs --table "$t" && dmsetup resume cowfs
  }
  bad on
  echo 3 > /proc/sys/vm/drop_caches
  if dd if=/dev/blkmap/$id of=/dev/null bs=4k count=1 skip=$slot iflag=direct status=none 2>/dev/null; then
    echo "FAIL: a read of a bad block under the COW file succeeded"; failed=$((failed + 1))
  else
    echo "PASS a read of a bad block under the COW file fails with an error"
  fi
  dd if=/dev/blkmap/$id of=/dev/null bs=4k count=1 skip=$(( (slot + 64) % slots )) iflag=direct status=none 2>/dev/null \
    && echo "PASS other reads still work" || { echo "FAIL: other reads fail too"; failed=$((failed + 1)); }
  systemctl is-active -q blkmap@$id && echo "PASS the device keeps serving" || { echo "FAIL: the server died"; failed=$((failed + 1)); }
  bad off
  echo 3 > /proc/sys/vm/drop_caches
  check "after the blocks are good again"
}

for sc in "${scenarios[@]}"; do
  echo "== $sc"
  sc_$sc
done
teardown
if [ $failed = 0 ]; then echo "FAULTS OK"; else echo "FAULTS FAIL: $failed checks"; exit 1; fi
