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

Requirements: Linux with the `ublk_drv` module. On Ubuntu that module is in
`linux-modules-extra-$(uname -r)`, which is not installed by default on minimal images.
Everything, including crash recovery and power cuts, is verified on kernel 6.8 (Ubuntu
24.04) and 7.0 (Ubuntu 26.04). Crash recovery needs the kernel's ublk
user-recovery feature (6.0+); without it blkmap logs a warning and a crash fails I/O instead
of pausing it.

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
in fstab can depend on it with `x-systemd.requires=blkmap@disk1.service`; the packaged udev
rule names the disk `/dev/blkmap/disk1`, so systemd sees the device the mount waits for:

```
/dev/blkmap/disk1  /mnt/disk1  ext4  x-systemd.requires=blkmap@disk1.service,nofail  0 0
```

### Restarts, crashes and upgrades

The kernel device outlives its server. If `blkmap serve` crashes or is killed, I/O to the
device pauses (nothing fails, the mount stays), systemd restarts the server within a second,
and it re-attaches to the same device; requests that were in flight are reissued. Writes
acknowledged before the crash survive even if they were never flushed: their bits live in a
shared bitmap in `/run/blkmap/<id>.bitmap`, which outlives the process (but, like the page
cache it describes, not a reboot).

```
systemctl reload blkmap@disk1   # restart the server without disturbing the device
systemctl stop blkmap@disk1     # tear the device down (unmount first)
blkmap reap disk1               # fail the I/O of a device whose server will not come back
```

A reload makes everything durable and re-executes the installed binary in the same process,
which re-attaches; package upgrades reload every running device, so the new binary takes
over without an unmount. If a server cannot come back (its origin stays down, say), systemd gives up after
10 attempts in a minute and `blkmap-reap@<id>` stops the waiting device, so its I/O fails
instead of hanging. A restart whose config no longer matches the device (another size, block
size or read-only setting) replaces it with a fresh one; the old one's I/O fails.

### Changed sources

The COW file only makes sense over the content it was written over. blkmap records a
fingerprint of the sources (file size and modification time, HTTP ETag or Last-Modified,
the layout) and refuses to start once writes exist if it changed; an HTTP origin whose ETag
changes while running fails reads rather than mixing old and new blocks. If the content is
known to be the same (an image copied with a new timestamp, a mirror), accept it:

```
blkmap pin disk1    # with the device stopped
```

### Status and metrics

```
blkmap status [disk1]        # state, chunks in the COW file, I/O, source, cache, hydration
blkmap status --json disk1
blkmap metrics               # all devices, Prometheus text format
```

Each server answers on a root-only socket, `/run/blkmap/<id>.sock` (`GET /status` as JSON,
`GET /metrics`). For Prometheus, write `blkmap metrics` into node_exporter's textfile
collector directory from a timer, or scrape the socket through a proxy.

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

HTTP sources must support Range requests (a one-byte Range request at open time checks that
and learns the size from Content-Range, so HEAD is never needed and `validate` reports a
server that cannot do it). Reads fetch 1 MiB aligned blocks through a small per-source LRU
cache; concurrent readers of one block share a single request, and transient failures
(network errors, 5xx, truncated bodies) are retried twice with backoff. A 404, 410 or 416 is
reported as `source.ErrNotFound`, which a cache tier treats as a miss.

### Holes and maps

`ReadAt` cannot say "this is a hole"; a source that knows can implement `source.Sparse`,
`Holes(off, length)`, returning ranges that read as zeros. Files and devices answer from
`SEEK_HOLE`/`SEEK_DATA` (a reported hole always reads as zeros, so this direction is safe;
data may contain zeros too), zero segments report themselves, and concat, cache and swappable
sources compose their parts. Hydration asks for holes in 4 GiB windows and marks hole chunks
in the bitmap without copying, so a mostly empty image hydrates without inflating the COW
file, and without transferring its zeros.

Sources that cannot tell (HTTP, a custom source, a device holding a sparse image) get the
same from a **map**: a text file of the data extents (`offset length` per line, same format as
the prefetch list; everything not listed is a hole), produced on the server side with
`blkmap map disk.img > disk.img.map`. Attach it to any source with `map:` (a path or URL); an
`http` source also probes `<url>.map` on its own. With a map attached, holes read as zeros
locally and are never requested, so a 1 TB image holding 100 MB hydrates by moving 100 MB.
`validate` shows how much of each mapped source is data. From code: `source.WithMap(src,
m, base)`, `source.LoadMap`, `source.NewMap`.

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

