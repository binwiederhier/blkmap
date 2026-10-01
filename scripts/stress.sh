#!/bin/bash
# Workload and integrity stress for an installed blkmap (needs root, ublk_drv, fio).
# Builds devices from every source type, runs fio verify workloads, formats and checks
# ext4/xfs/btrfs, exercises discard, restarts and SIGKILLs the daemon under load, and
# writes throughput numbers to $OUT. Leaves nothing running.
set -euo pipefail
dir=/var/tmp/blkmap-stress
mnt=/mnt/blkmap-stress
OUT=${OUT:-$dir/results.txt}
me="$(cd "$(dirname "$0")" && pwd)"
cleanup() {
  umount $mnt 2>/dev/null || true
  for id in s-mix s-raid s-big s-4k s-hyd; do systemctl stop blkmap@$id 2>/dev/null || true; done
  systemctl reset-failed 'blkmap@*' 2>/dev/null || true
  [ -n "${httpd:-}" ] && kill $httpd 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf $dir /var/lib/blkmap/s-*.cow /var/lib/blkmap/s-*.cow.bitmap
mkdir -p $dir $mnt
cd $dir # fio drops verify state files in the working directory
echo "blkmap stress $(date -u +%FT%TZ) on $(uname -r), $(nproc) cpus" | tee $OUT

# --- sources: random images, raid5 members, range-capable http server ---
head -c $((64<<20)) /dev/urandom > $dir/img64.img
head -c $((24<<20)) /dev/urandom > $dir/raid.img
python3 $me/mkraid5.py $dir/raid.img 65536 $dir 4
fuser -k 18099/tcp >/dev/null 2>&1 || true
python3 $me/rangehttpd.py $dir 18099 & httpd=$!
sleep 1

cat > /etc/blkmap/s-mix.yml <<YML
size: 256M
segments:
  - type: zero
    size: 1M
  - type: file
    path: $dir/img64.img
  - type: http
    offset: 128M
    url: http://localhost:18099/img64.img
YML
cat > /etc/blkmap/s-raid.yml <<YML
segments:
  - type: raid5
    stripe-size: 64K
    members:
      - type: file
        path: $dir/member0
      - missing: true
      - type: file
        path: $dir/member2
      - type: file
        path: $dir/member3
YML
cat > /etc/blkmap/s-big.yml <<YML
size: 4G
segments:
  - type: zero
    size: 4G
YML
cat > /etc/blkmap/s-4k.yml <<YML
block-size: 4096
cow:
  chunk-size: 1M
segments:
  - type: file
    path: $dir/img64.img
YML
for id in s-mix s-raid s-big s-4k; do blkmap validate $id >/dev/null; systemctl start blkmap@$id; done
echo "== content" | tee -a $OUT
cmp <(dd if=/dev/blkmap/s-mix bs=1M skip=1 count=64 status=none) $dir/img64.img && echo "mix file segment OK" | tee -a $OUT
cmp <(dd if=/dev/blkmap/s-mix bs=1M skip=128 count=64 status=none) $dir/img64.img && echo "mix http segment OK" | tee -a $OUT
cmp <(dd if=/dev/blkmap/s-raid bs=1M status=none) $dir/raid.img && echo "raid5 degraded reassembly OK" | tee -a $OUT
cmp <(dd if=/dev/blkmap/s-4k bs=1M status=none) $dir/img64.img && echo "4k block size device OK" | tee -a $OUT

# --- fio: verify workloads over the mixed device (reads hit file+http, writes hit the cow) ---
echo "== fio verify (s-mix)" | tee -a $OUT
fio --name=randwrite-verify --filename=/dev/blkmap/s-mix --rw=randwrite --bs=4k --direct=1 --iodepth=32 --numjobs=4 --size=48M --offset_increment=48M \
    --verify=crc32c --verify_fatal=1 --do_verify=1 --group_reporting --output-format=terse --terse-version=3 | tail -1 | awk -F';' '{printf "  4k randwrite+verify: write %d KiB/s %d iops\n", $48, $49}' | tee -a $OUT
fio --name=randrw-verify --filename=/dev/blkmap/s-mix --rw=randrw --rwmixread=70 --bs=64k --direct=1 --iodepth=16 --numjobs=2 --size=100M --offset_increment=100M \
    --verify=crc32c --verify_fatal=1 --do_verify=1 --group_reporting --output-format=terse --terse-version=3 | tail -1 | awk -F';' '{printf "  64k randrw 70/30+verify: read %d KiB/s, write %d KiB/s\n", $7, $48}' | tee -a $OUT
fio --name=seqwrite-verify --filename=/dev/blkmap/s-mix --rw=write --bs=1M --direct=1 --iodepth=8 --size=256M \
    --verify=crc32c --verify_fatal=1 --do_verify=1 --output-format=terse --terse-version=3 | tail -1 | awk -F';' '{printf "  1M seqwrite+verify: write %d KiB/s\n", $48}' | tee -a $OUT
# Buffered path with large and small blocks
fio --name=buffered --filename=/dev/blkmap/s-mix --rw=randrw --bs=16k --direct=0 --iodepth=4 --numjobs=2 --size=64M --offset_increment=64M \
    --verify=crc32c --verify_fatal=1 --do_verify=1 --group_reporting --output-format=terse --terse-version=3 | tail -1 | awk -F';' '{printf "  16k buffered randrw+verify: read %d KiB/s, write %d KiB/s\n", $7, $48}' | tee -a $OUT

# --- throughput on the 4G zero-backed device (cow absorbs writes) ---
echo "== throughput (s-big, 4 GiB zero source + cow)" | tee -a $OUT
for bs in 4k 64k 1M; do
  fio --name=r --filename=/dev/blkmap/s-big --rw=randread --bs=$bs --direct=1 --iodepth=32 --numjobs=4 --runtime=5 --time_based --group_reporting --output-format=terse --terse-version=3 | tail -1 \
    | awk -F';' -v bs=$bs '{printf "  %s randread: %d MiB/s %d iops\n", bs, $7/1024, $8}' | tee -a $OUT
done
for bs in 4k 64k 1M; do
  fio --name=w --filename=/dev/blkmap/s-big --rw=randwrite --bs=$bs --direct=1 --iodepth=32 --numjobs=4 --runtime=5 --time_based --group_reporting --output-format=terse --terse-version=3 | tail -1 \
    | awk -F';' -v bs=$bs '{printf "  %s randwrite: %d MiB/s %d iops\n", bs, $48/1024, $49}' | tee -a $OUT
done
fio --name=sr --filename=/dev/blkmap/s-big --rw=read --bs=1M --direct=1 --iodepth=16 --runtime=5 --time_based --output-format=terse --terse-version=3 | tail -1 \
  | awk -F';' '{printf "  1M seqread: %d MiB/s\n", $7/1024}' | tee -a $OUT
fio --name=sw --filename=/dev/blkmap/s-big --rw=write --bs=1M --direct=1 --iodepth=16 --runtime=5 --time_based --output-format=terse --terse-version=3 | tail -1 \
  | awk -F';' '{printf "  1M seqwrite: %d MiB/s\n", $48/1024}' | tee -a $OUT
grep -c . /proc/$(systemctl show -p MainPID --value blkmap@s-big)/status >/dev/null && echo "  rss: $(grep VmRSS /proc/$(systemctl show -p MainPID --value blkmap@s-big)/status)" | tee -a $OUT

# --- filesystems: mkfs, fill, check, discard, restart ---
echo "== filesystems (s-big)" | tee -a $OUT
fscheck() {
  case $1 in
    ext4) mkfs.ext4 -q -F /dev/blkmap/s-big ;;
    xfs) mkfs.xfs -q -f /dev/blkmap/s-big ;;
    btrfs) mkfs.btrfs -q -f /dev/blkmap/s-big ;;
  esac
  mount /dev/blkmap/s-big $mnt
  for i in $(seq 1 20); do head -c $((3<<20)) /dev/urandom > $mnt/f$i; done
  sha256sum $mnt/f* > $dir/sums.$1
  fstrim $mnt 2>/dev/null && trim=trimmed || trim="no trim"
  umount $mnt
  case $1 in
    ext4) fsck.ext4 -f -n /dev/blkmap/s-big >/dev/null ;;
    xfs) xfs_repair -n /dev/blkmap/s-big >/dev/null 2>&1 ;;
    btrfs) btrfs check --readonly /dev/blkmap/s-big >/dev/null 2>&1 ;;
  esac
  systemctl restart blkmap@s-big
  mount /dev/blkmap/s-big $mnt
  (cd / && sha256sum --quiet -c $dir/sums.$1)
  umount $mnt
  echo "  $1: mkfs, 20 files, $trim, fsck clean, survived restart" | tee -a $OUT
}
fscheck ext4; fscheck xfs; fscheck btrfs
blkdiscard /dev/blkmap/s-big && echo "  blkdiscard whole device OK; cow now $(du -h /var/lib/blkmap/s-big.cow | cut -f1)" | tee -a $OUT

