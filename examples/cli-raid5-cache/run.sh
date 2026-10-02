#!/bin/bash
# Degraded RAID-5 over cache tiers with hydration, run as the systemd unit blkmap@raid5.
# Builds the members from a random 24 MiB image with scripts/mkraid5.py, keeps only the
# first 4 MiB of each member in the "fast" directory, serves the full members over HTTP,
# installs device.yml as /etc/blkmap/raid5.yml, starts the unit and waits for hydration.
# Needs root, the blkmap deb, python3.
set -euo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
dir=/var/tmp/blkmap-example-raid5
cleanup() {
  systemctl stop blkmap@raid5 2>/dev/null || true
  [ -n "${httpd:-}" ] && kill $httpd 2>/dev/null || true
}
trap cleanup EXIT
rm -rf $dir /var/lib/blkmap/raid5.cow /var/lib/blkmap/raid5.cow.bitmap; mkdir -p $dir/fast $dir/slow
head -c $((24<<20)) /dev/urandom > $dir/image
python3 "$me/../../scripts/mkraid5.py" $dir/image 65536 $dir/slow 4
for m in member0 member2 member3; do head -c $((4<<20)) $dir/slow/$m > $dir/fast/$m; done
cp "$me/prefetch" $dir/prefetch
python3 "$me/../../scripts/rangehttpd.py" $dir/slow 18101 & httpd=$!
sleep 1
cp "$me/device.yml" /etc/blkmap/raid5.yml

echo "== validate"
blkmap validate raid5
echo "== start"
since="--since=@$(date +%s)"
systemctl start blkmap@raid5
cmp <(dd if=/dev/blkmap/raid5 bs=1M status=none) $dir/image && echo "the degraded array reads back the original image (member 1 rebuilt from parity)"
echo "== hydration (list first, then the rest at 64M/s, bypassing the caches)"
for i in $(seq 1 60); do journalctl -u blkmap@raid5 $since --no-pager -o cat | grep -q 'hydration done' && break; sleep 1; done
journalctl -u blkmap@raid5 $since --no-pager -o cat | grep hydration
echo "== the device no longer needs its sources"
systemctl stop blkmap@raid5
kill $httpd; httpd=
rm -rf $dir/fast $dir/slow
systemctl start blkmap@raid5
cmp <(dd if=/dev/blkmap/raid5 bs=1M status=none) $dir/image && echo "content intact with every source gone"
systemctl stop blkmap@raid5
rm -f /etc/blkmap/raid5.yml
echo "done"
