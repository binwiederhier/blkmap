#!/bin/bash
# Statement coverage of the unit tests plus the root-only ublk and device tests, which run
# on a scratch VM (they need a kernel to break). Writes dist/coverage.out, dist/coverage.html
# and prints the weakest functions of the packages that matter. Usage: coverage.sh HOST
set -euo pipefail
host=${1:?usage: coverage.sh HOST}
me="$(cd "$(dirname "$0")" && pwd)"
ssh="ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 ${SSH_OPTS:-} root@$host"
cd "$me/.."
mkdir -p dist
pkgs=./cow/...,./device/...,./source/...,./ublk/...,./config/...,./util/...,./cmd/...
go test -coverpkg=$pkgs -coverprofile=dist/cover-unit.out ./... >/dev/null
go test -c -cover -coverpkg=$pkgs -o dist/ublk-cover.test ./ublk/
go test -c -cover -coverpkg=$pkgs -o dist/device-cover.test ./device/
$ssh 'mkdir -p /root/blkmap-test && modprobe ublk_drv'
scp -q ${SSH_OPTS:-} dist/ublk-cover.test dist/device-cover.test root@$host:/root/blkmap-test/
for t in ublk device; do
  $ssh "cd /root/blkmap-test && ./$t-cover.test -test.timeout 10m -test.coverprofile=/root/blkmap-test/cover-$t.out >/dev/null 2>&1" || { echo "FAIL: $t tests"; exit 1; }
  scp -q ${SSH_OPTS:-} root@$host:/root/blkmap-test/cover-$t.out dist/
done
# Text profiles concatenate: the cover tool merges the blocks they share
{ echo "mode: set"; for f in dist/cover-unit.out dist/cover-ublk.out dist/cover-device.out; do grep -hv '^mode:' "$f"; done; } > dist/coverage.out
go tool cover -html=dist/coverage.out -o dist/coverage.html
for p in cow device source ublk config util cmd; do
  printf '%-8s %s\n' "$p" "$(go tool cover -func=dist/coverage.out | awk -v p="heckel.io/blkmap/$p/" 'index($1, p) == 1 { n++; gsub("%", "", $3); s += $3 } END { printf "%.1f%% (mean of %d functions)", s / n, n }')"
done
go tool cover -func=dist/coverage.out | tail -1
echo "functions under 50% in cow, device, ublk:"
go tool cover -func=dist/coverage.out | grep -E '/(cow|device|ublk)/' | awk '{ v = $3; gsub("%", "", v); if (v + 0 < 50) print "  " $0 }'
