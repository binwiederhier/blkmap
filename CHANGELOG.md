# Changelog

All notable changes to blkmap. Versions follow semantic versioning once 1.0 is tagged;
releases are cut with `make release` from a `vX.Y.Z` tag.

## v0.4.2 (2026-10-05)

- **HTTP identity names the resource** (2026-10-05 external review, finding 03): two URLs can
  answer with the same size and ETag and hold different bytes (RFC 9110 scopes validators to
  a resource), so an overlay could be reopened over another resource and mix the two. The
  identity now includes a hash of scheme, host and path; credentials and the query string
  are left out, so signed URLs and rotated tokens keep their identity. Overlays recorded with
  the older form are accepted and re-pinned (`source.LegacyIdentity`,
  `cow.Options.LegacyIdentity`), so upgrading needs no `blkmap pin`; moving an image to
  another URL does.
- **Short io_uring submissions** (finding 04): io_uring_enter may consume fewer entries than
  it is given, and the rest stayed in the submission queue for good, since the next flush
  counted from the tail already published. Submission now follows the kernel's head until
  everything is consumed, retrying interruptions, EAGAIN/EBUSY and calls without progress,
  and a wait hands over anything still unconsumed.

## v0.4.1 (2026-10-05)

- **Reliance on a sibling only where it is durable** (2026-10-05 external review, finding 01):
  nopwrite and reclaim over a sibling's view compared against the sibling's current content,
  so b could skip or drop data it had acknowledged in favour of bytes a still held unflushed,
  and a crash of a lost it. Now a store relies on its base only where `source.Durable` says
  the content would survive a crash; `cow.Store.Durable` answers for a sibling store (bit
  committed, data synced since its last change; `TrackDurability`, called by `ServeGroup`);
  views forward it (`source.Durability`), and a Binder that does not is never relied on. A
  chunk reclaim keeps for want of a durable base is looked at again later.
- **Writeback never copies a reclaimed chunk** (finding 02): it tested a chunk's bit before
  taking its lock, and a reclaim and flush in between punched the chunk, so Writeback wrote
  zeros over the destination's right bytes. It re-tests the bit under the lock.
- **Testing**: `make crashreplay MODE=mirror` (`scripts/mirrorcrash`): every logged crash state
  of a mirror group with nopwrite and reclaim; v0.4.0 loses acknowledged writes there, v0.4.1
  passes. The Windows soak bounds its SQL probes and power-cycles a guest that hangs in
  firmware after a reset.

## v0.4.0 (2026-10-05)

- **Reclaim** (library, `device.Options.Reclaim`, `cow.Store.Reclaim`): a background sweeper
  that drops stored chunks which have become identical to their base again. With nopwrite
  over a derived base a chunk freezes in the overlay the first time a write to it differs from
  the base at that instant, and for a mirror plex that happens by timing alone: the halves of
  a mirrored write land in either order, the plex whose half comes first sees its sibling
  still holding the old bytes and stores its half, and every later write to that chunk is
  compared against the stored copy, so a hot chunk stays stored on both plexes for good. The
  sweeper walks the chunks written since it last looked (a repeated write to a stored chunk
  counts too), compares each with its base and clears the bit of every one that equals it; a
  chunk that differs gets one second look a second later, when the other half of a mirrored
  write has landed. The chunk is punched by the Flush that puts the cleared bit on disk
  (`cow.Bitmap.Clear`), the mirror image of the data-then-bit order for a set bit, so a crash
  never finds a recorded chunk that was punched; a read in flight on the chunk is waited for;
  punches a crashed predecessor owed are found in the COW file and done. Costs one overlay
  read and one base read per examined chunk: full batches while the guest is idle, a trickle
  of 64 chunks per 100 ms under load. Counters in `Status.Reclaim`, `blkmap status` and the
  metrics. Refused together with `Hydrate`, which copies the base on purpose. Its state
  (about four bits per chunk) is allocated only when reclaim is on (`cow.Store.EnableReclaim`,
  which `device.Options.Reclaim` calls before the device serves I/O).
- **Windows soak harness**: guests boot from VirtIO SCSI. QEMU's AHCI and NVMe emulation drop
  a guest's write-through (FUA) writes, so SQL Server's commits never reached blkmap as
  flushes and a power cut lost acknowledged commits; whatever virtual controller fronts a
  blkmap device must pass FUA or flushes through. Windows 11 builds prevent BitLocker device
  encryption and set up WinRM without a network.

## v0.3.0 (2026-10-05)

- **Alias ranges removed, nopwrite over derived bases** (breaking, library): a device range
  routed into a sibling's store (`device.Alias`) let a write through one device overwrite the
  other's only copy. Windows resyncing a restored mirror races the guest: a block read from
  plex 0 before a guest write and copied to plex 1 after it landed in plex 0's store and
  undid the write, which corrupted mirrored boot volumes under load. A mirror plex is now a
  `source.Binder` view of its sibling with its own overlay (`examples/lib-ldm-mirror`), and
  nopwrite (skipping identical writes) applies over such derived bases too: a skipped range
  keeps following its base until a write there differs, so a resync or parity regeneration
  stores nothing.
  `GroupOptions` is gone; `ServeGroup` takes `[]*Options`, which gained `NopWrite`.
- **Write elision renamed to nopwrite**, the storage term (ZFS): `cow.Store.SetElision` is
  `SetNopWrite`, the `ElideIdenticalWrites` option is `NopWrite`.
- **Device groups report and hand off a failed store**: `Group.Done`, `Group.Err` and
  `Group.Abandon` (a derived base reads its siblings' stores, so one failed store breaks
  them too), `Device.Err`, and `cow.Store.Fail` for owners that find a COW file unusable
  some other way.
- **Testing**: `make faults` (write errors with and without a journal, bad blocks under the
  COW file), `make crashreplay MODE=fs` (guest filesystem and hydration across every logged
  crash state), and a Windows soak harness (`scripts/windows`: Windows Server 2019 to 2025
  and Windows 11 with SQL Server on blkmap devices, under kills, reloads, resets and power
  cuts).

## v0.2.3 (2026-10-04)

- **A failed COW fsync stops the store**: Linux marks the pages of a failed
  writeback clean, so a retried flush used to succeed and commit bitmap bits for data that
  never reached the disk (seen as lost copy-ups: chunks reading zeros instead of the base).
  Now the store refuses all I/O after the first failed sync, never commits another bit,
  drops the live bitmap, and the server hands the kernel device to a successor that serves
  the last durable state (`cow.ErrCOWFailed`, `Device.Abandon`).

## v0.2.2 (2026-10-04)

- **Zero marking after a crash**: hydration marking a base hole as zero could
  expose COW data whose bit a crash without a live bitmap had lost; it now punches first.

## Earlier releases

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