### Device groups

`device.ServeGroup` serves several devices from one process, for sets whose members depend on
each other (the disks of a software RAID restored from a backup that kept only the data):

- **Aliases**: `Alias{Offset, Length, Target, TargetOffset}` makes a range of one device a view
  of another device's range; reads and writes go to the target's store, so a mirror's second
  plex needs no copy and the two stay identical. An alias must land in a range its target
  serves itself (no chains or cycles), and a device with aliases cannot hydrate.
- **Derived bases**: a base that implements `source.Binder` (anywhere in its tree) receives a
  `source.Lookup` of its siblings' live views, so a parity column computed from the data
  members sees what the guest wrote to them.
- **Write elision** (`ElideIdenticalWrites`, or `cow.Store.SetElision`): a write equal to what
  the device already reads is dropped, so a RAID resync after a restore costs no overlay
  space. It compares against the base only when the base cannot change (no Binder in it),
  otherwise only against chunks already in the COW file, and it never makes a whole-chunk
  write depend on reading the base.
- **Writeback** (`Device.Writeback`, `cow.Store.Writeback`) copies the overlay into a writable
  copy of the base, for a device that was a scratch view of files. Writing back into the base
  itself changes its identity: discard the COW file and bitmap afterwards.

`Group.Close` ends every device's I/O before it closes any store.

## Performance

Requests from the kernel are served by one thread per ublk queue. While the backend answers
in microseconds (a local file) that thread serves reads itself, because a handoff would cost
more than the work; once the smoothed read service time passes 250 us (a network source) it
hands reads to a worker pool, one backend call per queue slot, so up to the full queue depth
of reads runs concurrently against the source. Writes, flushes and discards always run
inline: they target the local COW file, where parallel read-modify-writes only contend. The
store coalesces consecutive chunks in the same state, so a 1 MiB request over unwritten
chunks is one base read, not sixteen, and hydration copies in 1 MiB runs. Each device gets a
4 MiB kernel read-ahead window, and the HTTP source (and `source.NewReadAhead` for others)
fetches the next 8 MiB in the background when it sees a sequential reader, so one `dd` keeps
a slow source busy. Against a 20 ms source (`scripts/rangehttpd -delay 20ms`) a single `dd`,
buffered or direct, goes from the one-at-a-time 50 MiB/s to about 260 MiB/s, and 32 random
1 MiB reads in flight reach about 1,300 IOPS where one-at-a-time is 50. Across two hosts
(the gRPC example, 1 TiB sparse export) a single direct 1 MiB stream went from 23 to 94 MiB/s,
within 10% of what four parallel hydration workers get out of the link.

The hot paths allocate nothing per request (tests pin this with `testing.AllocsPerRun`):
store reads and writes, concat lookups (binary search over segments), cache hits and misses,
RAID-5 reads including parity reconstruction (pooled stripe buffers), and the COW
read-modify-write of a fresh chunk (pooled chunk buffers). `go test -bench . -benchmem
./source/ ./cow/` reports, on a 12 vCPU KVM guest: concat read 39 ns with one segment and
391 ns with a thousand, RAID-5 reconstruction 2.5 GB/s, bitmap set/test 3.5 ns, a 64 KiB
COW chunk write 37 us.

The ublk transport lives in-tree (`ublk/`, about 1,300 lines, derived from go-ublk): an
ioctl-encoded control plane, a minimal SQE128/CQE32 io_uring per queue, one OS thread per
queue, and per-tag buffers sized to the 1 MiB maximum request, so large I/O is never split.
Defaults are 4 queues (fewer on smaller machines) at depth 64, which costs at most 256 MiB of
request buffers per device, touched lazily.

Measured 2026-10-03 on a 12 vCPU KVM guest (kernel 6.8), zero-backed 4 GiB device, direct
I/O with fio's io_uring engine (the psync engine ignores iodepth and understates
everything), 4 jobs at queue depth 32, cow file on the guest's virtio root disk:

| workload | result |
|---|---|
| 4K random read | 1.19M IOPS |
| 64K random read | 388k IOPS, 24 GB/s |
| 1M sequential read, one job at depth 16 | 10.5 GB/s |
| 4K random write (COW, 64K chunks) | 195k IOPS (515k with the cow file on tmpfs) |

