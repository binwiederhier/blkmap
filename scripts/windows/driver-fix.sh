#!/bin/bash
# Makes a golden image built before setup.ps1 installed vioscsi boot from VirtIO SCSI (root,
# soak VM): boots it on its current controller with a probe VirtIO SCSI disk and the
# virtio-win ISO, installs the driver (vioscsi.ps1), then boots it from VirtIO SCSI to prove
# it, checks SQL Server, and leaves it read-only. Usage: driver-fix.sh VERSION [SLOT] [FROM]
#   FROM: the controller the image boots from now (nvme by default; ahci for fresh builds)
set -uo pipefail
v=${1:?usage: driver-fix.sh VERSION [SLOT] [FROM]} slot=${2:-0} from=${3:-nvme}
me="$(cd "$(dirname "$0")" && pwd)"
img=/srv/win/golden/$v.raw name=fix-$v port=$((15985 + slot))
[ -e $img ] || img=$img.part
wr() { /srv/win/venv/bin/python $me/wr.py $port "$1"; }
up() { for _ in $(seq 1 120); do WR_TIMEOUT=20 timeout 60 /srv/win/venv/bin/python $me/wr.py $port hostname >/dev/null 2>&1 && return 0; sleep 10; done; return 1; }
down() { for _ in $(seq 1 90); do kill -0 "$(cat /run/win/$name.pid 2>/dev/null)" 2>/dev/null || return 0; sleep 5; done; $me/win-vm.sh kill $name; return 1; }
chmod 644 $img
BUS=$from SCSI_PROBE=1 MEM=4096 CPUS=3 $me/win-vm.sh $name $slot $img /srv/win/iso/virtio-win.iso
up || { echo "FAIL: $v did not come up on $from"; $me/win-vm.sh kill $name; exit 1; }
out=$(wr "@$me/vioscsi.ps1" 2>&1)
wr 'Stop-Computer -Force' >/dev/null 2>&1
down || { echo "FAIL: $v did not shut down"; exit 1; }
BUS=scsi MEM=4096 CPUS=3 $me/win-vm.sh $name $slot $img
up || { echo "FAIL: $v does not boot from VirtIO SCSI ($(echo "$out" | tail -n 1))"; $me/win-vm.sh shot $name /srv/win/$name.png; $me/win-vm.sh kill $name; exit 1; }
sql=$(wr '(Get-Service MSSQLSERVER).WaitForStatus("Running", "00:05:00"); (Get-Service MSSQLSERVER).Status; Stop-Computer -Force' 2>&1)
down
[[ $img == *.part ]] || chmod 444 $img
echo "$v boots from VirtIO SCSI; $(echo "$out" | tail -n 1 | tr -d '\r'); SQL Server: $(echo $sql | tr -d '\r')"
