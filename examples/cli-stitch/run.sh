#!/bin/bash
# Stitch zero + file + http into /dev/blkmap/stitch with "blkmap serve" (no systemd), check
# the content, format it, write a file, restart the server, and show the file survived in the
# COW overlay. Needs root, the blkmap deb, and python3.
set -euo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
# The HTTP origin: the Go test server from scripts/rangehttpd (built into dist/ locally,
# shipped as bin/ on a test VM)
rangehttpd=""; for c in "$me/../../dist/rangehttpd" "$me/../../bin/rangehttpd"; do [ -x "$c" ] && rangehttpd=$c && break; done
[ -x "$rangehttpd" ] || { echo "rangehttpd not found: go build -o dist/rangehttpd ./scripts/rangehttpd" >&2; exit 1; }
dir=/var/tmp/blkmap-example-stitch
mnt=$dir/mnt
cleanup() {
  umount $mnt 2>/dev/null || true
  [ -n "${srv:-}" ] && kill $srv 2>/dev/null || true
  [ -n "${httpd:-}" ] && kill $httpd 2>/dev/null || true
}
trap cleanup EXIT
rm -rf $dir; mkdir -p $dir $mnt
head -c $((8<<20)) /dev/urandom > $dir/part.img
"$rangehttpd" $dir 127.0.0.1:18100 & httpd=$!
sleep 1

echo "== validate (opens every source, prints the layout)"
blkmap validate "$me/stitch.yml"

echo "== serve"
blkmap serve --config "$me/stitch.yml" stitch & srv=$!
for i in $(seq 1 50); do [ -e /dev/blkmap/stitch ] && break; sleep 0.1; done
ls -la /dev/blkmap/stitch
cmp <(dd if=/dev/blkmap/stitch bs=1M skip=1 count=8 status=none) $dir/part.img && echo "file segment reads back the image"
cmp <(dd if=/dev/blkmap/stitch bs=1M skip=32 count=8 status=none) $dir/part.img && echo "http segment reads back the image"

echo "== format, mount, write"
mkfs.ext4 -q /dev/blkmap/stitch
mount /dev/blkmap/stitch $mnt
echo "hello from blkmap" > $mnt/hello.txt
umount $mnt

echo "== restart the server: the write is in the cow file, the image is untouched"
kill $srv; wait $srv || true; srv=
blkmap serve --config "$me/stitch.yml" stitch & srv=$!
for i in $(seq 1 50); do [ -e /dev/blkmap/stitch ] && break; sleep 0.1; done
mount /dev/blkmap/stitch $mnt
cat $mnt/hello.txt
umount $mnt
ls -la $dir/stitch.cow $dir/stitch.cow.bitmap
kill $srv; wait $srv || true; srv=
echo "done"
