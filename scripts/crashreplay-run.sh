#!/bin/bash
# The machine side of crashreplay.sh (root, blkmap installed, binaries in $bin). Records a
# write workload on a blkmap device whose COW file and bitmap sit on an ext4 filesystem on
# dm-log-writes, then rebuilds every logged crash state of that filesystem and checks that
# a blkmap device started from it (no live bitmap: a power loss empties /run) still holds
# every record the writer saw acknowledged, and reads as the base everywhere else.
# Usage: crashreplay-run.sh [RECORDS] [STEP]
set -uo pipefail
records=${1:-3000}
step=${2:-1}
bin=/root/blkmap-test/bin
w=/var/tmp/blkmap-crash
id=crw
chk=crc
data="" logdev="" loop=""

wait_dev() { for _ in $(seq 1 100); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
config() { # config ID COWFILE: a random base, so a copy-up lost in a crash shows
  printf 'cow:\n  file: %s\nsegments:\n  - type: file\n    path: %s\n' "$2" $w/base.img > /etc/blkmap/$1.yml
}
cleanup() {
  systemctl stop blkmap@$id blkmap@$chk 2>/dev/null
  umount $w/f $w/chk 2>/dev/null
  dmsetup remove lw 2>/dev/null
  for l in $data $logdev $loop; do losetup -d $l 2>/dev/null; done
  rm -f /etc/blkmap/$id.yml /etc/blkmap/$chk.yml /run/blkmap/$id.* /run/blkmap/$chk.*
}
trap cleanup EXIT
cleanup
rm -rf $w && mkdir -p $w/f $w/chk

# Record: the filesystem is made before logging starts, so start.img is where replay begins
truncate -s 256M $w/data.img && mkfs.ext4 -q -F $w/data.img && cp --sparse=always $w/data.img $w/start.img
truncate -s 4G $w/log.img
head -c 64M /dev/urandom > $w/base.img
data=$(losetup -f --show $w/data.img) && logdev=$(losetup -f --show $w/log.img) || exit 1
dmsetup create lw --table "0 $(blockdev --getsz $data) log-writes $data $logdev" || exit 1
mount /dev/mapper/lw $w/f || exit 1
config $id $w/f/d.cow
systemctl start blkmap@$id && wait_dev $id || { echo "FAIL: the recorded device did not start"; exit 1; }
$bin/powercut write /dev/blkmap/$id -mark lw -count "$records" > $w/writer.out || { echo "FAIL: writer"; cat $w/writer.out; exit 1; }
systemctl stop blkmap@$id
umount $w/f && dmsetup remove lw && losetup -d $data $logdev
data="" logdev=""
start=$(awk '/^start /{print $2; exit}' $w/writer.out)
echo "recorded $records records ($(grep -c '^ack ' $w/writer.out) acknowledged flushes), log $(du -h --apparent-size $w/log.img | cut -f1) apparent, $(du -h $w/log.img | cut -f1) used"

# Replay: master.img advances through the flushes; each crash state is a copy of it plus the
# FUA writes and a random part of the plain writes logged before the next flush
$bin/logreplay plan $w/log.img "$step" > $w/plan || exit 1
total=$(wc -l < $w/plan)
echo "checking $total crash states (every ${step}th flush)"
cp --sparse=always $w/start.img $w/master.img
applied=0 n=0 failed=0 verified=0
keeps=(0 0.5 1)
config $chk $w/chk/d.cow
while read -r q p ack; do
  n=$((n + 1))
  if [ $((p + 1)) -gt $applied ]; then
    $bin/logreplay apply $w/log.img $w/master.img $applied $((p + 1)) || exit 1
    applied=$((p + 1))
  fi
  keep=${keeps[$((n % 3))]}
  cp --sparse=always $w/master.img $w/chk.img
  $bin/logreplay apply $w/log.img $w/chk.img $applied "$q" "$keep" "$q" || exit 1
  what="state $n (before entry $q, after $p, keep $keep, acked through ${ack})"
  loop=$(losetup -f --show $w/chk.img)
  if ! mount $loop $w/chk 2>/dev/null; then
    echo "FAIL: $what: the cow filesystem does not mount"; failed=$((failed + 1)); losetup -d $loop; loop=""; continue
  fi
  rm -f /run/blkmap/$chk.*
  if ! { systemctl start blkmap@$chk && wait_dev $chk; }; then
    echo "FAIL: $what: blkmap did not start"; journalctl -u blkmap@$chk -n 5 --no-pager -o cat; failed=$((failed + 1))
  elif [ "$ack" -ge "$start" ]; then
    if out=$($bin/powercut verify /dev/blkmap/$chk -base $w/base.img "$start-$ack" 2>&1); then
      verified=$((verified + 1))
    else
      echo "FAIL: $what: $out"; failed=$((failed + 1))
    fi
  fi
  systemctl stop blkmap@$chk
  umount $w/chk
  e2fsck -fn $loop >/dev/null 2>&1 || { echo "FAIL: $what: the cow filesystem is inconsistent after the check"; failed=$((failed + 1)); }
  losetup -d $loop; loop=""
  [ $((n % 50)) = 0 ] && echo "  $n/$total states, $failed failed"
  [ $failed -ge 10 ] && { echo "stopping after 10 failures"; break; }
done < $w/plan
if [ $failed = 0 ]; then
  echo "CRASHREPLAY OK: $n crash states, $verified with acknowledged records, all survived"
else
  echo "CRASHREPLAY FAIL: $failed of $n crash states"
  exit 1
fi
