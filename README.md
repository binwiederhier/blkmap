# blkmap

blkmap stitches files, block devices, HTTP URLs and zero ranges into one Linux block device,
`/dev/blkmap/<id>`, and captures every write in a separate copy-on-write (COW) file. It is
`dmsetup`-like in spirit but driven by one YAML file per device and runs as a systemd
instance, `blkmap@<id>`. The device is served from userspace through the kernel's `ublk`
driver (io_uring); no kernel modules of its own.

```
/dev/blkmap/<id>  ->  /dev/ublkbN   (kernel ublk device, served by this process)
                          |
                      COW store     written chunks: /var/lib/blkmap/<id>.cow (+ .bitmap)
                          |         everything else: the base below
                      base layout   [ zero 1M ][ file part1.img ][ device /dev/sdb1 ][ http ... ]
```

## Install

Build the Debian package with goreleaser (`make release-snapshot`) and install it with
`make install-deb`, or use the pre-built deb. The package ships the binary, the
`blkmap@.service` unit, a `modules-load.d` entry for `ublk_drv`, `/etc/blkmap/` and
`/var/lib/blkmap/`, and an annotated example config at `/usr/share/doc/blkmap/blkmap.example.yml`.

Requirements: Linux 6.8+ with the `ublk_drv` module. On Ubuntu 24.04 that module is in
`linux-modules-extra-$(uname -r)`, which is not installed by default on minimal images.

## Use

Write `/etc/blkmap/<id>.yml`:

```yaml
size: 10G             # optional; defaults to the end of the last segment
block-size: 512       # 512 (default) or 4096
read-only: false
cow:
  file: /var/lib/blkmap/disk1.cow     # default; bitmap lives next to it as .bitmap
  chunk-size: 64K                     # bitmap granularity
segments:             # in device order; "offset" defaults to the previous segment's end
  - type: zero
    size: 1M
  - type: file
    path: /srv/images/part1.img
    source-offset: 0  # optional window into the source
    size: 100M        # optional for file/device/http: defaults to the rest of the source
  - type: device
    path: /dev/sdb1
  - type: http
    offset: 5G        # explicit placement; the gap before it reads as zeros
    url: https://example.com/disk.img
```

Then:

```
blkmap validate disk1                 # parse, open the sources, print the resolved layout
systemctl enable --now blkmap@disk1   # /dev/blkmap/disk1 appears when the unit is active
mkfs.ext4 /dev/blkmap/disk1 ; mount /dev/blkmap/disk1 /mnt
systemctl stop blkmap@disk1           # unmount first: deletion waits for openers of the device
```

The unit is `Type=notify`, so `systemctl start` returns only once the device exists. A mount
in fstab can depend on it with `x-systemd.requires=blkmap@disk1.service`.

### RAID-5 segments (Windows dynamic disks and others)

A `raid5` segment reassembles a RAID-5 set from its members on the fly, including one
missing member rebuilt from parity. The geometry is given, not detected; for a Windows
dynamic-disk (LDM) RAID-5 volume the defaults match (64 KiB stripes, left-symmetric rotation,
the same `raid5_ls` table libldm builds), so you only need the member order and the offset
of the LDM data partition on each member. For a Linux md array pass the values from
`mdadm --examine` (`data_offset` as `source-offset`, chunk size as `stripe-size`, members in
role order).

```yaml
segments:
  - type: raid5
    stripe-size: 64K            # default
    layout: left-symmetric      # default; also left-asymmetric, right-symmetric, right-asymmetric
    size: 100G                  # optional; default (members-1) x smallest member, whole stripes
    members:                    # in array order; file, device or http, or missing
      - type: device
        path: /dev/sdb
        source-offset: 1M
      - type: file
        path: /srv/disk2.img
        source-offset: 1M
      - missing: true           # at most one
```

### Cache tiers

A `cache` segment reads from a fast source and falls back to a slow one on any error, with
no retries: an HTTP cache answering 404 for a block it does not have, a local copy that is
incomplete, a tier that is down. The fast tier is never written; something else fills it.
Both tiers take any source type, so cache tiers can sit inside raid5 members and vice versa.

```yaml
segments:
  - type: cache
    fast: {type: http, url: http://cache.lan/img.raw}
    slow: {type: http, url: https://origin.example.com/img.raw}
```

### Background hydration

With a `hydrate` block, blkmap copies the base into the COW file in the background, so the
device eventually serves everything locally and, once every chunk is there, no longer opens
its sources on start. Guest I/O always has priority: hydration pauses while requests are in
flight or arrived in the last 100 ms. A prefetch list (`offset length` per line, highest
priority first) is copied at full speed; the rest follows at the configured rate, or not at
all with `rest: false`. `use-cache: never` sends background reads straight to the slow tier.
Ranges known to be zeros (zero segments, gaps) are marked without being copied. Progress
goes to the journal every 30 seconds by default.

