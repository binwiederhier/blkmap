# Windows soak harness

Real Windows versions with SQL Server on blkmap devices for days, through blkmap kills and
reloads, guest crashes and power cuts. The plan and status live in
`~/Code/plans/261004-blkmap-windows-soak.md`.

| File | Runs on | What |
|---|---|---|
| `host-setup.sh DEB` | soak VM | installs the deb, copies the harness to `/srv/win/bin`, pywinrm venv |
| `golden.sh VERSION INDEX [SLOT]` | soak VM | unattended install of `/srv/win/iso/VERSION.iso` into `/srv/win/golden/VERSION.raw` (read-only when done) |
| `autounattend.xml`, `setup.ps1`, `sql-install.ps1` | the guest | the unattended install and its first logon: WinRM, no updates, no sleep, SQL Server Developer from the attached ISO (`/srv/win/iso/sql2022.iso` or `SQL=sql2025`), then shutdown |
| `win-vm.sh` | soak VM | starts a guest (q35, Secure Boot, TPM 2.0, NVMe disk or `BUS=ahci`, e1000e, WinRM on 15985+SLOT, SQL on 11433+SLOT); `shot NAME FILE.png` takes a screenshot |
| `wr.py PORT CMD` | soak VM | a PowerShell command in a guest over WinRM |
| `sqlsoak/` | codebox | Go client (own module): `init`, `run` (prints `ack N` per committed transaction), `verify -acked N` |
| `soak.sh HOURS VERSION...` | codebox | the soak: devices, guests, load, disruptions, verification; logs in `logs/win-*` |

The soak VM is VM 900 `blkmap-win` on box12 (192.168.1.223). Image indexes: the Server ISOs
(2019, 2022, 2025) are 1 Standard Core, 2 Standard (Desktop Experience), 3 Datacenter Core,
4 Datacenter; the golden images use 2. Lab credentials (`blkmap` / `Blkmap-Lab-2026!`, SQL
`sa` with the same password) are only reachable through the soak VM's port forwards.

Gotchas:

- Ubuntu's AppArmor profile for swtpm allows its state only in `/var/lib/swtpm` and its
  socket in `/run/libvirt/qemu/swtpm`; anything else fails with "Permission denied".
- The Windows boot loader waits for a key before it boots the DVD: `golden.sh` sends Enter
  for 20 s through the QEMU monitor.
- Builds that run at the same time need different slots (port forwards collide otherwise).
- Soak guests use NVMe: QEMU's AHCI drops a guest's FUA writes, so SQL Server's commits never
  reach blkmap as flushes (measured: 2,083 commits a minute, 17 flushes) and a power cut loses
  acknowledged commits. Golden images need `stornvme` set to boot-start (setup.ps1 does it;
  images built before that need it set once while booted on AHCI).
- Windows 11's OOBE stops at the region screen unless the oobeSystem pass sets the locale
  (autounattend.xml does) and can hang at "Checking for updates": cut the guest's link with
  `set_link n0 off` in the QEMU monitor until OOBE is past it.
- Box12 has 8 cores: two nested Windows guests at a time, and no golden build next to a soak.
- SQL Server's web bootstrappers (`SQL20xx-SSEI-*.exe`) stop working once Microsoft retires
  them ("This version of the installer is no longer supported"); the harness installs from
  the Developer ISOs on download.microsoft.com instead.
