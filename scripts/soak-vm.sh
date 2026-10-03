#!/bin/bash
# Soak test, run on the test host as root (scripts/soak.sh drives it). For DURATION seconds:
# continuous verified I/O on four devices, a random chaos action every 1-3 minutes on three
# of them (SIGKILL, reload, origin outage, page cache drop), and a resource sample every
# minute. sk-leak is never disturbed: its memory, descriptors and threads must stay flat.
# Ends with "SOAK OK" or "SOAK FAIL: reason" in $dir/soak.log.
set -uo pipefail
duration=${1:?usage: soak-vm.sh SECONDS}
me="$(cd "$(dirname "$0")" && pwd)"
rangehttpd="$me/../bin/rangehttpd"
dir=/var/tmp/blkmap-soak
port=18300
log=$dir/soak.log
csv=$dir/metrics.csv
pids=()

say() { echo "$(date -u +%FT%TZ) $*" | tee -a $log; }
fail() { say "SOAK FAIL: $*"; touch $dir/failed; }
unit() { systemctl "$1" "blkmap@$2" >/dev/null 2>&1; }
wait_dev() { for _ in $(seq 1 300); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
pid_of() { systemctl show -p MainPID --value blkmap@$1; }
origin() { "$rangehttpd" $dir 127.0.0.1:$port >/dev/null 2>&1 & echo $! > $dir/origin.pid; sleep 0.3; }
cfg() { cat > /etc/blkmap/$1.yml; rm -f /var/lib/blkmap/$1.cow /var/lib/blkmap/$1.cow.bitmap /run/blkmap/$1.bitmap; }

cleanup() {
  for p in "${pids[@]:-}"; do [ -n "$p" ] && kill $p 2>/dev/null; done
  pkill -x fio; sleep 1
  umount $dir/mnt 2>/dev/null
  for id in sk-fs sk-http sk-raw sk-leak; do systemctl reset-failed blkmap@$id 2>/dev/null; unit stop $id; done
  kill "$(cat $dir/origin.pid 2>/dev/null)" 2>/dev/null
}

# --- setup -------------------------------------------------------------------------------
cleanup 2>/dev/null; rm -rf $dir; mkdir -p $dir/mnt /etc/blkmap
: > $log
head -c 256M /dev/urandom > $dir/img
cp $dir/img $dir/raw.img
origin
cfg sk-fs <<YML
size: 2G
segments:
  - type: zero
    size: 2G
YML
cfg sk-http <<YML
segments:
  - type: http
    url: http://127.0.0.1:$port/img
hydrate:
  rate: 1M
  report-every: 5m
YML
cfg sk-raw <<YML
segments:
  - type: file
    path: $dir/raw.img
YML
cfg sk-leak <<YML
size: 512M
segments:
  - type: zero
    size: 512M
YML
for id in sk-fs sk-http sk-raw sk-leak; do unit start $id && wait_dev $id || { fail "$id did not start"; exit 1; }; done
mkfs.ext4 -q -F /dev/blkmap/sk-fs && mount /dev/blkmap/sk-fs $dir/mnt || { fail "mkfs/mount"; exit 1; }
say "soak started: $duration s, kernel $(uname -r)"

# --- workloads (each restarts itself until the deadline; any verify failure is fatal) ----
end=$(( $(date +%s) + duration ))
fio_loop() { # NAME ARGS...: back-to-back 5 minute verified runs
  local name=$1; shift
  while [ $(date +%s) -lt $end ] && [ ! -e $dir/failed ]; do
    # A steady, realistic rate: the soak is about time and chaos, not throughput (stress
    # covers that), and an unthrottled writer starves a shared host's disk until the VM
    # itself stalls, which then measures the host instead of blkmap
    fio --name=$name --ioengine=io_uring --iodepth=8 --bs=4k --rw=randwrite --runtime=300 --time_based --rate_iops=150 \
      --verify=crc32c --verify_backlog=256 --verify_fatal=1 "$@" --output=$dir/fio-$name.out >/dev/null 2>&1 \
      || { [ $(date +%s) -lt $end ] && fail "fio $name: $(grep -m1 -iE 'verify|error' $dir/fio-$name.out)"; }
  done
}
fio_loop fs --directory=$dir/mnt --size=256M & pids+=($!)
fio_loop raw --filename=/dev/blkmap/sk-raw --direct=1 --size=128M & pids+=($!)
fio_loop leak --filename=/dev/blkmap/sk-leak --direct=1 --size=256M & pids+=($!)
( # random 1M reads of the HTTP device must match the image
  while [ $(date +%s) -lt $end ] && [ ! -e $dir/failed ]; do
    n=$(( RANDOM % 256 ))
    if dd if=/dev/blkmap/sk-http bs=1M skip=$n count=1 iflag=direct of=$dir/http.blk status=none 2>/dev/null; then
      cmp -s $dir/http.blk <(dd if=$dir/img bs=1M skip=$n count=1 status=none) || fail "sk-http block $n differs from the image"
    fi
    sleep 0.2
  done
) & pids+=($!)

# --- chaos ---------------------------------------------------------------------------------
(
  while [ $(date +%s) -lt $end ] && [ ! -e $dir/failed ]; do
    sleep $(( 60 + RANDOM % 120 ))
    [ $(date +%s) -lt $(( end - 60 )) ] || break
    id=$(shuf -n1 -e sk-fs sk-http sk-raw)
    case $(( RANDOM % 4 )) in
      0) systemctl reset-failed blkmap@$id; p=$(pid_of $id); [ "${p:-0}" -gt 0 ] && kill -9 $p; say "chaos: SIGKILL $id ($p)" ;;
      1) systemctl reload blkmap@$id && say "chaos: reload $id" || fail "reload $id failed" ;;
      2) kill "$(cat $dir/origin.pid)" 2>/dev/null; say "chaos: origin down"; sleep $(( 15 + RANDOM % 25 )); origin; say "chaos: origin up" ;;
      3) sync -f $dir/mnt; echo 3 > /proc/sys/vm/drop_caches; say "chaos: dropped page caches" ;;
    esac
    # Whatever happened, every device must be served again within a minute
    for d in sk-fs sk-http sk-raw sk-leak; do
      for _ in $(seq 1 600); do [ "$(systemctl is-active blkmap@$d)" = active ] && [ -e /dev/blkmap/$d ] && break; sleep 0.1; done
      [ "$(systemctl is-active blkmap@$d)" = active ] || fail "$d not served a minute after the last chaos action"
    done
  done
) & pids+=($!)