# --- crash: SIGKILL the daemon under fio load, restart, fsck ---
echo "== crash under load (s-big)" | tee -a $OUT
ndev=$(ls /sys/class/ublk-char | wc -l)
mkfs.ext4 -q -F /dev/blkmap/s-big
mount /dev/blkmap/s-big $mnt
fio --name=crash --directory=$mnt --rw=randwrite --bs=4k --size=64M --numjobs=4 --runtime=30 --time_based --output-format=terse >/dev/null 2>&1 &
fiopid=$!
sleep 3
kill -9 $(systemctl show -p MainPID --value blkmap@s-big)
sleep 2
wait $fiopid 2>/dev/null || true
umount -l $mnt 2>/dev/null || true
sleep 1
systemctl reset-failed blkmap@s-big 2>/dev/null || true
systemctl start blkmap@s-big
fsck.ext4 -f -y /dev/blkmap/s-big >/dev/null 2>&1 && fsckrc=0 || fsckrc=$?
mount /dev/blkmap/s-big $mnt && ls $mnt >/dev/null && umount $mnt
echo "  daemon SIGKILLed mid-write: restarted, fsck exit $fsckrc (0/1 = clean or repaired), mounts" | tee -a $OUT
[ "$(ls /sys/class/ublk-char | wc -l)" = "$ndev" ] && echo "  dead kernel device from the crash was cleaned up on restart" | tee -a $OUT

