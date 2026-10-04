# lib-ldm-mirror

Both disks of a Windows dynamic-disk (LDM) mirror, rebuilt from a backup of one of them, as
two devices served from one process with `device.ServeGroup`:

- `/dev/blkmap/ldm0` is the backed-up disk.
- `/dev/blkmap/ldm1` is the other disk: its own header (partition table and LDM private
  region, from `-header1` if you kept it, else zeros) and, for the mirrored volume extent,
  an `Alias` onto the same range of `ldm0`. Reads there see disk 0's bytes, writes go to
  disk 0's store, so the plexes cannot diverge and nothing is stored twice.
- Both devices set `ElideIdenticalWrites`: a write whose bytes equal what the device
  already reads is dropped. When Windows resyncs the mirror after the restore it rewrites
  every block of disk 1 with disk 0's content; with elision that costs no overlay space.

```
go build ./examples/lib-ldm-mirror
sudo ./lib-ldm-mirror -disk0 disk0.img -data-offset 1M -demo
```

`-demo` does what a resync does, through the kernel devices, and prints the result:

```
resync of 16 MiB onto ldm1: chunks stored before 0, after 0
4 KiB change through ldm1: chunks stored 1; ldm0 reads it back: true
```

The volume extent defaults to everything between the data offset and the LDM database in
the last MiB of the disk; pass `-data-length` for the real extent (from the LDM database or
the disk management console). If the extents sit at different offsets on the two disks, set
the alias's `TargetOffset` accordingly; the example keeps them equal.

`go test ./examples/lib-ldm-mirror` checks elision at the store level without a kernel;
`TestMirrorThroughKernel` serves both disks and runs the demo (root and `ublk_drv` only).