Random 4K writes pay for the copy-on-write chunking: the first write into a 64K chunk copies
the chunk from the base and writes it whole. A smaller `cow.chunk-size` trades that for a
bigger bitmap.

## Examples

`examples/` holds runnable examples with their own READMEs: two CLI ones (`cli-stitch`,
`cli-raid5-cache`), two library ones (`lib-synthetic`, `lib-dircache`: a fast cache tier
implemented as a directory of block files) and `grpc-remote`, a two-call gRPC protocol that
exports sparse files from another host with their hole map. `make examples` vets, tests and
builds them.

## Development

```
make test        # unit tests (no root)
make test-root   # ublk and device integration tests; needs root and ublk_drv loaded
make stress      # e2e + fio verify workloads, ext4/xfs/btrfs, fstrim, SIGKILL under load, restarts
make scenarios   # 36 real-life scenarios: origins that die or hang, SIGKILL mid-write and
                 # mid-hydration, reloads and package upgrades under load, crash loops,
                 # changed sources, lost cow/bitmap, full disk, 8 TiB device, ...
make test-remote HOST=ip SUITE=all       # all of the above on a scratch VM
make powercut HOST=ip MODE=power|kill    # power cuts / daemon kills under a verifying writer
make soak HOST=ip MINUTES=120            # hours of verified I/O under kills, reloads, outages
make test-vm     # everything, on a VM created for the run and destroyed after it
make vet
```

The test plan, what each layer covers and how to repeat a full run, is in
[docs/testing.md](docs/testing.md); results of past runs are in `docs/test-results/`.

Layout follows the ntfy conventions: `cmd/` (CLI), `config/` (YAML), `source/` (zero, file,
http, raid5, concat), `cow/` (bitmap + COW store, discard and write-zeroes aware), `device/`
(COW over source over ublk, symlink, crash cleanup), `ublk/` (kernel transport), `util/`.

Run the root suites on a throwaway VM rather than your workstation: a transport bug can wedge
the kernel for good. `make test-vm` clones a cloud-init Ubuntu template on a Proxmox host
(`PROXMOX=root@box11 TEMPLATE=9000`), runs every suite, the scenarios, daemon-kill and
power-cut cycles there, and destroys the VM. GitHub Actions runs the unit tests, vet, the
examples and a package build on every push.

The power-cut test (`scripts/powercut.sh`) writes checksummed, numbered records, flushes after
every 16, and records each acknowledged flush on the controlling machine; then the VM loses
power mid-write (an immediate reboot without sync). After it comes back, every acknowledged
record must be on the device. In kill mode the daemon is SIGKILLed mid-write instead, three
times per cycle, and the writer must never see an error.

### Durability and shutdown

The COW file and its bitmap are kept consistent in one direction at all times: a chunk's
bit reaches the bitmap file only after the chunk's data reached the COW file (fdatasync
first, then the bitmap pages, then fdatasync again). That happens on every flush the guest
issues, every 5 seconds when anything changed, and on shutdown. After a crash or power cut
the device therefore shows, per chunk, either the write or the base, never zeros for a
write that was acknowledged but not flushed.

Shutdown order is: stop hydration and the flush timer, abort reads blocked in a source (a
hung origin fails those with EIO rather than holding up the stop), STOP_DEV (drains in-flight
I/O), flush and close the store, then DEL_DEV, then remove the symlink. DEL_DEV waits for anything
holding the block device open, so `systemctl stop` on a mounted device logs a warning and
waits; after `TimeoutStopSec` systemd kills the daemon, which by then has everything on
disk, and the next start deletes the dead kernel device.

`serve` records the ublk id and its pid in `/run/blkmap/<id>`; the next start re-attaches to
a device waiting for recovery, refuses to start next to a live server of the same id, and
deletes any other dead predecessor. Devices created without recovery (by older versions)
are left behind by a crash until the next start deletes them. Two kernel 6.8 traps worth
knowing: a ublk server without recovery that dies while START_DEV is scanning partitions
wedges that device for good and makes a global `sync` hang (use `sync -f`), and only the
ioctl-encoded command set is accepted (`CONFIG_BLKDEV_UBLK_LEGACY_OPCODES` is off).

## License

Apache 2.0. The `ublk` package derives from [go-ublk](https://github.com/ehrlich-b/go-ublk)
(MIT, Benjamin Ehrlich).
