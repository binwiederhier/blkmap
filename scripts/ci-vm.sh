#!/bin/bash
# The whole root-level test suite on a throwaway VM: clones a cloud-init Ubuntu template on
# a Proxmox host, provisions it (ublk_drv, fio, filesystems), runs the ublk and device
# integration tests, e2e, stress, the real-life scenarios, daemon-kill and power-cut
# cycles, and destroys the VM, pass or fail. A wedged kernel cannot outlive the run.
# Usage: ci-vm.sh   (PROXMOX=root@box11 TEMPLATE=9000 KEEP=1 to leave the VM for debugging)
set -euo pipefail
pve=${PROXMOX:-root@box11}
template=${TEMPLATE:-9000}
me="$(cd "$(dirname "$0")" && pwd)"
export SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
pssh() { ssh -o ConnectTimeout=10 -o BatchMode=yes "$pve" "$@"; }
vmid=$(pssh pvesh get /cluster/nextid)
cleanup() {
  [ -n "${KEEP:-}" ] && { echo "keeping VM $vmid ($ip)"; return; }
  pssh "qm stop $vmid --skiplock --timeout 15 >/dev/null 2>&1; qm destroy $vmid --purge >/dev/null 2>&1" || true
}
ip=""
trap cleanup EXIT
echo "== VM $vmid from template $template on $pve"
pssh "qm clone $template $vmid --name blkmap-ci-$vmid >/dev/null && qm start $vmid"
for _ in $(seq 1 60); do
  ip=$(pssh "qm guest cmd $vmid network-get-interfaces 2>/dev/null" | grep -o '"ip-address" *: *"[0-9.]*"' | grep -v '"127\.' | head -1 | grep -o '[0-9.]*[0-9]' || true)
  [ -n "$ip" ] && break; sleep 5
done
[ -n "$ip" ] || { echo "FAIL: VM $vmid got no address"; exit 1; }
user=$(pssh "qm config $template" | sed -n 's/^ciuser: //p')
for _ in $(seq 1 60); do ssh $SSH_OPTS -o ConnectTimeout=5 -o BatchMode=yes "${user:-root}@$ip" true 2>/dev/null && break; sleep 5; done
echo "== provisioning $ip"
ssh $SSH_OPTS -o BatchMode=yes "${user:-root}@$ip" 'cloud-init status --wait >/dev/null 2>&1; sudo install -d -m700 /root/.ssh && sudo install -m600 ~/.ssh/authorized_keys /root/.ssh/authorized_keys'
# ublk_drv ships in linux-modules-extra; a template's kernel may be older than the archive
# still carries modules for, so install the current kernel with them and boot into it
ssh $SSH_OPTS -o BatchMode=yes "root@$ip" 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq fio xfsprogs btrfs-progs >/dev/null
  modprobe ublk_drv 2>/dev/null || apt-get install -y -qq "linux-modules-extra-$(uname -r)" >/dev/null 2>&1 || apt-get install -y -qq linux-image-generic linux-modules-extra-generic >/dev/null'
if ! ssh $SSH_OPTS -o BatchMode=yes "root@$ip" 'modprobe ublk_drv' 2>/dev/null; then
  ssh $SSH_OPTS -o BatchMode=yes "root@$ip" 'systemctl reboot' 2>/dev/null || true
  sleep 15
  for _ in $(seq 1 60); do ssh $SSH_OPTS -o ConnectTimeout=5 -o BatchMode=yes "root@$ip" 'modprobe ublk_drv' 2>/dev/null && break; sleep 5; done
fi
ssh $SSH_OPTS -o BatchMode=yes "root@$ip" 'modprobe ublk_drv && echo "kernel $(uname -r), ublk_drv loaded"'
"$me/remote-test.sh" "$ip" all
echo "== daemon kills under load"
"$me/powercut.sh" "$ip" 5 kill
echo "== power cuts"
"$me/powercut.sh" "$ip" 10 power
echo "== ALL PASSED on $(ssh $SSH_OPTS -o BatchMode=yes root@$ip uname -r)"
