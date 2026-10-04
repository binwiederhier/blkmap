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

## Quick start

Write `/etc/blkmap/disk1.yml`:

```yaml
segments:
  - type: file
    path: /srv/images/part1.img
  - type: http
    url: https://example.com/rest-of-disk.img
```

Then:

```
blkmap validate disk1                 # parse, open the sources, print the resolved layout
systemctl enable --now blkmap@disk1   # /dev/blkmap/disk1 appears when the unit is active
mkfs.ext4 /dev/blkmap/disk1 ; mount /dev/blkmap/disk1 /mnt
blkmap status disk1
systemctl stop blkmap@disk1           # unmount first: deletion waits for openers of the device
```

Writes go to `/var/lib/blkmap/disk1.cow`; the sources are never modified. A server crash
pauses I/O and the restarted server re-attaches; `systemctl reload` and package upgrades
replace the server without disturbing the device.

## Documentation

- [docs/guide.md](docs/guide.md): the user guide, by use case: stitching images, HTTP sources
  and maps, RAID-5 sets, cache tiers, booting fast from a slow archive with a recorded
  prefetch list, status and metrics, systemd and fstab, guarantees, and the Go library.
- [docs/architecture.md](docs/architecture.md): how it works inside: packages, important
  types, the ublk transport, the COW store, sequence diagrams of start, I/O, crash recovery
  and hydration, with real output.
- [docs/test-plan.md](docs/test-plan.md) and [docs/testing.md](docs/testing.md): every use
  case as a test with its expected outcome, and how to run the suite against a machine.
- `examples/`: four runnable programs (two CLI, two library, one gRPC across hosts).

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
A device whose reads can reach the network gets one queue at depth 64 (one queue thread keeps
a network busy; 64 MiB of request buffers), a local one up to 4 (at most 256 MiB), touched
lazily.

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
make test                          # unit tests (no root)
make examples                      # vet, test and build the examples
make test-machine HOST=ip          # everything against a throwaway machine, with a summary
make test-vm                       # the same on a VM created and destroyed for the run
make release                       # tag first; builds and publishes debs and rpms
```

Never run the root suites on a workstation: a transport bug can wedge the kernel for good.
Layout follows the ntfy conventions: `cmd/` (CLI), `config/`, `source/`, `cow/`, `device/`,
`ublk/`, `util/`; see the architecture doc. GitHub Actions runs the unit tests, vet, the
examples and a package build on every push.

## License

Apache 2.0. The `ublk` package derives from [go-ublk](https://github.com/ehrlich-b/go-ublk)
(MIT, Bryan Ehrlich).
