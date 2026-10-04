#!/bin/bash
# Crash-point replay against a scratch VM: power cuts test that a device survives, this
# tests every crash state in between. The COW file and bitmap live on a filesystem on
# dm-log-writes; each logged flush is replayed with a random part of the writes that were
# not yet flushed, and a device started from that state must hold every acknowledged record.
# MODE fs does the same with a guest filesystem over a hydrating sparse base (see
# crashreplay-run.sh). Needs root SSH to HOST. Usage: crashreplay.sh HOST [MODE] [N] [CHECKS]
set -euo pipefail
host=${1:?usage: crashreplay.sh HOST [records|fs] [N] [CHECKS]}
mode=${2:-records}
n=${3:-3000}
checks=${4:-0}
me="$(cd "$(dirname "$0")" && pwd)"
ssh="ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 ${SSH_OPTS:-} root@$host"
cd "$me/.."
goreleaser release --snapshot --clean >/dev/null 2>&1
go build -o dist/powercut ./scripts/powercut
go build -o dist/logreplay ./scripts/logreplay
deb=$(ls dist/blkmap_*_linux_amd64.deb)
$ssh 'mkdir -p /root/blkmap-test/scripts /root/blkmap-test/bin'
scp -q ${SSH_OPTS:-} "$deb" root@$host:/root/blkmap-test/
scp -q ${SSH_OPTS:-} dist/powercut dist/logreplay root@$host:/root/blkmap-test/bin/
scp -q ${SSH_OPTS:-} scripts/crashreplay-run.sh root@$host:/root/blkmap-test/scripts/
# Detached on the host, so a dropped session cannot leave devices or mounts behind
steps="modprobe ublk_drv; modprobe dm-log-writes; dpkg -i $(basename "$deb") >/dev/null; scripts/crashreplay-run.sh $mode $n $checks"
$ssh "cd /root/blkmap-test && rm -f crash.log && nohup bash -c \"$steps; echo '== done'\" > crash.log 2>&1 < /dev/null &"
$ssh "cd /root/blkmap-test && tail -n +1 -F crash.log 2>/dev/null | sed '/^== done/q'"
$ssh "grep -q '^CRASHREPLAY OK' /root/blkmap-test/crash.log"
