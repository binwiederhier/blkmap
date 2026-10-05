#!/usr/bin/env python3
# Runs a PowerShell command in a soak guest over WinRM (lab credentials, see setup.ps1).
# Usage: wr.py PORT COMMAND|@FILE   (the guest's WinRM port on this host, 15985+SLOT)
# env WR_TIMEOUT (seconds, 3600): a guest still booting accepts the forwarded port and never
# answers, so probes need a short one
import os
import sys
import winrm

s = winrm.Session(f"http://127.0.0.1:{sys.argv[1]}/wsman", auth=("blkmap", "Blkmap-Lab-2026!"), transport="basic", operation_timeout_sec=int(os.environ.get("WR_TIMEOUT", "3600")), read_timeout_sec=int(os.environ.get("WR_TIMEOUT", "3600")) + 60)
cmd = sys.argv[2]
if cmd.startswith("@"):
    with open(cmd[1:]) as f:
        cmd = f.read()
r = s.run_ps(cmd)
sys.stdout.write(r.std_out.decode(errors="replace"))
sys.stderr.write(r.std_err.decode(errors="replace") if r.status_code else "")
sys.exit(r.status_code)
