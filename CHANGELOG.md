# Changelog

All notable changes to blkmap. Versions follow semantic versioning once 1.0 is tagged;
releases are cut with `make release` from a `vX.Y.Z` tag.

## Unreleased

- **Device groups** (library): `device.ServeGroup` with aliased ranges between devices
  (mirror plexes stored once), `source.Binder` for bases derived from their siblings' live
  views (RAID parity), write elision for identical rewrites, and overlay writeback. Review
  fixes before merging: alias chains and cycles are refused (a cycle crashed the process),
  elision never trusts a base that can change and never makes a whole-chunk write need the
  base, Binders nested in a base are bound, a cancelled start touches nothing, a failed
  publish no longer closes the base twice, a group halts all I/O before closing any store,
  and writeback reads each chunk under its lock.

Production hardening:

- **Crash and restart survival**: the kernel device outlives its server. A crash or kill
  pauses I/O, systemd restarts the server within a second, and it re-attaches (ublk user
  recovery, in-flight requests reissued). Acknowledged but unflushed writes survive through
  a live bitmap in `/run/blkmap`. `systemctl reload` re-executes the (upgraded) binary in
  place, which re-attaches; package upgrades do that for every running device. `blkmap reap` and the
  `blkmap-reap@` unit fail the I/O of a device whose server will not come back.
- **Source pinning**: a fingerprint of the sources (file mtime, HTTP ETag/Last-Modified,
  layout) is recorded in the bitmap; a changed source is refused once writes exist, and an
  HTTP origin that changes while running fails reads. `blkmap pin` accepts a changed source.
- **Status and metrics**: `blkmap status`, `blkmap metrics` (Prometheus text), and a
  per-device socket in `/run/blkmap` with request counts, latency histograms, source and
  cache counters, hydration progress and dispatch state.
- **Tests**: power-cut and daemon-kill cycles under a verifying writer
  (`scripts/powercut.sh`), 36 real-life scenarios, `make test-vm` on a throwaway Proxmox
  VM, GitHub Actions for unit tests, vet, examples and packaging.
- Earlier in this cycle: aborting hung sources on stop, flush ordering under concurrent
  writes, COW file locking and loss detection, hydration retry passes, read-ahead under
  parallel dispatch, HTTP response validation, a udev rule so fstab mounts can wait for
  `/dev/blkmap/<id>`, unit hardening.

Compatibility: devices started by earlier versions keep running through an upgrade but are
not handed off (they would just die); restart them when convenient. The state file in
`/run/blkmap/<id>` now holds `ID PID`.