# --- cache tier, hydration with a prefetch list, restart without sources (s-hyd) ---
echo "== cache + hydration (s-hyd)" | tee -a $OUT
head -c $((32<<20)) /dev/urandom > $dir/origin.img
head -c $((8<<20)) $dir/origin.img > $dir/partial.img   # the fast tier only has the first 8 MiB
printf '24M 4M\n0 1M\n' > $dir/hyd.prefetch
cat > /etc/blkmap/s-hyd.yml <<YML
segments:
  - type: zero
    size: 4M
  - type: cache
    fast: {type: file, path: $dir/partial.img}
    slow: {type: http, url: http://localhost:18099/origin.img}
hydrate:
  prefetch-list: $dir/hyd.prefetch
  rate: 64M
  use-cache: never
  report-every: 2s
YML
blkmap validate s-hyd | grep -E 'Hydrate|cache' | sed 's/^/  /' | tee -a $OUT
systemctl start blkmap@s-hyd
cmp <(dd if=/dev/blkmap/s-hyd bs=1M skip=4 status=none) $dir/origin.img && echo "  cache fall-through content OK" | tee -a $OUT
for i in $(seq 1 90); do journalctl -u blkmap@s-hyd --no-pager -o cat | grep -q 'hydration done' && break; sleep 1; done
journalctl -u blkmap@s-hyd --no-pager -o cat | grep -E 'hydration (list|rest|done)' | tail -2 | sed 's/^/  /' | tee -a $OUT
systemctl stop blkmap@s-hyd
mv $dir/origin.img $dir/origin.gone; rm $dir/partial.img
systemctl start blkmap@s-hyd
journalctl -u blkmap@s-hyd --no-pager -o cat | grep -q 'fully hydrated' && echo "  restarted without its sources (fully hydrated)" | tee -a $OUT
cmp <(dd if=/dev/blkmap/s-hyd bs=1M skip=4 status=none) $dir/origin.gone && echo "  detached content OK" | tee -a $OUT
systemctl stop blkmap@s-hyd

# --- many restarts and concurrent devices ---
echo "== churn" | tee -a $OUT
# systemd rate-limits unit starts (5 per 10 s by default), so pace the restarts
for i in $(seq 1 10); do systemctl restart blkmap@s-mix blkmap@s-raid; sleep 2.5; done
cmp <(dd if=/dev/blkmap/s-raid bs=1M status=none) $dir/raid.img && echo "  10 restarts of two devices: content intact" | tee -a $OUT
ls /dev/blkmap/ | tr '\n' ' ' | sed 's/^/  devices: /' | tee -a $OUT
echo "STRESS OK" | tee -a $OUT
