#!/bin/sh
set -e

# Load the ublk driver now (modules-load.d covers future boots) and let systemd see the
# new unit. Devices are started per instance: systemctl enable --now blkmap@<id>.
if [ "$1" = "configure" ] || [ "$1" -ge 1 ]; then
  modprobe ublk_drv 2>/dev/null || echo "blkmap: ublk_drv not available; on Ubuntu install linux-modules-extra-$(uname -r)"
  if [ -d /run/systemd/system ]; then
    systemctl --system daemon-reload >/dev/null || true
  fi
fi
