#!/bin/bash
# Run the root-only suites against a throwaway VM instead of this machine: a bug in the ublk
# layer can wedge a kernel for good (see README), and a scratch VM can simply be rebooted.
# Builds the deb and the root test binaries here, ships them with the scripts, and runs
# ublk + device integration tests and e2e there, plus the stress workloads and/or the
# real-life scenarios. Usage: remote-test.sh HOST [stress|scenarios|all]
set -euo pipefail
host=${1:?usage: remote-test.sh HOST [stress|scenarios|all]}
suite=${2:-}
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
scp -q scripts/e2e.sh scripts/stress.sh scripts/scenarios.sh scripts/mkraid5.py root@$host:/root/blkmap-test/scripts/
# Everything runs detached on the host (a dropped ssh session must not kill a scenario
# halfway, which would leave devices behind); this side follows the log until it ends.
steps="modprobe ublk_drv; dpkg -i $(basename "$deb") >/dev/null"
steps="$steps; echo '== ublk tests'; ./ublk.test -test.timeout 5m 2>&1 | tail -3"
steps="$steps; echo '== device tests'; ./device.test -test.timeout 5m 2>&1 | tail -3"
steps="$steps; echo '== e2e'; scripts/e2e.sh 2>&1 | grep -E 'OK|FAIL|rror' | tail -4"
if [ "$suite" = stress ] || [ "$suite" = all ]; then
  steps="$steps; echo '== stress'; scripts/stress.sh >/dev/null 2>&1; cat /var/tmp/blkmap-stress/results.txt"
fi
if [ "$suite" = scenarios ] || [ "$suite" = all ]; then
  steps="$steps; echo '== scenarios'; scripts/scenarios.sh 2>&1 | grep -E '^(===|PASS|FAIL|passed|  )'"
fi
$ssh "cd /root/blkmap-test && rm -f run.log && nohup bash -c \"$steps; echo '== done'\" > run.log 2>&1 < /dev/null &"
$ssh "cd /root/blkmap-test && tail -n +1 -F run.log 2>/dev/null | sed '/^== done/q'"
