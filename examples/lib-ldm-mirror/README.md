# lib-ldm-mirror

Both disks of a Windows dynamic-disk (LDM) mirror, rebuilt from a backup of one of them, as
two devices served from one process with `device.ServeGroup`:

- `/dev/blkmap/ldm0` is the backed-up disk.
- `/dev/blkmap/ldm1` is the other disk: its own header (partition table and LDM private
  region, from `-header1` if you kept it, else zeros) and, for the mirrored volume extent, a
  live view of the same range of `ldm0`, a small `source.Binder` that reads `ldm0` through
  its device, overlay included. `ldm1` has its own overlay: a write to it never changes
  `ldm0`, exactly as with two real disks.
- Both devices set `NopWrite`: a write whose bytes equal what the device
  already reads is dropped. When Windows resyncs the mirror after the restore it rewrites
  every block of disk 1 with disk 0's content; with nopwrite that costs no overlay space, and
  the range keeps following disk 0. The same goes for disk 1's half of every mirrored write
  while the plexes agree. Only a write that differs from disk 0 is stored, on disk 1 alone.

Do not route disk 1's writes into disk 0's store to save the second copy. Windows' resync
races the guest: a block read from plex 0 before a guest write and copied to plex 1 after it
would then land in plex 0's store and undo the write, on the only copy there is. blkmap
offered exactly that as an alias range once and removed it for this reason.

```
go build ./examples/lib-ldm-mirror
sudo ./lib-ldm-mirror -disk0 disk0.img -data-offset 1M -demo
```

`-demo` does what Windows does, through the kernel devices, and prints the result:

```
resync of 16 MiB onto ldm1: chunks stored before 0, after 0
4 KiB mirrored write to both disks: chunks stored 1; ldm1 reads it back: true
4 KiB write to ldm1 alone: chunks stored 2; ldm0 keeps its bytes: true
```

The volume extent defaults to everything between the data offset and the LDM database in
the last MiB of the disk; pass `-data-length` for the real extent (from the LDM database or
the disk management console). If the extents sit at different offsets on the two disks, give
the view the offset on disk 0; the example keeps them equal.

`go test ./examples/lib-ldm-mirror` checks nopwrite at the store level without a kernel;
`TestMirrorThroughKernel` serves both disks and runs the demo (root and `ublk_drv` only).
