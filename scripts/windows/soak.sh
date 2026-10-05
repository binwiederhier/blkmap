#!/bin/bash
# Windows soak on blkmap (plan: ~/Code/plans/261004-blkmap-windows-soak.md). Runs on a
# machine outside the crash domain (codebox): starts one blkmap device and one Windows guest
# per GUEST on HOST (the soak VM, VM 900 on box12) over its golden image, drives sqlsoak
# against each, and every 10-20 minutes disrupts one: kill -9 or reload of its blkmap
# server, a hard reset of the guest, or a power cut of the whole soak VM. After each,
# SQL Server must come back with every acknowledged commit, CHECKDB and chkdsk clean.
# Usage: soak.sh HOURS VERSION... (VERSION: a golden image; slot = position)
#   env HOST (192.168.1.223), PVE (root@box12), VMID (900), GUEST_MEM (MiB, 4096)
set -uo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
# Run from a private copy: bash reads scripts as it goes, and a soak runs for days
[ -n "${SOAK_COPY:-}" ] || { c=$(mktemp /tmp/soak.XXXX.sh); cp "$0" "$c"; SOAK_COPY=1 SOAK_DIR=$me exec bash "$c" "$@"; }
me=${SOAK_DIR:-$me}
hours=${1:?usage: soak.sh HOURS VERSION...}; shift
guests=("$@")
host=${HOST:-192.168.1.223} pve=${PVE:-root@box12} vmid=${VMID:-900}
logs="$me/../../logs/win-$(date +%Y%m%d-%H%M)"
mkdir -p "$logs"
ssh="ssh -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o BatchMode=yes root@$host"
sqlsoak="$logs/sqlsoak"
(cd "$me/sqlsoak" && go build -o "$sqlsoak" .) || exit 1
declare -A clients=() acks=()
failures=0 events=0
log() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$logs/soak.log"; }
fail() { log "FAIL $*"; failures=$((failures + 1)); }
slot() { local i; for i in "${!guests[@]}"; do [ "${guests[$i]}" = "$1" ] && echo $i; done; }
up() { for _ in $(seq 1 90); do $ssh true 2>/dev/null && return 0; sleep 5; done; return 1; }

# guest_start V: blkmap device (enabled, so it comes back after a power cut) and the guest
guest_start() {
  local v=$1 s; s=$(slot $1)
  $ssh "test -e /etc/blkmap/win-$v.yml || printf 'cow:\n  file: /var/lib/blkmap/win-$v.cow\nsegments:\n  - type: file\n    path: /srv/win/golden/$v.raw\n' > /etc/blkmap/win-$v.yml
    systemctl enable -q blkmap@win-$v; systemctl start blkmap@win-$v
    for i in \$(seq 1 100); do [ -e /dev/blkmap/win-$v ] && break; sleep 0.1; done
    kill -0 \$(cat /run/win/$v.pid 2>/dev/null) 2>/dev/null || MEM=${GUEST_MEM:-4096} CPUS=3 /srv/win/bin/win-vm.sh $v $s /dev/blkmap/win-$v" || fail "$v: start"
}
# sql_ready V: wait until SQL Server in guest V answers (a Windows boot, crash recovery)
sql_ready() {
  local s; s=$(slot $1)
  for _ in $(seq 1 180); do
    "$sqlsoak" init -addr "$host:$((11433 + s))" >/dev/null 2>&1 && return 0
    sleep 10
  done
  return 1
}
client_start() {
  local v=$1 s; s=$(slot $1)
  "$sqlsoak" run -addr "$host:$((11433 + s))" >> "$logs/$v.acks" 2>&1 &
  clients[$v]=$!
}
client_stop() {
  local v=$1
  [ -n "${clients[$v]:-}" ] && { kill "${clients[$v]}" 2>/dev/null; wait "${clients[$v]}" 2>/dev/null; }
  clients[$v]=""
  acks[$v]=$(awk '/^ack /{v=$2} END{print v}' "$logs/$v.acks" 2>/dev/null)
}
# check V WHY: SQL back, every acknowledged commit there, CHECKDB and chkdsk clean
check() {
  local v=$1 why=$2 s out; s=$(slot $1)
  client_stop $v
  sql_ready $v || { fail "$v after $why: SQL Server did not come back in 30 min"; $ssh "/srv/win/bin/win-vm.sh shot $v /srv/win/$v-stuck.png"; return; }
  if out=$("$sqlsoak" verify -addr "$host:$((11433 + s))" -acked "${acks[$v]:--1}" 2>&1); then
    log "PASS $v after $why: $out"
  else
    fail "$v after $why: $out"
  fi
  out=$($ssh "/srv/win/venv/bin/python /srv/win/bin/wr.py $((15985 + s)) 'chkdsk C: /scan'" 2>&1)
  grep -qiE "found no problems|no further action is required" <<<"$out" || fail "$v after $why: chkdsk: $(tr -s '\r\n' ' ' <<<"$out" | tail -c 400)"
  client_start $v
}

trap 'for v in "${guests[@]}"; do client_stop $v; done' EXIT
log "soak of ${guests[*]} on $host for $hours h; logs in $logs"
$ssh 'modprobe ublk_drv' || exit 1
for v in "${guests[@]}"; do guest_start $v; done
for v in "${guests[@]}"; do
  sql_ready $v || { fail "$v: SQL Server never came up"; exit 1; }
  log "$v up"; client_start $v
done
end=$(( $(date +%s) + hours * 3600 )) next_sample=0
while [ "$(date +%s)" -lt "$end" ]; do
  sleep $(( 600 + RANDOM % 600 ))
  if [ "$(date +%s)" -ge "$next_sample" ]; then
    log "sample: $($ssh 'for p in $(pgrep -f "blkmap serve"); do echo -n "$(tr "\0" " " < /proc/$p/cmdline | cut -d" " -f3) rss $(awk "/VmRSS/{print \$2}" /proc/$p/status)k fds $(ls /proc/$p/fd | wc -l); "; done; free -m | awk "/Mem/{print \"host free \" \$7 \"M\"}"')"
    next_sample=$(( $(date +%s) + 3600 ))
  fi
  v=${guests[$((RANDOM % ${#guests[@]}))]}
  events=$((events + 1))
  case $((RANDOM % 10)) in
    0|1|2) log "event $events: kill -9 of blkmap@win-$v"; $ssh "systemctl kill -s KILL blkmap@win-$v"; sleep 20; check $v "kill -9" ;;
    3|4) log "event $events: reload of blkmap@win-$v"; $ssh "systemctl reload blkmap@win-$v"; sleep 20; check $v "reload" ;;
    5|6|7) log "event $events: hard reset of guest $v"; $ssh "/srv/win/bin/win-vm.sh reset $v"; check $v "guest reset" ;;
    8|9)
      log "event $events: power cut of the soak VM"
      for g in "${guests[@]}"; do client_stop $g; done
      ssh -o BatchMode=yes $pve "qm stop $vmid --skiplock --timeout 30 >/dev/null 2>&1; qm start $vmid >/dev/null"
      up || { fail "the soak VM did not come back"; exit 1; }
      $ssh 'modprobe ublk_drv'
      for g in "${guests[@]}"; do guest_start $g; done
      for g in "${guests[@]}"; do clients[$g]=""; check $g "power cut"; done ;;
  esac
done
for v in "${guests[@]}"; do check $v "the end"; done
log "$events events, $failures failures"
[ $failures = 0 ] && log "WINDOWS SOAK OK" || log "WINDOWS SOAK FAIL"
