#!/bin/bash
# Run the root-only suites against a throwaway VM instead of this machine: a bug in the ublk
# layer can wedge a kernel for good (see README), and a scratch VM can simply be rebooted.
# Builds the deb and the root test binaries here, ships them with the scripts, and runs
# ublk + device integration tests, e2e and stress there. Usage: remote-test.sh HOST [stress]
set -euo pipefail
host=${1:?usage: remote-test.sh HOST [stress]}
me="$(cd "$(dirname "$0")" && pwd)"
root="$me/.."
ssh="ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 root@$host"
cd "$root"
goreleaser release --snapshot --clean >/dev/null 2>&1
go test -c -o dist/ublk.test ./ublk/
go test -c -o dist/device.test ./device/
go build -o dist/rangehttpd ./scripts/rangehttpd
deb=$(ls dist/blkmap_*_linux_amd64.deb)
$ssh 'mkdir -p /root/blkmap-test/scripts /root/blkmap-test/bin'
scp -q "$deb" dist/ublk.test dist/device.test root@$host:/root/blkmap-test/
scp -q dist/rangehttpd root@$host:/root/blkmap-test/bin/
scp -q scripts/e2e.sh scripts/stress.sh scripts/mkraid5.py root@$host:/root/blkmap-test/scripts/
$ssh "set -e; cd /root/blkmap-test; modprobe ublk_drv; dpkg -i $(basename "$deb") >/dev/null; echo '== ublk tests'; ./ublk.test 2>&1 | tail -3; echo '== device tests'; ./device.test 2>&1 | tail -3; echo '== e2e'; scripts/e2e.sh 2>&1 | grep -E 'OK|FAIL|rror' | tail -4"
if [ "${2:-}" = "stress" ]; then
  $ssh "cd /root/blkmap-test && scripts/stress.sh >/dev/null 2>&1; cat /var/tmp/blkmap-stress/results.txt"
fi
