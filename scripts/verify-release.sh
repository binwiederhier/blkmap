#!/bin/bash
# Release verification, unattended and in sequence: for every kernel template, a fresh VM is
# created, the whole suite runs on it (scripts/ci-vm.sh: integration, e2e, stress, scenarios,
# daemon kills, power cuts), the same VM is then soaked for SOAK minutes, and the VM is
# destroyed. One heavy job at a time: box11 has one NVMe. A failure in one template does not
# stop the others. The summary goes to stdout and logs/verify-release-<date>.md, logs to
# logs/verify-*.log (not dist/, which every package build empties). Usage, meant for nohup:
#   nohup scripts/verify-release.sh [SOAK_MINUTES] > logs/verify.log 2>&1 &
# Env: TEMPLATES="9000 9002 9003" (26.04/7.0, 24.04/6.8, 22.04 HWE 6.8 systemd 249), PROXMOX=root@box11,
# SOAK_TEMPLATES="9000 9002" (which templates also get the soak; 9003 is kernel 6.8 again).
set -uo pipefail
soak=${1:-120}
templates=${TEMPLATES:-"9000 9002 9003"}
soakTemplates=${SOAK_TEMPLATES:-"9000 9002"}
pve=${PROXMOX:-root@box11}
me="$(cd "$(dirname "$0")" && pwd)"
cd "$me/.."
mkdir -p logs
stamp=$(date +%Y%m%d-%H%M)
out=logs/verify-release-$stamp.md
pssh() { ssh -o ConnectTimeout=10 -o BatchMode=yes "$pve" "$@"; }
declare -a rows
note() { echo "$(date +%H:%M:%S) $*"; }
for t in $templates; do
  log=logs/verify-$t-testvm-$stamp.log
  note "== template $t: make test-vm (KEEP=1)"
  KEEP=1 TEMPLATE=$t PROXMOX=$pve scripts/ci-vm.sh > "$log" 2>&1
  suite=FAIL; grep -q '== ALL PASSED' "$log" && suite=PASS
  kernel=$(grep -o 'ALL PASSED on .*' "$log" | sed 's/ALL PASSED on //'); kernel=${kernel:-unknown}
  vmid=$(grep -o 'keeping VM [0-9]*' "$log" | awk '{print $3}'); ip=$(grep -o 'keeping VM [0-9]* ([0-9.]*)' "$log" | tr -d '()' | awk '{print $4}')
  note "   suite: $suite ($kernel), vm $vmid at $ip"
  soakres="not run"
  if [ "$suite" = PASS ] && [ -n "$ip" ] && [[ " $soakTemplates " == *" $t "* ]]; then
    slog=logs/verify-$t-soak-$stamp.log
    note "   soak $soak min on $ip"
    if scripts/soak.sh "$ip" "$soak" > "$slog" 2>&1; then soakres="SOAK OK"; else soakres="SOAK FAIL"; fi
    note "   $soakres"
  fi
  if [ -n "$vmid" ]; then
    pssh "qm stop $vmid --skiplock --timeout 15 >/dev/null 2>&1; qm destroy $vmid --purge >/dev/null 2>&1" || note "   could not destroy VM $vmid; do it by hand"
  fi
  rows+=("| $t | $kernel | $suite | $soakres | $log |")
done
{
  echo "# Release verification $(date +%Y-%m-%d)"; echo
  echo "Commit: $(git describe --always --dirty); soak ${soak} min per soaked template."; echo
  echo "| Template | Kernel | Suite (test-vm) | Soak | Log |"; echo "|---|---|---|---|---|"
  printf '%s\n' "${rows[@]}"
} | tee "$out"
note "written to $out"
for r in "${rows[@]}"; do case "$r" in *"| FAIL |"*|*"SOAK FAIL"*) exit 1;; esac; done
