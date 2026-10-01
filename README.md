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
systemctl stop blkmap@disk1           # unmount first; stop tears the kernel device down
```

The unit is `Type=notify`, so `systemctl start` returns only once the device exists. A mount
in fstab can depend on it with `x-systemd.requires=blkmap@disk1.service`.

HTTP sources must support Range requests (checked when the source is opened, so `validate`
reports a server that cannot do it). Reads fetch 1 MiB aligned blocks through a small
per-source LRU cache.

Writes never touch the sources. They land in the COW file, a sparse raw image of the overlay
at device offsets, and a bitmap records which chunks are there. Delete both files to reset
the device to its sources (while the unit is stopped). The bitmap header pins the device size
and chunk size, so a config change that alters either is refused rather than silently
misreading old data.

## Development

```
make test        # unit tests (no root)
make test-root   # also the ublk integration tests; needs root and ublk_drv loaded
make vet
```

Layout follows the ntfy conventions: `cmd/` (CLI), `config/` (YAML), `source/` (zero, file,
http, concat), `cow/` (bitmap + COW store, implements the ublk backend), `device/` (ublk glue,
symlink, lifecycle), `util/`. The ublk transport is
[github.com/ehrlich-b/go-ublk](https://github.com/ehrlich-b/go-ublk) (MIT, pure Go).

Known limit: requests are capped at 64 KiB (`maxIOSize` in `device/service.go`) because
go-ublk's per-tag buffers are 64 KiB and larger requests return stale data with that library
version. Throughput is fine for a POC; raising it means fixing or vendoring the library.

## License

Apache 2.0. go-ublk is MIT licensed.
