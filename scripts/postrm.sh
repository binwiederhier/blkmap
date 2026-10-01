#!/bin/sh
set -e

# Purge removes the config directory; COW files in /var/lib/blkmap are kept on purpose
if [ "$1" = "purge" ] || [ "$1" = "0" ]; then
  rm -rf /etc/blkmap
  if [ -d /run/systemd/system ]; then
    systemctl --system daemon-reload >/dev/null || true
  fi
fi
