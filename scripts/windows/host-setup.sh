#!/bin/bash
# Prepares the soak VM (root): the blkmap deb, the harness in /srv/win/bin, pywinrm.
# Usage: host-setup.sh DEB
set -euo pipefail
me="$(cd "$(dirname "$0")" && pwd)"
modprobe ublk_drv
dpkg -i "${1:?usage: host-setup.sh DEB}" >/dev/null
mkdir -p /srv/win/bin /var/lib/blkmap
cp "$me"/win-vm.sh "$me"/golden.sh "$me"/autounattend.xml "$me"/setup.ps1 "$me"/sql-install.ps1 "$me"/wr.py /srv/win/bin/
[ -x /srv/win/venv/bin/python ] || { python3 -m venv /srv/win/venv && /srv/win/venv/bin/pip install -q pywinrm; }
echo "soak host ready: $(blkmap --version 2>/dev/null || dpkg -s blkmap | grep Version)"
