#!/bin/bash
# End-to-end check of an installed blkmap package on this host (needs root, ublk_drv).
# Builds a device from a random image file (via file AND http segments), starts it through
# systemd, verifies the stitched content, formats and mounts it, writes a file, restarts the
# unit, and verifies the file survived in the COW overlay. Leaves nothing running.
set -euo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
# The HTTP origin: the Go test server from scripts/rangehttpd (built into dist/ locally,
# shipped as bin/ on a test VM)
rangehttpd=""; for c in "$me/../dist/rangehttpd" "$me/../bin/rangehttpd"; do [ -x "$c" ] && rangehttpd=$c && break; done
[ -x "$rangehttpd" ] || { echo "rangehttpd not found: go build -o dist/rangehttpd ./scripts/rangehttpd" >&2; exit 1; }
id=e2e
dir=/var/tmp/blkmap-$id
mnt=/mnt/blkmap-$id
dev=/dev/blkmap/$id
cleanup() {
  umount $mnt 2>/dev/null || true
  systemctl stop blkmap@$id 2>/dev/null || true
  [ -n "${httpd:-}" ] && kill $httpd 2>/dev/null || true
}
trap cleanup EXIT
systemctl stop blkmap@$id 2>/dev/null || true
rm -rf $dir /var/lib/blkmap/$id.cow /var/lib/blkmap/$id.cow.bitmap
mkdir -p $dir $mnt
head -c 8388608 /dev/urandom > $dir/part.img
fuser -k 18099/tcp >/dev/null 2>&1 || true
"$rangehttpd" $dir 127.0.0.1:18099 & httpd=$!
sleep 1
cat > /etc/blkmap/$id.yml <<YML
size: 64M
segments:
  - type: zero
    size: 1M
  - type: file
    path: $dir/part.img
  - type: http
    offset: 32M
    url: http://localhost:18099/part.img
YML
echo "== validate"; blkmap validate $id
echo "== start"; systemctl start blkmap@$id; systemctl is-active blkmap@$id
ls -la $dev; readlink $dev
echo "== content"
[ "$(blockdev --getsize64 $dev)" = 67108864 ] && echo "size OK"
cmp <(dd if=$dev bs=1M skip=1 count=8 status=none) $dir/part.img && echo "file segment OK"
cmp <(dd if=$dev bs=1M skip=32 count=8 status=none) $dir/part.img && echo "http segment OK"
cmp <(dd if=$dev bs=1M count=1 status=none) <(head -c 1048576 /dev/zero) && echo "zero segment OK"
cmp <(dd if=$dev bs=1M skip=9 count=23 status=none) <(head -c $((23*1048576)) /dev/zero) && echo "gap OK"
echo "== mkfs + mount + write"
mkfs.ext4 -q $dev
mount $dev $mnt
echo "hello from blkmap $(date)" > $mnt/hello.txt
head -c 3000000 /dev/urandom > $mnt/blob; sha=$(sha256sum $mnt/blob | cut -d' ' -f1)
sync -f $mnt; umount $mnt
echo "== restart"
systemctl stop blkmap@$id; [ ! -e $dev ] && echo "symlink removed on stop"
cmp $dir/part.img <(dd if=$dir/part.img status=none) >/dev/null && echo "source image untouched: $(sha256sum $dir/part.img | cut -d' ' -f1)"
systemctl start blkmap@$id
mount $dev $mnt
cat $mnt/hello.txt
[ "$(sha256sum $mnt/blob | cut -d' ' -f1)" = "$sha" ] && echo "blob survived restart OK"
umount $mnt
systemctl stop blkmap@$id
echo "== cow files"; ls -la /var/lib/blkmap/; du -h /var/lib/blkmap/$id.cow
echo "== journal"; journalctl -u blkmap@$id --no-pager -o cat | tail -8
echo "E2E OK"
