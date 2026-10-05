#!/bin/bash
# The machine side of crashreplay.sh (root, blkmap installed, binaries in $bin). Records a
# workload on a blkmap device whose COW file and bitmap sit on an ext4 filesystem on
# dm-log-writes, then rebuilds logged crash states of that filesystem and checks a blkmap
# device started from each (no live bitmap: a power loss empties /run).
#   records: a writer stores checksummed records over a random base; every acknowledged
#            record must survive and every other slot must still read as the base.
#   fs:      the base is a sparse ext4 image, hydrated in the background; a guest workload
#            creates, fsyncs and deletes files and trims. Each crash state is read once
#            without hydration, then hydrated to the end (marking base holes as zero): the
#            device must read the same, and the guest filesystem must mount, fsck clean and
#            hold every file as its last fsync left it.
#   mirror:  a mirror group (mirrorcrash): m1's base is a live view of m0, nopwrite and
#            reclaim on both; writes reach the plexes in either order and are flushed at
#            different times. In each crash state every write a plex acknowledged with a
#            flush must read back on that plex, whatever its sibling had made durable.
# Usage: crashreplay-run.sh [records|fs|mirror] [N] [CHECKS]: N records or files; CHECKS crash
# states spread evenly over the log (0: every flush).
set -uo pipefail
mode=${1:-records}
n=${2:-3000}
checks=${3:-0}
bin=/root/blkmap-test/bin
w=/var/tmp/blkmap-crash
id=crw
chk=crc
data="" logdev="" loop=""

