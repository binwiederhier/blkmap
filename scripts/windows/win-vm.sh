#!/bin/bash
# Starts a Windows guest under QEMU/KVM on the soak host (root), daemonized: q35 with
# Secure Boot OVMF and a TPM 2.0 (swtpm), the disk on VirtIO SCSI (BUS=ahci or nvme for
# images without the vioscsi driver) and an e1000e NIC, user networking with WinRM on
# 15985+SLOT and SQL Server on 11433+SLOT. Control: /run/win/NAME.mon (QEMU monitor),
# NAME.pid. VirtIO SCSI, because QEMU's AHCI and NVMe emulation never turn a guest's
# write-through (FUA) writes into flushes: SQL Server's commits would reach the disk
# unflushed (measured: 2,083 commits a minute against 17 flushes on AHCI, 446 against 121 on
# NVMe, 300 against 520 on VirtIO SCSI). Usage:
#   win-vm.sh NAME SLOT DISK [CDROM...]      env MEM (MiB, 4096), CPUS (4), BUS (scsi|ahci|nvme)
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
disk_dev=(-device virtio-scsi-pci,id=scsi0 -device scsi-hd,drive=d0,bus=scsi0.0)
[ "${BUS:-scsi}" = ahci ] && disk_dev=(-device ide-hd,drive=d0,bus=ahci.0,rotation_rate=1)
[ "${BUS:-scsi}" = nvme ] && disk_dev=(-device "nvme,drive=d0,serial=blkmap-$name")
# SCSI_PROBE=1 adds a small VirtIO SCSI disk, so Windows installs vioscsi as a boot driver
# before the boot disk moves there
if [ -n "${SCSI_PROBE:-}" ]; then
  [ -f /srv/win/scsi-probe.img ] || truncate -s 64M /srv/win/scsi-probe.img
  disk_dev+=(-device virtio-scsi-pci,id=scsi9 -drive file=/srv/win/scsi-probe.img,if=none,id=probe,format=raw -device scsi-hd,drive=probe,bus=scsi9.0)
fi
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
