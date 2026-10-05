#!/bin/bash
# Builds a golden image on the soak host (root): an unattended install of one Windows
# version into /srv/win/golden/VERSION.raw with SQL Server Developer from
# /srv/win/iso/$SQL.iso (SQL=sql2022 by default; autounattend.xml, setup.ps1,
# sql-install.ps1), which shuts down when it is done. Usage: golden.sh VERSION INDEX [SLOT]
#   VERSION names /srv/win/iso/VERSION.iso; INDEX is the install.wim image (wiminfo); SLOT
#   (default 9) keeps builds that run at the same time apart (see win-vm.sh).
set -euo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
# Run from a private copy: bash reads scripts as it goes, so replacing this file under a
# running build (host-setup.sh, scp) would otherwise break it hours in
[ -n "${GOLDEN_COPY:-}" ] || { c=$(mktemp /tmp/golden.XXXX.sh); cp "$0" "$c"; GOLDEN_COPY=1 GOLDEN_DIR=$me exec bash "$c" "$@"; }
me=${GOLDEN_DIR:-$me}
version=${1:?usage: golden.sh VERSION INDEX [SLOT]} index=${2:?usage: golden.sh VERSION INDEX [SLOT]} slot=${3:-9}
w=/srv/win/build/$version
golden=/srv/win/golden/$version.raw
mkdir -p $w/cd /srv/win/golden
[ -e $golden ] && { echo "$golden exists; delete it to rebuild"; exit 1; }
sed -e "s/@INDEX@/$index/" -e "s/@NAME@/$(echo "$version" | tr -cd 'a-z0-9' | cut -c1-15)/" $me/autounattend.xml > $w/cd/autounattend.xml
cp $me/setup.ps1 $me/sql-install.ps1 $w/cd/
xorriso -as mkisofs -quiet -V UNATTEND -J -r -o $w/unattend.iso $w/cd
truncate -s 48G $golden.part
rm -f /srv/win/vars/golden-$version.fd; rm -rf /var/lib/swtpm/golden-$version
$me/win-vm.sh golden-$version $slot $golden.part /srv/win/iso/$version.iso $w/unattend.iso /srv/win/iso/${SQL:-sql2022}.iso
# The installer's boot loader waits for a key before it boots from the DVD
$me/win-vm.sh keys golden-$version ret 20
start=$(date +%s)
while kill -0 "$(cat /run/win/golden-$version.pid 2>/dev/null)" 2>/dev/null; do
  sleep 30
  if [ $(( $(date +%s) - start )) -gt 14400 ]; then
    $me/win-vm.sh shot golden-$version $w/timeout.png; $me/win-vm.sh kill golden-$version
    echo "FAIL: $version did not finish in 4 h; screenshot $w/timeout.png"; exit 1
  fi
done
# The guest shut itself down; setup.ps1 leaves a marker and its log on C:
loop=$(losetup -f --show -P -r $golden.part)
mkdir -p $w/c && mount -t ntfs3 -o ro ${loop}p3 $w/c
ok=0; [ -f $w/c/golden-done.txt ] && [ -d "$w/c/Program Files/Microsoft SQL Server" ] && ok=1
cp $w/c/setup.log $w/setup.log 2>/dev/null || true
umount $w/c; losetup -d $loop
[ $ok = 1 ] || { echo "FAIL: $version setup incomplete; see $w/setup.log"; exit 1; }
mv $golden.part $golden && chmod 444 $golden
echo "golden $version done in $(( ($(date +%s) - start) / 60 )) min: $golden"
