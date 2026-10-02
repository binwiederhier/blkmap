#!/bin/sh
set -e

# Load the ublk driver now (modules-load.d covers future boots) and let systemd see the
# new unit. Devices are started per instance: systemctl enable --now blkmap@<id>.
if [ "$1" = "configure" ] || [ "$1" -ge 1 ]; then
  modprobe ublk_drv 2>/dev/null || echo "blkmap: ublk_drv not available; on Ubuntu install linux-modules-extra-$(uname -r)"
  if [ -d /run/systemd/system ]; then
    systemctl --system daemon-reload >/dev/null || true
  fi
  udevadm control --reload-rules 2>/dev/null || true
fi

# On upgrade, hand every running device to the new binary: reload makes the old server
# detach (I/O pauses for a moment, nothing fails) and systemd starts the new one, which
# re-attaches. Servers from before live restarts record no pid in their state file; they
# would just die on the signal, so they are left running and named instead.
upgrade=""
if [ "$1" = "configure" ] && [ -n "$2" ]; then upgrade=1; fi
case "$1" in [2-9]*) upgrade=1 ;; esac
if [ -n "$upgrade" ] && [ -d /run/systemd/system ]; then
  for unit in $(systemctl list-units --type=service --state=active --plain --no-legend 'blkmap@*' | awk '{print $1}'); do
    id=${unit#blkmap@}; id=${id%.service}
    if [ "$(wc -w < "/run/blkmap/$id" 2>/dev/null || echo 0)" -ge 2 ]; then
      systemctl reload "$unit" || echo "blkmap: could not hand $id to the new version; restart $unit when convenient"
    else
      echo "blkmap: $id still runs the previous version; unmount it and restart $unit to upgrade it"
    fi
  done
fi