wait_dev() { for _ in $(seq 1 100); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
config() { # config ID COWFILE [HYDRATE-RATE]: the device over $w/base.img
  printf 'cow:\n  file: %s\nsegments:\n  - type: file\n    path: %s\n' "$2" $w/base.img > /etc/blkmap/$1.yml
  [ -n "${3:-}" ] && printf 'hydrate:\n  rest: true\n  rate: %s\n' "$3" >> /etc/blkmap/$1.yml
  return 0
}
mark() { dmsetup message lw 0 mark "ack$1"; }
content() { head -c "$2" < <(yes "file $1 of the crash replay"); } # content FILE SIZE (no pipe: SIGPIPE under pipefail)
cleanup() {
  umount $w/g 2>/dev/null
  systemctl stop blkmap@$id blkmap@$chk 2>/dev/null
  umount $w/f $w/chk 2>/dev/null
  dmsetup remove lw 2>/dev/null
  for l in $data $logdev $loop; do losetup -d $l 2>/dev/null; done
  rm -f /etc/blkmap/$id.yml /etc/blkmap/$chk.yml /run/blkmap/$id.* /run/blkmap/$chk.*
}
trap cleanup EXIT
cleanup
systemctl reset-failed blkmap@$id blkmap@$chk 2>/dev/null
rm -rf $w && mkdir -p $w/f $w/chk $w/g

# The base: random bytes, or a sparse ext4 image with some files on it
if [ "$mode" = mirror ]; then
  head -c 16M /dev/urandom > $w/base.img
elif [ "$mode" = fs ]; then
  truncate -s 256M $w/base.img && mkfs.ext4 -q -F $w/base.img
  mount -o loop $w/base.img $w/g && head -c 24M /dev/urandom > $w/g/data && mkdir $w/g/dir && umount $w/g || exit 1
else
  head -c 64M /dev/urandom > $w/base.img
fi

# Record: the COW filesystem is made before logging starts, so start.img is where replay begins
truncate -s 512M $w/data.img && mkfs.ext4 -q -F $w/data.img && cp --sparse=always $w/data.img $w/start.img
truncate -s 4G $w/log.img
data=$(losetup -f --show $w/data.img) && logdev=$(losetup -f --show $w/log.img) || exit 1
dmsetup create lw --table "0 $(blockdev --getsz $data) log-writes $data $logdev" || exit 1
mount /dev/mapper/lw $w/f || exit 1
if [ "$mode" = mirror ]; then
  # The group runs in mirrorcrash itself; its live bitmaps go to tmpfs, off the log, so every
  # replayed state is a host reboot
  mkdir -p /run/blkmap-mirror
  $bin/mirrorcrash record -dir $w/f -base $w/base.img -run /run/blkmap-mirror -records "$n" -mark lw -journal $w/journal | sed 's/^/  /' || { echo "FAIL: mirrorcrash record"; exit 1; }
  rm -rf /run/blkmap-mirror
  echo "recorded $n mirror writes, $(grep -c '^A' $w/journal) acknowledgements in $(awk '/^A/{k=$2} END{print k}' $w/journal) flushes"
elif [ "$mode" = fs ]; then config $id $w/f/d.cow 4M; else config $id $w/f/d.cow; fi
[ "$mode" = mirror ] || { systemctl start blkmap@$id && wait_dev $id; } || { echo "FAIL: the recorded device did not start"; exit 1; }
if [ "$mode" = mirror ]; then
  :
elif [ "$mode" = fs ]; then
  # Each fsync is followed by a mark; ops records which mark made which change durable
  mount /dev/blkmap/$id $w/g || exit 1
  m=0
  for i in $(seq 1 "$n"); do
    content $i $(( (i * 7919 % 300 + 1) * 1024 )) > $w/g/dir/f$i && sync $w/g/dir/f$i && sync $w/g/dir || exit 1
    m=$((m + 1)); echo "$m create $i" >> $w/ops; mark $m
    if [ $((i % 10)) = 0 ]; then
      rm $w/g/dir/f$((i - 5)) && sync $w/g/dir || exit 1
      m=$((m + 1)); echo "$m delete $((i - 5))" >> $w/ops; mark $m
    fi
    [ $((i % 25)) = 0 ] && fstrim $w/g
  done
  umount $w/g
  echo "recorded $n files, $m fsynced changes"
else
  $bin/powercut write /dev/blkmap/$id -mark lw -count "$n" > $w/writer.out || { echo "FAIL: writer"; cat $w/writer.out; exit 1; }
  start=$(awk '/^start /{print $2; exit}' $w/writer.out)
  echo "recorded $n records ($(grep -c '^ack ' $w/writer.out) acknowledged flushes)"
fi
[ "$mode" = mirror ] || systemctl stop blkmap@$id
umount $w/f && dmsetup remove lw && losetup -d $data $logdev
data="" logdev=""
echo "log: $(du -h $w/log.img | cut -f1) used"

# check_records ACK / check_fs ACK: verify the device started from a crash state
check_records() {
  [ "$1" -ge "$start" ] || return 0
  $bin/powercut verify /dev/blkmap/$chk -base $w/base.img "$start-$1" 2>&1
}
devsum() { dd if=/dev/blkmap/$chk bs=1M iflag=direct status=none | sha256sum | cut -d' ' -f1; }
check_fs() {
  local out="" i op what before
  # Hydration must not change what the device reads: zero marking must not expose COW data
  # a crash left unclaimed. Read it once without hydration, then restart with it
  before=$(devsum)
  systemctl stop blkmap@$chk; config $chk $w/chk/d.cow 0
  { systemctl start blkmap@$chk && wait_dev $chk; } || { echo "blkmap did not restart with hydration"; return; }
  config $chk $w/chk/d.cow
  for _ in $(seq 1 300); do blkmap status $chk 2>/dev/null | grep -q 'hydration done' && break; sleep 0.1; done
  blkmap status $chk 2>/dev/null | grep -q 'hydration done' || out="hydration did not finish; "
  [ "$(devsum)" = "$before" ] || out="${out}hydration changed what the device reads; "
  mount /dev/blkmap/$chk $w/g 2>/dev/null || { echo "${out}the guest filesystem does not mount"; return; }
  # The state of every file whose last change was made durable by mark ACK or earlier
  declare -A state=() later=()
  while read -r m op i; do
    if [ "$m" -le "$1" ]; then state[$i]=$op; else later[$i]=1; fi
  done < $w/ops
  for i in "${!state[@]}"; do
    [ -n "${later[$i]:-}" ] && continue
    if [ "${state[$i]}" = create ]; then
      cmp -s $w/g/dir/f$i <(content $i $(( (i * 7919 % 300 + 1) * 1024 ))) || out="${out}f$i lost or wrong; "
    elif [ -e $w/g/dir/f$i ]; then
      out="${out}f$i came back after its delete; "
    fi
  done
  umount $w/g
  e2fsck -fn /dev/blkmap/$chk >/dev/null 2>&1 || out="${out}guest fsck found errors"
  echo -n "$out"
}

# Replay: master.img advances through the flushes; each crash state is a copy of it plus the
# FUA writes and a random part of the plain writes logged before the next flush
$bin/logreplay plan $w/log.img > $w/plan.all || exit 1
all=$(wc -l < $w/plan.all)
if [ "$checks" -gt 0 ] && [ "$all" -gt "$checks" ]; then
  awk -v all=$all -v k=$checks 'NR % int(all / k) == 0' $w/plan.all > $w/plan
else
  cp $w/plan.all $w/plan
fi
total=$(wc -l < $w/plan)
echo "checking $total of $all crash states"
cp --sparse=always $w/start.img $w/master.img
applied=0 k=0 failed=0 checked=0
keeps=(0 0.5 1)
config $chk $w/chk/d.cow
while read -r q p ack; do
  k=$((k + 1))
  if [ $((p + 1)) -gt $applied ]; then
    $bin/logreplay apply $w/log.img $w/master.img $applied $((p + 1)) || exit 1
    applied=$((p + 1))
  fi
  keep=${keeps[$((k % 3))]}
  cp --sparse=always $w/master.img $w/chk.img
  $bin/logreplay apply $w/log.img $w/chk.img $applied "$q" "$keep" "$q" || exit 1
  what="state $k (before entry $q, after $p, keep $keep, durable through mark $ack)"
  loop=$(losetup -f --show $w/chk.img)
  if ! mount $loop $w/chk 2>/dev/null; then
    echo "FAIL: $what: the COW filesystem does not mount"; failed=$((failed + 1)); losetup -d $loop; loop=""; continue
  fi
  if [ "$mode" = mirror ]; then
    # Both stores straight from the replayed files, no server: what a reboot finds
    if out=$($bin/mirrorcrash verify -dir $w/chk -base $w/base.img -journal $w/journal -upto "$ack" 2>&1); then
      checked=$((checked + 1))
    else
      echo "FAIL: $what: $out"; failed=$((failed + 1))
    fi
    umount $w/chk
    e2fsck -fn $loop >/dev/null 2>&1 || { echo "FAIL: $what: the COW filesystem is inconsistent after the check"; failed=$((failed + 1)); }
    losetup -d $loop; loop=""
    [ $((k % 50)) = 0 ] && echo "  $k/$total states, $failed failed"
    [ $failed -ge 10 ] && { echo "stopping after 10 failures"; break; }
    continue
  fi
  rm -f /run/blkmap/$chk.*
  if ! { systemctl start blkmap@$chk && wait_dev $chk; }; then
    echo "FAIL: $what: blkmap did not start"; journalctl -u blkmap@$chk -n 5 --no-pager -o cat; failed=$((failed + 1))
  else
    out=$(check_$mode "$ack")
    case "$out" in ""|OK*|verified*) checked=$((checked + 1)) ;; *) echo "FAIL: $what: $out"; failed=$((failed + 1)) ;; esac
  fi
  systemctl stop blkmap@$chk
  umount $w/chk
  e2fsck -fn $loop >/dev/null 2>&1 || { echo "FAIL: $what: the COW filesystem is inconsistent after the check"; failed=$((failed + 1)); }
  losetup -d $loop; loop=""
  [ $((k % 50)) = 0 ] && echo "  $k/$total states, $failed failed"
  [ $failed -ge 10 ] && { echo "stopping after 10 failures"; break; }
done < $w/plan
if [ $failed = 0 ]; then
  echo "CRASHREPLAY OK: $mode, $k crash states, $checked checked, all consistent"
else
  echo "CRASHREPLAY FAIL: $mode, $failed of $k crash states"
  exit 1
fi
