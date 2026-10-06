#!/bin/bash
# Real-life scenarios against an installed blkmap: failures at start, origins that die or
# hang mid-flight, crashes under load and during hydration, restarts, config mistakes, full
# disks, many devices, huge devices, partitions, read-only, unprivileged use. Every scenario
# asserts its outcome and that nothing leaked (kernel devices, processes, mounts), and the
# run continues past failures. Needs root, the deb, fio, and bin/rangehttpd. Usage:
#   scenarios.sh [NAME ...]    (default: all)
set -uo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
rangehttpd=""; for c in "$me/../dist/rangehttpd" "$me/../bin/rangehttpd"; do [ -x "$c" ] && rangehttpd=$c && break; done
[ -x "$rangehttpd" ] || { echo "rangehttpd not found" >&2; exit 1; }
dir=/var/tmp/blkmap-scen
mnt=$dir/mnt
port=18200
pass=0; fail=0; failed=()

log() { echo "    $*"; }
ok() { echo "PASS $1"; }
bad() { echo "FAIL $1: $2"; }
unit() { systemctl "$1" "blkmap@$2" 2>/dev/null; }
wait_dev() { for i in $(seq 1 100); do [ -e /dev/blkmap/$1 ] && return 0; sleep 0.1; done; return 1; }
wait_gone() { for i in $(seq 1 200); do [ -e /dev/blkmap/$1 ] || return 0; sleep 0.1; done; return 1; }
since() { echo "--since=@$(date +%s)"; }
journal() { journalctl -u blkmap@$1 $2 --no-pager -o cat; }
# logged UNIT SINCE PATTERN: journald stores a dying process's last lines a moment later
# (a here-string, not a pipe: grep -q exiting early SIGPIPEs journalctl, which pipefail reports)
logged() { for i in $(seq 1 30); do grep -qi "$3" <<<"$(journal $1 "$2")" && return 0; sleep 0.2; done; return 1; }
cfg() { cat > /etc/blkmap/$1.yml; rm -f /var/lib/blkmap/$1.cow /var/lib/blkmap/$1.cow.bitmap /var/lib/blkmap/$1.rec; }
origin() { # origin PORT [args]: (re)start the http origin on PORT serving $dir
  fuser -k $1/tcp >/dev/null 2>&1 || true; sleep 0.2
  local p=$1; shift; "$rangehttpd" $dir 127.0.0.1:$p "$@" >/dev/null 2>&1 & sleep 0.3
}
baseline=0
leaks() { # after each scenario: no device, process, mount or unit left behind
  # Only serve processes count: udev runs `blkmap udev-name` for a moment on device events
  local p=$(pgrep -fc "blkmap serve"); [ "$p" = 0 ] || { sleep 1; p=$(pgrep -fc "blkmap serve"); }
  local n=$(ls /sys/class/ublk-char | wc -l) m=$(mount | grep -c /dev/ublkb) u=$(systemctl list-units 'blkmap@*' --state=active --no-pager --plain 2>/dev/null | grep -c blkmap)
  [ "$n" = "$baseline" ] && [ "$p" = 0 ] && [ "$m" = 0 ] && [ "$u" = 0 ] && return 0
  echo "    leak: ublk devices $n (baseline $baseline), processes $p, mounts $m, active units $u"
  for id in $(ls /etc/blkmap/ | sed 's/\.yml$//'); do unit stop $id; done
  umount -l $mnt 2>/dev/null; return 1
}
cleanup_all() {
  for id in $(ls /etc/blkmap/ 2>/dev/null | sed 's/\.yml$//'); do unit stop $id; systemctl disable blkmap@$id 2>/dev/null; done
  umount -l $mnt 2>/dev/null; umount -l $dir/small 2>/dev/null
  fuser -k $port/tcp >/dev/null 2>&1; fuser -k $((port+1))/tcp >/dev/null 2>&1
  rm -f /etc/blkmap/sc-*.yml /var/lib/blkmap/sc-*.cow /var/lib/blkmap/sc-*.cow.bitmap /run/blkmap/sc-*
  systemctl reset-failed 'blkmap@*' 2>/dev/null
  sed -i '/blkmap-scen/d' /etc/fstab; systemctl daemon-reload
}
run() { # run NAME: scenario sc_NAME in a subshell with a time limit, verdict from its output
  local name=$1 out=$dir/out.$1; echo "=== $name"
  ( sc_$name ) > $out 2>&1 & local p=$!
  for i in $(seq 1 300); do kill -0 $p 2>/dev/null || break; sleep 1; done
  if kill -0 $p 2>/dev/null; then kill -9 $p 2>/dev/null; echo "FAIL $name: timed out after 300s" >> $out; fi
  cat $out
  local verdict=$(grep -m1 -E '^(PASS|FAIL)' $out)
  case "$verdict" in PASS*) ;; FAIL*) ;; *) verdict="FAIL $name: no verdict"; echo "$verdict";; esac
  if ! leaks; then verdict="FAIL $name: leak"; echo "$verdict"; fi
  case "$verdict" in PASS*) pass=$((pass+1));; *) fail=$((fail+1)); failed+=("${verdict#FAIL }");; esac
  systemctl reset-failed 'blkmap@*' 2>/dev/null
}
# --- scenarios ---------------------------------------------------------------------------
sc_start_stop_cycles() {
  cfg sc-a <<YML
segments:
  - type: file
    path: $dir/img64
YML
  for i in $(seq 1 10); do unit start sc-a && wait_dev sc-a || { bad start_stop_cycles "start $i"; return; }; unit stop sc-a; wait_gone sc-a || { bad start_stop_cycles "stop $i"; return; }; sleep 2.2; done
  ok start_stop_cycles
}
sc_missing_source() {
  cfg sc-m <<YML
segments:
  - type: file
    path: $dir/does-not-exist
YML
  local s=$(since); unit start sc-m && { bad missing_source "start succeeded"; unit stop sc-m; return; }
  logged sc-m "$s" "no such file" && ok missing_source || bad missing_source "no clear error in journal"
}
sc_origin_down_at_start() {
  cfg sc-o <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
YML
  fuser -k $((port+1))/tcp >/dev/null 2>&1
  unit start sc-o && { bad origin_down_at_start "started without origin"; unit stop sc-o; return; }
  origin $((port+1)); systemctl reset-failed blkmap@sc-o
  unit start sc-o && wait_dev sc-o && cmp <(dd if=/dev/blkmap/sc-o bs=1M status=none) $dir/img64 && ok origin_down_at_start || bad origin_down_at_start "did not start once the origin was back"
  unit stop sc-o; fuser -k $((port+1))/tcp >/dev/null 2>&1
}
sc_origin_dies_mid_flight() {
  origin $((port+1))
  cfg sc-d <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
YML
  unit start sc-d && wait_dev sc-d || { bad origin_dies_mid_flight "start"; return; }
  dd if=/dev/blkmap/sc-d bs=1M count=4 iflag=direct of=/dev/null status=none
  fuser -k $((port+1))/tcp >/dev/null 2>&1; sleep 0.3
  local t0=$(date +%s)
  dd if=/dev/blkmap/sc-d bs=1M skip=32 count=1 iflag=direct of=/dev/null status=none 2>/dev/null && { bad origin_dies_mid_flight "read succeeded with the origin dead"; unit stop sc-d; return; }
  log "read failed after $(( $(date +%s) - t0 ))s with the origin down (expected: EIO, quickly)"
  origin $((port+1))
  cmp <(dd if=/dev/blkmap/sc-d bs=1M skip=32 count=8 iflag=direct status=none) <(dd if=$dir/img64 bs=1M skip=32 count=8 status=none) && ok origin_dies_mid_flight || bad origin_dies_mid_flight "reads did not recover after the origin returned"
  unit stop sc-d; fuser -k $((port+1))/tcp >/dev/null 2>&1
}
sc_origin_hangs_then_stop() {
  origin $((port+1)) -delay 90s
  cfg sc-h <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
YML
  # Open probes the origin too, so give it a healthy one first, then swap in the hanging one
  origin $((port+1)); unit start sc-h && wait_dev sc-h || { bad origin_hangs_then_stop "start"; return; }
  origin $((port+1)) -delay 90s
  dd if=/dev/blkmap/sc-h bs=1M skip=40 count=1 iflag=direct of=/dev/null status=none 2>/dev/null & local rd=$!
  sleep 1
  local t0=$(date +%s); unit stop sc-h; local t=$(( $(date +%s) - t0 ))
  wait $rd 2>/dev/null
  log "stop with a read hung on the origin took ${t}s (the hung read is aborted, not waited for)"
  [ $t -le 10 ] && ok origin_hangs_then_stop || bad origin_hangs_then_stop "stop took ${t}s"
  fuser -k $((port+1))/tcp >/dev/null 2>&1; systemctl reset-failed blkmap@sc-h 2>/dev/null
}
sc_kill9_under_write_load() {
  cfg sc-k <<YML
size: 1G
segments:
  - type: zero
    size: 1G
YML
  unit start sc-k && wait_dev sc-k || { bad kill9_under_write_load "start"; return; }
  mkfs.ext4 -q -F /dev/blkmap/sc-k && mount /dev/blkmap/sc-k $mnt
  fio --name=w --ioengine=io_uring --directory=$mnt --rw=randwrite --bs=4k --size=64M --numjobs=2 --iodepth=8 --runtime=15 --time_based --verify=crc32c --do_verify=1 --verify_fatal=1 --output-format=terse >/dev/null 2>&1 & local fp=$!
  sleep 3; local s=$(since)
  # The daemon dies with I/O in flight: the device stays, the I/O waits, systemd restarts
  # the server, it re-attaches, and the filesystem never notices
  kill -9 $(systemctl show -p MainPID --value blkmap@sc-k)
  wait $fp; local rc=$?
  logged sc-k "$s" "re-attached" || { bad kill9_under_write_load "the restarted server did not re-attach"; umount -l $mnt; unit stop sc-k; return; }
  umount $mnt && fsck.ext4 -f -n /dev/blkmap/sc-k >/dev/null 2>&1 && [ $rc = 0 ] && ok kill9_under_write_load || bad kill9_under_write_load "fio $rc or fsck not clean after the crash"
  unit stop sc-k
}
sc_kill9_during_hydration() {
  cfg sc-hy <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  rate: 8M
  report-every: 1s
YML
  local s=$(since); unit start sc-hy && wait_dev sc-hy || { bad kill9_during_hydration "start"; return; }
  sleep 3; kill -9 $(systemctl show -p MainPID --value blkmap@sc-hy)
  for i in $(seq 1 100); do [ "$(systemctl is-active blkmap@sc-hy)" = active ] && [ -e /dev/blkmap/sc-hy ] && break; sleep 0.2; done
  local before=$(journal sc-hy "$s" | grep -m1 'serving' | sed 's/.* \([0-9]*\)\/1024 chunks.*/\1/')
  for i in $(seq 1 60); do logged sc-hy "$s" 'hydration done' && break; sleep 1; done
  local done=$(journal sc-hy "$s" | grep 'hydration done' | tail -1)
  log "after the crash the restart found chunks already hydrated and continued: $done"
  grep -q '1024/1024' <<<"$done" && cmp <(dd if=/dev/blkmap/sc-hy bs=1M status=none) $dir/img64 && ok kill9_during_hydration || bad kill9_during_hydration "hydration did not resume to completion"
  unit stop sc-hy
}
sc_stop_during_hydration() {
  cfg sc-sh <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  rate: 4M
YML
  unit start sc-sh && wait_dev sc-sh || { bad stop_during_hydration "start"; return; }
  sleep 2; local s=$(since) t0=$(date +%s); unit stop sc-sh; local t=$(( $(date +%s) - t0 ))
  [ $t -le 5 ] && ok stop_during_hydration || { bad stop_during_hydration "stop took ${t}s while hydrating"; journal sc-sh "$s" | tail -4 | sed 's/^/    /'; }
}
sc_restart_storm_under_reads() {
  cfg sc-r <<YML
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-r && wait_dev sc-r || { bad restart_storm_under_reads "start"; return; }
  ( for i in $(seq 1 200); do dd if=/dev/blkmap/sc-r bs=1M count=1 skip=$((i%64)) of=/dev/null status=none 2>/dev/null; sleep 0.05; done ) & local rp=$!
  for i in 1 2 3 4; do sleep 2.5; systemctl restart blkmap@sc-r || { bad restart_storm_under_reads "restart $i failed"; kill $rp; unit stop sc-r; return; }; done
  wait $rp
  wait_dev sc-r && cmp <(dd if=/dev/blkmap/sc-r bs=1M status=none) $dir/img64 && ok restart_storm_under_reads || bad restart_storm_under_reads "content wrong after restarts"
  unit stop sc-r
}
sc_two_devices_one_cow() {
  cfg sc-c1 <<YML
cow:
  file: /var/lib/blkmap/sc-shared.cow
segments:
  - type: file
    path: $dir/img64
YML
  cfg sc-c2 <<YML
cow:
  file: /var/lib/blkmap/sc-shared.cow
segments:
  - type: file
    path: $dir/img64
YML
  rm -f /var/lib/blkmap/sc-shared.cow*
  unit start sc-c1 && wait_dev sc-c1 || { bad two_devices_one_cow "first start"; return; }
  local s=$(since)
  if unit start sc-c2; then bad two_devices_one_cow "second device opened the same cow file"; unit stop sc-c2; else logged sc-c2 "$s" "in use\|locked" && ok two_devices_one_cow || bad two_devices_one_cow "refused but without a clear reason"; fi
  # Restart=on-failure keeps retrying the refused unit; it takes the lock once sc-c1 is gone
  unit stop sc-c1; unit stop sc-c2; rm -f /var/lib/blkmap/sc-shared.cow*
}
sc_geometry_change_refused() {
  cfg sc-g <<YML
cow:
  chunk-size: 64K
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-g && wait_dev sc-g && unit stop sc-g || { bad geometry_change_refused "first start"; return; }
  sed -i 's/chunk-size: 64K/chunk-size: 128K/' /etc/blkmap/sc-g.yml
  local s=$(since); unit start sc-g && { bad geometry_change_refused "started with a changed chunk size"; unit stop sc-g; return; }
  logged sc-g "$s" "chunk size mismatch" || { bad geometry_change_refused "no chunk-size message"; return; }
  sed -i "s/chunk-size: 128K/chunk-size: 64K/; s|path: $dir/img64|path: $dir/img32|" /etc/blkmap/sc-g.yml; systemctl reset-failed blkmap@sc-g
  s=$(since); unit start sc-g && { bad geometry_change_refused "started with a changed size"; unit stop sc-g; return; }
  logged sc-g "$s" "device size mismatch" && ok geometry_change_refused || bad geometry_change_refused "no size message"
  unit stop sc-g
}
sc_cow_file_lost() {
  cfg sc-l <<YML
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-l && wait_dev sc-l || { bad cow_file_lost "start"; return; }
  dd if=/dev/urandom of=/dev/blkmap/sc-l bs=1M count=2 oflag=direct status=none; sync -f /var/lib/blkmap; unit stop sc-l
  rm -f /var/lib/blkmap/sc-l.cow   # the bitmap still says 32 chunks are in the cow file
  local s=$(since)
  if unit start sc-l; then bad cow_file_lost "started with the cow file missing but a bitmap claiming data"; unit stop sc-l; else logged sc-l "$s" "cow file" && ok cow_file_lost || bad cow_file_lost "refused without naming the cow file"; fi
}
sc_bitmap_lost() {
  cfg sc-b <<YML
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-b && wait_dev sc-b || { bad bitmap_lost "start"; return; }
  dd if=/dev/zero of=/dev/blkmap/sc-b bs=1M count=2 oflag=direct status=none; unit stop sc-b
  rm -f /var/lib/blkmap/sc-b.cow.bitmap
  # Without the bitmap the overlay is unknown: the device comes up on its sources alone
  unit start sc-b && wait_dev sc-b && cmp <(dd if=/dev/blkmap/sc-b bs=1M count=2 status=none) <(dd if=$dir/img64 bs=1M count=2 status=none) && ok bitmap_lost || bad bitmap_lost "did not come up on the sources"
  unit stop sc-b
}
sc_cow_disk_full() {
  mkdir -p $dir/small; mount -t tmpfs -o size=24M tmpfs $dir/small
  cfg sc-f <<YML
cow:
  file: $dir/small/f.cow
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-f && wait_dev sc-f || { bad cow_disk_full "start"; umount $dir/small; return; }
  dd if=/dev/urandom of=/dev/blkmap/sc-f bs=1M count=48 oflag=direct status=none 2>/dev/null && { bad cow_disk_full "48 MiB of writes fit in a 24 MiB cow filesystem"; unit stop sc-f; umount $dir/small; return; }
  log "writes beyond the cow filesystem's space failed as they should"
  cmp <(dd if=/dev/blkmap/sc-f bs=1M skip=60 count=4 iflag=direct status=none) <(dd if=$dir/img64 bs=1M skip=60 count=4 status=none) && ok cow_disk_full || bad cow_disk_full "reads broke after the full disk"
  unit stop sc-f; umount $dir/small
}
sc_read_only_device() {
  cfg sc-ro <<YML
read-only: true
segments:
  - type: file
    path: $dir/img64
YML
  rm -f /var/lib/blkmap/sc-ro.cow*
  local s=$(since)
  unit start sc-ro && wait_dev sc-ro || { bad read_only_device "start"; return; }
  dd if=/dev/zero of=/dev/blkmap/sc-ro bs=4k count=1 oflag=direct status=none 2>/dev/null && { bad read_only_device "write succeeded"; unit stop sc-ro; return; }
  [ "$(blockdev --getro /dev/blkmap/sc-ro)" = 1 ] || { bad read_only_device "not flagged read-only"; unit stop sc-ro; return; }
  # Nothing is stored, so nothing is kept: no cow file, bitmap or live bitmap, across a crash and a reload
  systemctl kill -s KILL blkmap@sc-ro; sleep 2; wait_dev sc-ro; systemctl reload blkmap@sc-ro; sleep 2
  cmp -s /dev/blkmap/sc-ro $dir/img64 || { bad read_only_device "content differs from the image"; unit stop sc-ro; return; }
  local st; st=$(blkmap status sc-ro 2>&1)
  ls /var/lib/blkmap/sc-ro.cow* /run/blkmap/sc-ro.bitmap >/dev/null 2>&1 && { bad read_only_device "a cow file or bitmap was created"; unit stop sc-ro; return; }
  grep -q "read-only, no cow file" <<<"$st" && [ "$(journal sc-ro "$s" | grep -c re-attached)" -ge 2 ] && ok read_only_device || bad read_only_device "status or re-attach: $(head -2 <<<"$st" | tail -1), $(journal sc-ro "$s" | grep -c re-attached) re-attaches"
  unit stop sc-ro
}
sc_bad_configs_rejected() {
  local n=0
  for c in "segments: []" "segments:\n  - type: nope" "segments:\n  - type: raid5\n    members:\n      - missing: true\n      - missing: true\n      - type: file\n        path: $dir/img64" "segments:\n  - type: file\n    path: $dir/img64\n    map: http://127.0.0.1:$port/no-such.map" "sizes: 1M\nsegments:\n  - type: zero\n    size: 1M"; do
    printf "$c\n" > /etc/blkmap/sc-bad.yml
    blkmap validate sc-bad >/dev/null 2>&1 && { bad bad_configs_rejected "accepted: $(echo "$c" | head -c 60)"; return; }
    n=$((n+1))
  done
  rm -f /etc/blkmap/sc-bad.yml; ok bad_configs_rejected
}
sc_many_devices() {
  for i in $(seq 1 12); do cfg sc-n$i <<YML
segments:
  - type: file
    path: $dir/img64
    source-offset: $((i))M
YML
  done
  for i in $(seq 1 12); do unit start sc-n$i & done; wait
  local good=0
  for i in $(seq 1 12); do wait_dev sc-n$i && cmp <(dd if=/dev/blkmap/sc-n$i bs=1M count=4 status=none) <(dd if=$dir/img64 bs=1M skip=$i count=4 status=none) && good=$((good+1)); done
  for i in $(seq 1 12); do unit stop sc-n$i & done; wait
  [ $good = 12 ] && ok many_devices || bad many_devices "$good of 12 devices correct"
  rm -f /etc/blkmap/sc-n*.yml /var/lib/blkmap/sc-n*
}
sc_huge_device() {
  cfg sc-big <<YML
size: 8T
segments:
  - type: zero
    size: 8T
YML
  unit start sc-big && wait_dev sc-big || { bad huge_device "start"; return; }
  local last=$((8*1024*1024*1024/4-1))
  echo huge | dd of=/dev/blkmap/sc-big bs=4k seek=$last conv=sync status=none
  sync -f /var/lib/blkmap
  [ "$(dd if=/dev/blkmap/sc-big bs=4k skip=$last count=1 status=none | head -c 4)" = huge ] && log "bitmap: $(ls -la /var/lib/blkmap/sc-big.cow.bitmap | awk '{print $5}') bytes, rss $(grep VmRSS /proc/$(systemctl show -p MainPID --value blkmap@sc-big)/status | awk '{print $2}') kB" && ok huge_device || bad huge_device "write at the end of 8 TiB did not read back"
  unit stop sc-big
}
sc_unprivileged() {
  cfg sc-u <<YML
segments:
  - type: file
    path: $dir/img64
YML
  chmod 644 /etc/blkmap/sc-u.yml
  su -s /bin/bash nobody -c "blkmap serve sc-u" >/dev/null 2>&1 && { bad unprivileged "served as nobody"; return; }
  [ -e /dev/blkmap/sc-u ] && { bad unprivileged "device appeared"; return; }
  ok unprivileged
}
sc_partition_table_survives_restart() {
  cfg sc-p <<YML
size: 256M
segments:
  - type: zero
    size: 256M
YML
  unit start sc-p && wait_dev sc-p || { bad partition_table_survives_restart "start"; return; }
  local dev=$(readlink -f /dev/blkmap/sc-p)
  printf 'label: gpt\n,64M\n,\n' | sfdisk -q $dev >/dev/null 2>&1; udevadm settle
  [ -e ${dev}p1 ] && [ -e ${dev}p2 ] || { bad partition_table_survives_restart "partitions did not appear"; unit stop sc-p; return; }
  mkfs.ext4 -q ${dev}p2 && mount ${dev}p2 $mnt && echo part > $mnt/f && umount $mnt
  unit restart sc-p; wait_dev sc-p; udevadm settle; dev=$(readlink -f /dev/blkmap/sc-p)
  [ -e ${dev}p2 ] && mount ${dev}p2 $mnt && [ "$(cat $mnt/f)" = part ] && umount $mnt && ok partition_table_survives_restart || bad partition_table_survives_restart "partition or file gone after restart"
  unit stop sc-p
}
sc_fstab_mount_dependency() {
  cfg sc-fs <<YML
size: 128M
segments:
  - type: zero
    size: 128M
YML
  unit start sc-fs && wait_dev sc-fs && mkfs.ext4 -q -F /dev/blkmap/sc-fs && unit stop sc-fs || { bad fstab_mount_dependency "prepare"; return; }
  wait_gone sc-fs
  mkdir -p /mnt/blkmap-scen
  echo "/dev/blkmap/sc-fs /mnt/blkmap-scen ext4 x-systemd.requires=blkmap@sc-fs.service,nofail 0 0" >> /etc/fstab
  systemctl daemon-reload
  # mount(8) ignores x-systemd.requires; starting the mount unit is what boot does
  if systemctl start "$(systemd-escape -p --suffix=mount /mnt/blkmap-scen)" && mountpoint -q /mnt/blkmap-scen && [ "$(systemctl is-active blkmap@sc-fs)" = active ]; then ok fstab_mount_dependency; else bad fstab_mount_dependency "the mount unit did not pull the device unit up"; fi
  umount /mnt/blkmap-scen 2>/dev/null; sed -i '/blkmap-scen/d' /etc/fstab; systemctl daemon-reload; unit stop sc-fs
}
sc_hydration_survives_origin_outage() {
  origin $((port+1))
  cfg sc-ho <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
hydrate:
  rate: 8M
  report-every: 1s
YML
  local s=$(since); unit start sc-ho && wait_dev sc-ho || { bad hydration_survives_origin_outage "start"; return; }
  sleep 2; fuser -k $((port+1))/tcp >/dev/null 2>&1; sleep 4; origin $((port+1))
  for i in $(seq 1 120); do logged sc-ho "$s" 'hydration done: 1024/1024' && break; sleep 1; done
  journal sc-ho "$s" | grep 'hydration done' | tail -1 | sed 's/^/    /'
  logged sc-ho "$s" 'hydration done: 1024/1024' && cmp <(dd if=/dev/blkmap/sc-ho bs=1M status=none) $dir/img64 && ok hydration_survives_origin_outage || bad hydration_survives_origin_outage "hydration did not complete after the outage"
  unit stop sc-ho; fuser -k $((port+1))/tcp >/dev/null 2>&1
}
sc_hydration_vs_guest_writes() {
  cfg sc-hw <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  rate: 16M
YML
  unit start sc-hw && wait_dev sc-hw || { bad hydration_vs_guest_writes "start"; return; }
  # fio writes and verifies while hydration copies underneath; a hydration run that
  # overwrote a guest write would fail the verify
  fio --name=v --ioengine=io_uring --filename=/dev/blkmap/sc-hw --rw=randwrite --bs=4k --direct=1 --iodepth=8 --size=32M --verify=crc32c --do_verify=1 --verify_fatal=1 --output-format=terse >/dev/null 2>&1 && ok hydration_vs_guest_writes || bad hydration_vs_guest_writes "fio verify failed during hydration"
  unit stop sc-hw
}
sc_detached_after_sources_gone() {
  cp $dir/img64 $dir/img64-copy
  cfg sc-det <<YML
segments:
  - type: file
    path: $dir/img64-copy
hydrate: {}
YML
  local s=$(since); unit start sc-det && wait_dev sc-det || { bad detached_after_sources_gone "start"; return; }
  for i in $(seq 1 60); do logged sc-det "$s" 'hydration done: 1024/1024' && break; sleep 1; done
  unit stop sc-det; rm -f $dir/img64-copy
  unit start sc-det && wait_dev sc-det && cmp <(dd if=/dev/blkmap/sc-det bs=1M status=none) $dir/img64 && ok detached_after_sources_gone || bad detached_after_sources_gone "did not come up without its source"
  unit stop sc-det
}
sc_stop_while_mounted() {
  cfg sc-sm <<YML
size: 128M
segments:
  - type: zero
    size: 128M
YML
  unit start sc-sm && wait_dev sc-sm && mkfs.ext4 -q -F /dev/blkmap/sc-sm && mount /dev/blkmap/sc-sm $mnt || { bad stop_while_mounted "prepare"; return; }
  echo keep > $mnt/keep; sync -f $mnt
  local t0=$(date +%s); unit stop sc-sm; local t=$(( $(date +%s) - t0 ))
  umount -l $mnt 2>/dev/null; systemctl reset-failed blkmap@sc-sm
  unit start sc-sm && wait_dev sc-sm && mount /dev/blkmap/sc-sm $mnt && [ "$(cat $mnt/keep)" = keep ] && umount $mnt && ok stop_while_mounted || bad stop_while_mounted "data lost across the forced stop (${t}s)"
  unit stop sc-sm
}
sc_rapid_restart_reuses_id() {
  cfg sc-rr <<YML
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-rr && wait_dev sc-rr || { bad rapid_restart_reuses_id "start"; return; }
  for i in 1 2 3; do systemctl restart blkmap@sc-rr; sleep 2.5; done
  wait_dev sc-rr && [ "$(readlink -f /dev/blkmap/sc-rr)" = "/dev/ublkb$(cut -d" " -f1 /run/blkmap/sc-rr)" ] && cmp <(dd if=/dev/blkmap/sc-rr bs=1M count=8 status=none) <(head -c 8M $dir/img64) && ok rapid_restart_reuses_id || bad rapid_restart_reuses_id "symlink/state/content inconsistent after rapid restarts"
  unit stop sc-rr
}
sc_sigterm_twice() {
  cfg sc-st <<YML
segments:
  - type: file
    path: $dir/img64
YML
  unit start sc-st && wait_dev sc-st || { bad sigterm_twice "start"; return; }
  local pid=$(systemctl show -p MainPID --value blkmap@sc-st)
  kill -TERM $pid; kill -TERM $pid 2>/dev/null
  for i in $(seq 1 100); do [ "$(systemctl is-active blkmap@sc-st)" = inactive ] && break; sleep 0.1; done
  [ "$(systemctl is-active blkmap@sc-st)" = inactive ] && ! [ -e /dev/blkmap/sc-st ] && ok sigterm_twice || bad sigterm_twice "did not exit cleanly on double SIGTERM"
  systemctl reset-failed blkmap@sc-st 2>/dev/null
}
sc_record_then_prefetch() {
  # Record a workload, compact it, and hydrate exactly that on a fresh device
  cfg sc-rec <<YML
segments:
  - type: file
    path: $dir/img64
record:
  file: /var/lib/blkmap/sc-rec.rec
  max-duration: 4s
YML
  local s=$(since); unit start sc-rec && wait_dev sc-rec || { bad record_then_prefetch "start"; return; }
  for off in 10 30 50; do dd if=/dev/blkmap/sc-rec of=/dev/null bs=1M count=1 skip=$off iflag=direct status=none; done
  dd if=/dev/zero of=/dev/blkmap/sc-rec bs=4k count=1 seek=1280 oflag=direct status=none
  for i in $(seq 1 15); do logged sc-rec "$s" 'recording to .* ended' && break; sleep 1; done
  logged sc-rec "$s" 'recording to .* ended (max-duration reached)' || { bad record_then_prefetch "recording did not end at max-duration"; unit stop sc-rec; return; }
  grep -q 'recording done' <<< "$(blkmap status sc-rec)" || { bad record_then_prefetch "status does not show the finished recording"; unit stop sc-rec; return; }
  unit stop sc-rec
  blkmap recording compact /var/lib/blkmap/sc-rec.rec > $dir/sc-rec.prefetch || { bad record_then_prefetch "compaction failed"; return; }
  blkmap recording stats /var/lib/blkmap/sc-rec.rec | sed 's/^/    /'
  for want in 10485760 31457280 52428800; do grep -q " R $want " $dir/sc-rec.prefetch || { bad record_then_prefetch "read at $want missing from the list"; return; }; done
  grep -q ' W ' $dir/sc-rec.prefetch && { bad record_then_prefetch "writes in the compacted list"; return; }
  grep -q ' W 5242880 4096' /var/lib/blkmap/sc-rec.rec || { bad record_then_prefetch "the write is missing from the raw recording"; return; }
  local chunks=$(awk '{n += $4} END {print n / 65536}' $dir/sc-rec.prefetch)
  cfg sc-rec2 <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  prefetch-list: $dir/sc-rec.prefetch
  rest: false
YML
  s=$(since); unit start sc-rec2 && wait_dev sc-rec2 || { bad record_then_prefetch "second start"; return; }
  for i in $(seq 1 30); do logged sc-rec2 "$s" 'hydration done' && break; sleep 1; done
  log "$(journal sc-rec2 "$s" | grep 'hydration done') (list has $chunks chunks)"
  grep -q "hydration done: $chunks/1024 chunks.* 0 errors" <<<"$(journal sc-rec2 "$s")" && cmp <(dd if=/dev/blkmap/sc-rec2 bs=1M skip=30 count=1 status=none) <(dd if=$dir/img64 bs=1M skip=30 count=1 status=none) && ok record_then_prefetch || bad record_then_prefetch "hydration did not copy exactly the recorded chunks"
  unit stop sc-rec2; rm -f /var/lib/blkmap/sc-rec.rec
}
sc_prefetch_beyond_end() {
  printf '60M 100M\n200M 1M\n' > $dir/pf
  cfg sc-pf <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  prefetch-list: $dir/pf
  rest: false
YML
  local s=$(since); unit start sc-pf && wait_dev sc-pf || { bad prefetch_beyond_end "start"; return; }
  for i in $(seq 1 30); do logged sc-pf "$s" 'hydration done' && break; sleep 1; done
  grep -q 'hydration done.* 0 errors' <<<"$(journal sc-pf "$s")" && ok prefetch_beyond_end || bad prefetch_beyond_end "errors or no completion with out-of-range prefetch ranges"
  unit stop sc-pf
}
sc_cache_tier_vanishes() {
  cfg sc-ct <<YML
segments:
  - type: cache
    fast: {type: file, path: $dir/fast64}
    slow: {type: file, path: $dir/img64}
YML
  cp $dir/img64 $dir/fast64
  unit start sc-ct && wait_dev sc-ct || { bad cache_tier_vanishes "start"; return; }
  dd if=/dev/blkmap/sc-ct bs=1M count=4 of=/dev/null iflag=direct status=none
  truncate -s 8M $dir/fast64   # the fast tier loses most of its content; reads must fall through
  cmp <(dd if=/dev/blkmap/sc-ct bs=1M skip=16 count=8 iflag=direct status=none) <(dd if=$dir/img64 bs=1M skip=16 count=8 status=none) && ok cache_tier_vanishes || bad cache_tier_vanishes "reads did not fall through to the slow tier"
  unit stop sc-ct; rm -f $dir/fast64
}
sc_partial_cache_file() {
  # A sparse local copy is the fast tier with no map: what it holds is served from it, what it
  # lacks (holes) comes from the slow tier, never zeros
  cp --sparse=never $dir/img64 $dir/partial64; for o in 8 24 40 56; do fallocate -p -o ${o}M -l 8M $dir/partial64; done
  cfg sc-pc <<YML
segments:
  - type: cache
    fast: {type: file, path: $dir/partial64}
    slow: {type: file, path: $dir/img64}
YML
  unit start sc-pc && wait_dev sc-pc || { bad partial_cache_file "start"; return; }
  cmp <(dd if=/dev/blkmap/sc-pc bs=1M iflag=direct status=none) $dir/img64 || { bad partial_cache_file "content differs from the image (holes served as zeros?)"; unit stop sc-pc; return; }
  local st=$(blkmap status sc-pc | grep cache:); log "$st"
  grep -qE "cache: [1-9][0-9]* hits, [1-9][0-9]* misses, 0 failures" <<<"$st" && ok partial_cache_file || bad partial_cache_file "expected hits and misses: $st"
  unit stop sc-pc; rm -f $dir/partial64
}
sc_discard_reclaims_space() {
  cfg sc-ds <<YML
size: 256M
segments:
  - type: zero
    size: 256M
YML
  unit start sc-ds && wait_dev sc-ds || { bad discard_reclaims_space "start"; return; }
  dd if=/dev/urandom of=/dev/blkmap/sc-ds bs=1M count=64 oflag=direct status=none; sync -f /var/lib/blkmap
  local before=$(du -k /var/lib/blkmap/sc-ds.cow | cut -f1)
  blkdiscard /dev/blkmap/sc-ds && sync -f /var/lib/blkmap
  local after=$(du -k /var/lib/blkmap/sc-ds.cow | cut -f1)
  log "cow file: ${before} kB before discard, ${after} kB after"
  [ $after -lt $((before/4)) ] && cmp <(dd if=/dev/blkmap/sc-ds bs=1M count=4 status=none) <(head -c 4M /dev/zero) && ok discard_reclaims_space || bad discard_reclaims_space "discard did not reclaim or zeros not read back"
  unit stop sc-ds
}
sc_reload_handoff_under_load() {
  cfg sc-rl <<YML
size: 512M
segments:
  - type: zero
    size: 512M
YML
  unit start sc-rl && wait_dev sc-rl && mkfs.ext4 -q -F /dev/blkmap/sc-rl && mount /dev/blkmap/sc-rl $mnt || { bad reload_handoff_under_load "prepare"; return; }
  local s=$(since)
  fio --name=v --ioengine=io_uring --directory=$mnt --rw=randwrite --bs=4k --size=64M --iodepth=8 --runtime=15 --time_based --verify=crc32c --do_verify=1 --verify_fatal=1 --output-format=terse >/dev/null 2>&1 & local fp=$!
  for i in 1 2 3; do sleep 3; systemctl reload blkmap@sc-rl || { bad reload_handoff_under_load "reload $i failed"; break; }; done
  wait $fp; local rc=$?
  local n=$(journal sc-rl "$s" | grep -c "re-attached")
  umount $mnt; fsck.ext4 -f -n /dev/blkmap/sc-rl >/dev/null 2>&1; local fsck=$?
  [ $rc = 0 ] && [ "$n" -ge 3 ] && [ $fsck = 0 ] && ok reload_handoff_under_load || bad reload_handoff_under_load "fio $rc, $n re-attaches, fsck $fsck"
  unit stop sc-rl
}
sc_package_upgrade_under_load() {
  local deb=""; for d in /root/blkmap-test/blkmap_*_linux_amd64.deb; do [ -f "$d" ] && deb=$d; done
  [ -n "$deb" ] || { bad package_upgrade_under_load "no deb in /root/blkmap-test"; return; }
  cfg sc-up <<YML
size: 512M
segments:
  - type: zero
    size: 512M
YML
  unit start sc-up && wait_dev sc-up && mkfs.ext4 -q -F /dev/blkmap/sc-up && mount /dev/blkmap/sc-up $mnt || { bad package_upgrade_under_load "prepare"; return; }
  local pid=$(systemctl show -p MainPID --value blkmap@sc-up) s=$(since)
  fio --name=v --ioengine=io_uring --directory=$mnt --rw=randwrite --bs=4k --size=64M --iodepth=8 --runtime=12 --time_based --verify=crc32c --do_verify=1 --verify_fatal=1 --output-format=terse >/dev/null 2>&1 & local fp=$!
  sleep 3; dpkg -i "$deb" >/dev/null 2>&1 || { bad package_upgrade_under_load "dpkg -i failed"; }
  wait $fp; local rc=$?
  umount $mnt
  # The server re-executed the installed binary in place (same pid) and re-attached
  [ $rc = 0 ] && logged sc-up "$s" "re-executing" && logged sc-up "$s" "re-attached" && [ "$(systemctl is-active blkmap@sc-up)" = active ] && ok package_upgrade_under_load || bad package_upgrade_under_load "fio $rc or no handoff logged"
  unit stop sc-up
}
sc_origin_down_across_crash() {
  origin $((port+1))
  cfg sc-oc <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
YML
  unit start sc-oc && wait_dev sc-oc || { bad origin_down_across_crash "start"; return; }
  local s=$(since)
  fuser -k $((port+1))/tcp >/dev/null 2>&1
  kill -9 $(systemctl show -p MainPID --value blkmap@sc-oc)
  # A read waits while the server is gone and its restarts fail on the dead origin
  dd if=/dev/blkmap/sc-oc bs=1M skip=20 count=1 iflag=direct of=$dir/oc.out status=none 2>/dev/null & local rd=$!
  sleep 5; origin $((port+1))
  for i in $(seq 1 60); do kill -0 $rd 2>/dev/null || break; sleep 0.5; done
  if kill -0 $rd 2>/dev/null; then bad origin_down_across_crash "the read never completed"; kill $rd; unit stop sc-oc; return; fi
  wait $rd; local rc=$?
  cmp $dir/oc.out <(dd if=$dir/img64 bs=1M skip=20 count=1 status=none) && [ $rc = 0 ] && logged sc-oc "$s" "re-attached" && ok origin_down_across_crash || bad origin_down_across_crash "read rc $rc or wrong data after recovery"
  unit stop sc-oc; fuser -k $((port+1))/tcp >/dev/null 2>&1
}
sc_crash_loop_reaps() {
  origin $((port+1))
  cfg sc-cl <<YML
segments:
  - type: http
    url: http://127.0.0.1:$((port+1))/img64
YML
  unit start sc-cl && wait_dev sc-cl || { bad crash_loop_reaps "start"; return; }
  fuser -k $((port+1))/tcp >/dev/null 2>&1
  kill -9 $(systemctl show -p MainPID --value blkmap@sc-cl)
  # The origin never returns: once systemd gives up, the waiting read must fail, not hang
  local t0=$(date +%s)
  dd if=/dev/blkmap/sc-cl bs=1M skip=20 count=1 iflag=direct of=$dir/cl.out status=none 2>/dev/null & local rd=$!
  for i in $(seq 1 120); do kill -0 $rd 2>/dev/null || break; sleep 0.5; done
  if kill -0 $rd 2>/dev/null; then bad crash_loop_reaps "the read still hangs after $(( $(date +%s) - t0 ))s"; kill -9 $rd; blkmap reap sc-cl; return; fi
  wait $rd; local rc=$?
  log "the read failed after $(( $(date +%s) - t0 ))s; unit result $(systemctl show -p Result --value blkmap@sc-cl)"
  blkmap reap sc-cl # the stopped device is deleted once nothing holds it
  # Failed: an error, or the stopped disk reading as its end; never the data
  local got=$(stat -c %s $dir/cl.out 2>/dev/null || echo 0)
  [ "$got" != 1048576 ] && ok crash_loop_reaps || bad crash_loop_reaps "the read returned data without a server (rc $rc)"
  systemctl reset-failed blkmap@sc-cl
}
sc_source_changed_refused() {
  cp $dir/img64 $dir/img64-pin
  cfg sc-sc <<YML
segments:
  - type: file
    path: $dir/img64-pin
YML
  unit start sc-sc && wait_dev sc-sc || { bad source_changed_refused "start"; return; }
  dd if=/dev/urandom of=/dev/blkmap/sc-sc bs=1M count=1 oflag=direct status=none; unit stop sc-sc
  # The image is replaced while the device is down: the overlay belongs to the old one
  touch -d tomorrow $dir/img64-pin
  local s=$(since)
  if unit start sc-sc; then bad source_changed_refused "started over a changed source"; unit stop sc-sc; return; fi
  logged sc-sc "$s" "source changed" || { bad source_changed_refused "no clear message"; return; }
  systemctl reset-failed blkmap@sc-sc
  blkmap pin sc-sc >/dev/null && unit start sc-sc && wait_dev sc-sc && ok source_changed_refused || bad source_changed_refused "pin did not let it start"
  unit stop sc-sc; rm -f $dir/img64-pin
}
sc_status_and_metrics() {
  cfg sc-sm2 <<YML
segments:
  - type: file
    path: $dir/img64
hydrate:
  rate: 8M
YML
  unit start sc-sm2 && wait_dev sc-sm2 || { bad status_and_metrics "start"; return; }
  dd if=/dev/blkmap/sc-sm2 bs=1M count=4 iflag=direct of=/dev/null status=none; sleep 1
  local st=$(blkmap status sc-sm2) m=$(blkmap metrics)
  echo "$st" | sed 's/^/    /'
  grep -q "chunks in the cow file" <<<"$st" && grep -q "hydration" <<<"$st" && grep -q 'blkmap_up{device="sc-sm2"} 1' <<<"$m" && grep -q 'blkmap_requests_total{device="sc-sm2",op="read"}' <<<"$m" && grep -q '"id":"sc-sm2"' <<<"$(blkmap status --json sc-sm2)" && ok status_and_metrics || bad status_and_metrics "status or metrics output incomplete"
  unit stop sc-sm2
}
# --- main --------------------------------------------------------------------------------
trap cleanup_all EXIT
cleanup_all
mkdir -p $dir $mnt /etc/blkmap
[ -f $dir/img64 ] || head -c 64M /dev/urandom > $dir/img64
[ -f $dir/img32 ] || head -c 32M /dev/urandom > $dir/img32
baseline=$(ls /sys/class/ublk-char | wc -l)
origin $port
all="start_stop_cycles missing_source origin_down_at_start origin_dies_mid_flight origin_hangs_then_stop kill9_under_write_load kill9_during_hydration stop_during_hydration restart_storm_under_reads two_devices_one_cow geometry_change_refused cow_file_lost bitmap_lost cow_disk_full read_only_device bad_configs_rejected many_devices huge_device unprivileged partition_table_survives_restart fstab_mount_dependency hydration_survives_origin_outage hydration_vs_guest_writes detached_after_sources_gone stop_while_mounted rapid_restart_reuses_id sigterm_twice prefetch_beyond_end record_then_prefetch cache_tier_vanishes partial_cache_file discard_reclaims_space reload_handoff_under_load package_upgrade_under_load origin_down_across_crash crash_loop_reaps source_changed_refused status_and_metrics"
for name in ${@:-$all}; do run $name; done
echo; echo "passed $pass, failed $fail"
for f in "${failed[@]:-}"; do [ -n "$f" ] && echo "  $f"; done
[ $fail = 0 ]