# --- monitor ---------------------------------------------------------------------------
echo "time,device,pid,rss_kb,fds,threads,written,errors" > $csv
while [ $(date +%s) -lt $end ] && [ ! -e $dir/failed ]; do
  for d in sk-fs sk-http sk-raw sk-leak; do
    p=$(pid_of $d)
    [ "${p:-0}" -gt 0 ] || continue
    rss=$(awk '/VmRSS/{print $2}' /proc/$p/status 2>/dev/null); thr=$(awk '/Threads/{print $2}' /proc/$p/status 2>/dev/null)
    fds=$(ls /proc/$p/fd 2>/dev/null | wc -l)
    st=$(blkmap status --json $d 2>/dev/null)
    w=$(grep -o '"written":[0-9]*' <<<"$st" | cut -d: -f2); e=$(grep -o '"errors":[0-9]*' <<<"$st" | head -1 | cut -d: -f2)
    echo "$(date +%s),$d,$p,${rss:-},$fds,${thr:-},${w:-},${e:-}" >> $csv
  done
  sleep 60
done

# --- verdict -----------------------------------------------------------------------------
for p in "${pids[@]}"; do wait $p 2>/dev/null; done
pids=()
if [ ! -e $dir/failed ]; then
  umount $dir/mnt && fsck.ext4 -f -n /dev/blkmap/sk-fs >/dev/null 2>&1 || fail "sk-fs: unmount or fsck not clean"
  # sk-leak ran undisturbed all along: compare its last sample with the one at 10 minutes
  awk -F, -v t0="$(head -2 $csv | tail -1 | cut -d, -f1)" '$2=="sk-leak" && $1-t0>=600 && !base {base=1; r=$4; f=$5; h=$6} $2=="sk-leak" {lr=$4; lf=$5; lh=$6} END {
      if (!base) {print "too short to judge"; exit 0}
      printf "sk-leak at 10 min: rss %d kB, %d fds, %d threads; at the end: rss %d kB, %d fds, %d threads\n", r, f, h, lr, lf, lh
      if (lr > r*1.5 + 20480 || lf > f + 10 || lh > h + 20) exit 1 }' $csv | tee -a $log || fail "sk-leak resources grew"
fi
kills=$(grep -c 'chaos: SIGKILL' $log); reloads=$(grep -c 'chaos: reload' $log); outages=$(grep -c 'chaos: origin down' $log); drops=$(grep -c 'chaos: dropped' $log)
say "chaos actions: $kills kills, $reloads reloads, $outages origin outages, $drops cache drops"
cleanup
[ -e $dir/failed ] || say "SOAK OK"