```yaml
hydrate:
  prefetch-list: /etc/blkmap/img1.prefetch
  rest: true
  rate: 20M
  use-cache: never
```

HTTP sources must support Range requests (checked when the source is opened, so `validate`
reports a server that cannot do it). Reads fetch 1 MiB aligned blocks through a small
per-source LRU cache.

Writes never touch the sources. They land in the COW file, a sparse raw image of the overlay
at device offsets, and a bitmap records which chunks are there. Delete both files to reset
the device to its sources (while the unit is stopped). The bitmap header pins the device size
and chunk size, so a config change that alters either is refused rather than silently
misreading old data.

## Library use

The daemon is a thin layer over three packages, and a Go program that computes or fetches
blocks can use them directly. Implement `source.Source` (`ReadAt`, `Size`, `Close`) and hand it
to `device.Serve`; the COW overlay, the kernel device and the `/dev/blkmap/<id>` symlink come
with it:

```go
dev, err := device.Serve(ctx, &device.Options{
    ID:      "synth",
    Base:    mySource,                      // source.Source, read-only
    COWFile: "/var/lib/blkmap/synth.cow",   // bitmap and 64K chunks by default
})
// ... /dev/blkmap/synth is live until dev.Close()
```

`source.Concat`, `source.RAID5`, `source.Cache`, `source.File`, `source.HTTP` and
`source.Zero` are ordinary Sources and compose, so one segment of an otherwise ordinary
layout can come from your code. `source.NewSwappable` wraps a Source whose target can be
replaced while the device is live. A config-driven program can supply one segment as
`type: custom` after `source.Register("name", constructor)`. A fast tier of your own signals a
miss with `source.ErrNotFound`. Hydration from code takes `device.Hydrate` with ranges, a
rate, the cache policy and an optional progress callback. One level down, `ublk.Create`
serves any `ublk.Backend` (ReadAt, WriteAt, Size, Flush, optionally Discard and WriteZeroes)
without the COW layer.

## Performance

The ublk transport lives in-tree (`ublk/`, about 800 lines, derived from go-ublk): an
ioctl-encoded control plane, a minimal SQE128/CQE32 io_uring per queue, one OS thread per
queue, and per-tag buffers sized to the 1 MiB maximum request, so large I/O is never split.
Defaults are 4 queues (fewer on smaller machines) at depth 64, which costs at most 256 MiB of
request buffers per device, touched lazily.

Measured on a 12 vCPU KVM guest (kernel 6.8) with `scripts/stress.sh`, zero-backed 4 GiB
device, direct I/O, 4 jobs at queue depth 32:

| workload | result |
|---|---|
| 4K random read | 329k IOPS |
| 1M random read | 21 GB/s |
| 1M sequential read | 4.1 GB/s (single job) |
| 4K random write (COW, 64K chunks) | 23k IOPS |
| 1M random write (COW) | 977 MB/s |

Random 4K writes pay for the copy-on-write chunking: the first write into a 64K chunk copies
the chunk from the base and writes it whole. A smaller `cow.chunk-size` trades that for a
bigger bitmap.

## Development

```
make test        # unit tests (no root)
make test-root   # ublk and device integration tests; needs root and ublk_drv loaded
make stress      # e2e + fio verify workloads, ext4/xfs/btrfs, fstrim, SIGKILL under load, restarts
make test-remote HOST=ip STRESS=stress   # the same on a throwaway VM (recommended, see below)
make vet
```

Layout follows the ntfy conventions: `cmd/` (CLI), `config/` (YAML), `source/` (zero, file,
http, raid5, concat), `cow/` (bitmap + COW store, discard and write-zeroes aware), `device/`
(COW over source over ublk, symlink, crash cleanup), `ublk/` (kernel transport), `util/`.

Run the root suites on a throwaway VM rather than your workstation: a transport bug can wedge
the kernel for good, and a scratch VM is rebooted in seconds. `scripts/remote-test.sh` builds
here and runs there; verified on Ubuntu 24.04 (kernel 6.8) and 26.04 (kernel 7.0).

A crashed server leaves its kernel device behind (only DEL_DEV removes one); `serve` records
the ublk id in `/run/blkmap/<id>` and deletes the dead predecessor on the next start. Two
kernel 6.8 traps worth knowing: a ublk server that dies while START_DEV is scanning
partitions wedges that device for good and makes a global `sync` hang (use `sync -f`), and
only the ioctl-encoded command set is accepted (`CONFIG_BLKDEV_UBLK_LEGACY_OPCODES` is off).

## License

Apache 2.0. The `ublk` package derives from [go-ublk](https://github.com/ehrlich-b/go-ublk)
(MIT, Benjamin Ehrlich).
