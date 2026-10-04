#!/bin/bash
# Write errors on the filesystem under the COW file, against a scratch VM: see
# faults-run.sh. Needs root SSH to HOST. Usage: faults.sh HOST
set -euo pipefail
host=${1:?usage: faults.sh HOST}
me="$(cd "$(dirname "$0")" && pwd)"
ssh="ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 ${SSH_OPTS:-} root@$host"
cd "$me/.."
goreleaser release --snapshot --clean >/dev/null 2>&1
go build -o dist/powercut ./scripts/powercut
deb=$(ls dist/blkmap_*_linux_amd64.deb)
$ssh 'mkdir -p /root/blkmap-test/scripts /root/blkmap-test/bin'
scp -q ${SSH_OPTS:-} "$deb" root@$host:/root/blkmap-test/
scp -q ${SSH_OPTS:-} dist/powercut root@$host:/root/blkmap-test/bin/
scp -q ${SSH_OPTS:-} scripts/faults-run.sh root@$host:/root/blkmap-test/scripts/
steps="modprobe ublk_drv; modprobe dm-flakey; dpkg -i $(basename "$deb") >/dev/null; scripts/faults-run.sh"
$ssh "cd /root/blkmap-test && rm -f faults.log && nohup bash -c \"$steps; echo '== done'\" > faults.log 2>&1 < /dev/null &"
$ssh "cd /root/blkmap-test && tail -n +1 -F faults.log 2>/dev/null | sed '/^== done/q'"
$ssh "grep -q '^FAULTS OK' /root/blkmap-test/faults.log"
