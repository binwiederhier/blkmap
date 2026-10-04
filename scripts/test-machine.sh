#!/bin/bash
# The whole test suite against one machine, with a summary at the end: unit tests here, then
# on HOST (root SSH, ublk_drv, fio): the ublk and device integration tests, e2e through the
# deb and systemd, the stress workloads, every real-life scenario, daemon kills under a
# verifying writer, and optionally power cuts (HOST reboots) and a soak. HOST must be a
# throwaway machine: a transport bug can wedge its kernel. Usage:
#   test-machine.sh HOST [--power [CYCLES]] [--soak MINUTES]
# The summary names the test-plan sections (docs/test-plan.md) each layer covers and is also
# written to logs/test-machine-<host>-<date>.md, ready for docs/test-results/ (not dist/, which
# the package build empties).
set -uo pipefail
host=${1:?usage: test-machine.sh HOST [--power [CYCLES]] [--soak MINUTES]}; shift
power=""; soak=""
while [ $# -gt 0 ]; do
  case "$1" in
    --power) power=5; if [[ "${2:-}" =~ ^[0-9]+$ ]]; then power=$2; shift; fi ;;
    --soak) soak=${2:?--soak needs MINUTES}; shift ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
  shift
done
me="$(cd "$(dirname "$0")" && pwd)"
cd "$me/.."
out=logs/test-machine-$host-$(date +%Y%m%d-%H%M).md
mkdir -p logs
declare -a names results covers
run() { # run NAME "PLAN SECTIONS" CMD...: run a layer, record its verdict, keep going
  local name=$1 plan=$2; shift 2
  echo; echo "#### $name"; local start=$(date +%s)
  if "$@" 2>&1 | tee "logs/test-machine-${name// /-}.log" | tail -40; then r=PASS; else r=FAIL; fi
  names+=("$name"); results+=("$r ($(( $(date +%s) - start )) s)"); covers+=("$plan")
}
run "unit tests" "all packages (TP-U)" go test -race ./...
run "examples" "library use (TP-L)" make examples
run "integration, e2e, stress, scenarios" "TP-C, TP-S, TP-O, TP-R, TP-H, TP-F, TP-X" scripts/remote-test.sh "$host" all
run "daemon kills under a verifying writer" "TP-R2" scripts/powercut.sh "$host" 3 kill
[ -n "$power" ] && run "power cuts under a verifying writer" "TP-R3" scripts/powercut.sh "$host" "$power" power
[ -n "$soak" ] && run "soak, $soak min of chaos" "TP-R4" scripts/soak.sh "$host" "$soak"
kernel=$(ssh -o BatchMode=yes "root@$host" 'uname -r; . /etc/os-release; echo "$PRETTY_NAME"; blkmap --version' 2>/dev/null | tr '\n' ' ')
{
  echo "# Test run $(date +%Y-%m-%d) on $host"; echo
  echo "Target: $kernel"; echo "Commit: $(git describe --always --dirty)"; echo
  echo "| Layer | Result | Test plan |"; echo "|---|---|---|"
  for i in "${!names[@]}"; do echo "| ${names[$i]} | ${results[$i]} | ${covers[$i]} |"; done
} | tee "$out"
echo; echo "written to $out"
for r in "${results[@]}"; do case "$r" in FAIL*) exit 1;; esac; done
