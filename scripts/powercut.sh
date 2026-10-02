#!/bin/bash
# Power-loss and crash test against a scratch VM. A writer stores checksummed records on a
# blkmap device and flushes after every batch; this side records what was acknowledged.
#   power: the VM loses power mid-write (sysrq reboot: guest RAM gone, nothing synced);
#          after it comes back, every acknowledged record must have survived.
#   kill:  the blkmap daemon is SIGKILLed mid-write, several times per cycle; systemd
#          restarts it, it re-attaches, and the writer must never see an error.
# Needs root SSH to HOST and the blkmap deb installed there. Usage:
#   powercut.sh HOST [CYCLES] [power|kill]
set -euo pipefail
host=${1:?usage: powercut.sh HOST [CYCLES] [power|kill]}
cycles=${2:-10}
mode=${3:-power}
me="$(cd "$(dirname "$0")" && pwd)"
work=$(mktemp -d)
ssh="ssh -o ConnectTimeout=5 -o BatchMode=yes -o ServerAliveInterval=2 -o ServerAliveCountMax=3 ${SSH_OPTS:-} root@$host"
dev=/dev/blkmap/pc
spans=()

up() { for _ in $(seq 1 60); do $ssh true 2>/dev/null && return 0; sleep 3; done; echo "FAIL: $host did not come back" >&2; exit 1; }
start_device() {
  $ssh "systemctl start blkmap@pc && for i in \$(seq 1 50); do [ -e $dev ] && exit 0; sleep 0.1; done; exit 1"
}
verify() {
  [ ${#spans[@]} -eq 0 ] && return 0
  $ssh "/root/powercut verify $dev ${spans[*]}" || { echo "FAIL: acknowledged writes lost (cycle $1)"; exit 1; }
}

(cd "$me/.." && go build -o "$work/powercut" ./scripts/powercut)
scp -q ${SSH_OPTS:-} "$work/powercut" root@$host:/root/powercut
$ssh "systemctl stop blkmap@pc 2>/dev/null; rm -f /var/lib/blkmap/pc.cow /var/lib/blkmap/pc.cow.bitmap
  printf 'size: 1G\nsegments:\n  - type: zero\n    size: 1G\n' > /etc/blkmap/pc.yml
  echo 1 > /proc/sys/kernel/sysrq"
for i in $(seq 1 "$cycles"); do
  start_device
  verify "$i"
  out="$work/cycle.$i"
  $ssh "/root/powercut write $dev" > "$out" 2>&1 &
  writer=$!
  sleep $((3 + RANDOM % 8))
  if [ "$mode" = power ]; then
    $ssh 'echo b > /proc/sysrq-trigger' >/dev/null 2>&1 &
    sleep 5; kill $writer 2>/dev/null || true; wait $writer 2>/dev/null || true
    up
  else
    for k in 1 2 3; do
      $ssh 'kill -9 $(systemctl show -p MainPID --value blkmap@pc)'
      sleep $((2 + RANDOM % 3))
    done
    $ssh 'pkill -x powercut' || true
    wait $writer 2>/dev/null || true
    if grep -q '^powercut:' "$out"; then echo "FAIL: the writer saw an error across daemon kills (cycle $i):"; grep '^powercut:' "$out"; exit 1; fi
  fi
  first=$(grep -m1 '^start ' "$out" | cut -d' ' -f2 || true)
  last=$(grep '^ack ' "$out" | tail -1 | cut -d' ' -f2 || true)
  [ -n "$first" ] && [ -n "$last" ] && spans+=("$first-$last")
  echo "cycle $i ($mode): wrote from ${first:-?}, acknowledged up to ${last:-nothing}"
done
start_device
verify final
$ssh "systemctl stop blkmap@pc"
echo "OK: $cycles $mode cycles, every acknowledged write survived"
rm -rf "$work"
