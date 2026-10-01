#!/bin/sh
set -e

# Stop every running device instance before the binary disappears
if [ "$1" = "remove" ] || [ "$1" = "0" ]; then
  if [ -d /run/systemd/system ]; then
    systemctl stop 'blkmap@*.service' >/dev/null 2>&1 || true
  fi
fi
