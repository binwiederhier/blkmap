#!/bin/bash
# Starts a Windows guest under QEMU/KVM on the soak host (root), daemonized: q35 with
# Secure Boot OVMF and a TPM 2.0 (swtpm), the disk on NVMe (or AHCI with BUS=ahci) and an
# e1000e NIC (all native to Windows, no drivers needed), user networking with WinRM on
# 15985+SLOT and SQL Server on 11433+SLOT. Control: /run/win/NAME.mon (QEMU monitor),
# NAME.pid. NVMe, because QEMU's AHCI never turns a guest's write-through (FUA) writes into
# flushes: SQL Server commits would reach the disk unflushed. Usage:
#   win-vm.sh NAME SLOT DISK [CDROM...]      env MEM (MiB, 4096), CPUS (4), BUS (nvme|ahci)
#   win-vm.sh stop|reset|kill NAME | shot NAME FILE.png | keys NAME KEY SECONDS
set -euo pipefail
run=/run/win
mon() { echo "$2" | socat - "UNIX-CONNECT:$run/$1.mon" >/dev/null; }
case "${1:-}" in
  stop) mon "$2" system_powerdown; exit ;;
  reset) mon "$2" system_reset; exit ;;
  kill) mon "$2" quit 2>/dev/null || kill "$(cat $run/$2.pid)" 2>/dev/null || true; exit ;;
  shot) mon "$2" "screendump $run/$2.ppm"; sleep 1; pnmtopng "$run/$2.ppm" > "$3" 2>/dev/null; exit ;;
  keys) for _ in $(seq 1 "$4"); do mon "$2" "sendkey $3" 2>/dev/null || true; sleep 1; done; exit ;;
esac
name=$1 slot=$2 disk=$3; shift 3
# swtpm's AppArmor profile allows its state and socket only in libvirt's places
tpmdir=/var/lib/swtpm/$name tpmsock=/run/libvirt/qemu/swtpm/$name.sock
mkdir -p $run /srv/win/vars $tpmdir /run/libvirt/qemu/swtpm
vars=/srv/win/vars/$name.fd
[ -f $vars ] || cp /usr/share/OVMF/OVMF_VARS_4M.ms.fd $vars
pkill -f "swtpm socket --tpmstate dir=$tpmdir " 2>/dev/null || true
swtpm socket --tpmstate dir=$tpmdir --ctrl type=unixio,path=$tpmsock --tpm2 -d
disk_dev=(-device "nvme,drive=d0,serial=blkmap-$name")
[ "${BUS:-nvme}" = ahci ] && disk_dev=(-device ide-hd,drive=d0,bus=ahci.0,rotation_rate=1)
cds=() n=1
for iso in "$@"; do
  cds+=(-drive "file=$iso,if=none,id=cd$n,media=cdrom,readonly=on" -device "ide-cd,drive=cd$n,bus=ahci.$n")
  n=$((n + 1))
done
qemu-system-x86_64 -name "$name" -machine q35,smm=on,accel=kvm -cpu host -smp "${CPUS:-4}" -m "${MEM:-4096}" \
  -global driver=cfi.pflash01,property=secure,value=on \
  -drive if=pflash,format=raw,unit=0,file=/usr/share/OVMF/OVMF_CODE_4M.secboot.fd,readonly=on \
  -drive if=pflash,format=raw,unit=1,file=$vars \
  -chardev socket,id=chrtpm,path=$tpmsock -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-tis,tpmdev=tpm0 \
  -device ahci,id=ahci \
  -drive "file=$disk,if=none,id=d0,format=raw,cache=none,aio=io_uring,discard=unmap" "${disk_dev[@]}" \
  "${cds[@]}" \
  -netdev "user,id=n0,hostfwd=tcp::$((15985 + slot))-:5985,hostfwd=tcp::$((11433 + slot))-:1433" -device e1000e,netdev=n0 \
  -rtc base=utc -vga std -display none -monitor "unix:$run/$name.mon,server,nowait" \
  -daemonize -pidfile "$run/$name.pid"
