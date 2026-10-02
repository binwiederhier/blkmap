#!/bin/bash
# Soak test against a scratch host: builds and installs the deb, starts scripts/soak-vm.sh
# there detached (an SSH drop cannot end it), and reports every 10 minutes (every minute for short runs) until it ends.
# Usage: soak.sh HOST [MINUTES]   (default 120)
set -euo pipefail
host=${1:?usage: soak.sh HOST [MINUTES]}
minutes=${2:-120}
me="$(cd "$(dirname "$0")" && pwd)"
ssh="ssh -o ConnectTimeout=15 -o BatchMode=yes ${SSH_OPTS:-} root@$host"
cd "$me/.."
goreleaser release --snapshot --clean >/dev/null 2>&1
go build -o dist/rangehttpd ./scripts/rangehttpd
$ssh 'mkdir -p /root/blkmap-soak/scripts /root/blkmap-soak/bin'
scp -q ${SSH_OPTS:-} dist/blkmap_*_linux_amd64.deb root@$host:/root/blkmap-soak/blkmap.deb
scp -q ${SSH_OPTS:-} dist/rangehttpd root@$host:/root/blkmap-soak/bin/
scp -q ${SSH_OPTS:-} scripts/soak-vm.sh root@$host:/root/blkmap-soak/scripts/
$ssh "modprobe ublk_drv; dpkg -i /root/blkmap-soak/blkmap.deb >/dev/null && systemctl daemon-reload
  cd /root/blkmap-soak && nohup scripts/soak-vm.sh $(( minutes * 60 )) > /dev/null 2>&1 < /dev/null &"
echo "soak started on $host for $minutes min; log in /var/tmp/blkmap-soak/soak.log there"
while sleep $(( minutes < 30 ? 60 : 600 )); do
  out=$($ssh 'tail -n 3 /var/tmp/blkmap-soak/soak.log' 2>/dev/null || true)
  echo "--- $(date -u +%H:%M)"; echo "$out"
  grep -qE 'SOAK (OK|FAIL)' <<<"$out" && break
done
$ssh 'grep -E "SOAK (OK|FAIL)" /var/tmp/blkmap-soak/soak.log' | grep -q 'SOAK OK'
