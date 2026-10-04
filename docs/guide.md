# blkmap user guide

How to use blkmap from the command line and from Go, by use case. The README is the short
version; [architecture.md](architecture.md) explains how it works inside;
[test-plan.md](test-plan.md) lists what is verified.

## The idea in one paragraph

A YAML file names the pieces a disk is made of: files, block devices, HTTP URLs, zero ranges,
RAID-5 sets, cache tiers, or a source your own program provides. blkmap serves them as one
block device, `/dev/blkmap/<id>`, read-only underneath. Every write goes to a separate
copy-on-write (COW) file next to a bitmap of which 64 KiB chunks the COW file now owns, so
the sources are never modified and the device can be restarted, moved and discarded without
touching them. Optionally it copies the base into the COW file in the background
(hydration), in the order a recorded workload needed it, until no source is needed at all.

## Use cases

| You have | Config shape | Section |
|---|---|---|
| a disk image (or several partition images) and want a device | `file` segments, maybe `zero` for a gap | [Quick start](#quick-start) |
| an image on an HTTP server | `http` segment, optional `map:` next to it | [HTTP sources, holes and maps](#holes-and-maps) |
| a slow archive and some fast local space | `cache` with a sparse local copy as `fast:`, plus `hydrate:` from a recording | [Boot fast from a slow source](#boot-fast-from-a-slow-source-with-a-local-cache) |
| the members of a Windows dynamic-disk or md RAID-5, one possibly lost | `raid5` segment | [RAID-5](#raid-5-segments-windows-dynamic-disks-and-others) |
| a live block device you want to try things on without touching it | `device` segment; writes land in the COW file | [Quick start](#quick-start) |
| a program that computes or fetches blocks | `device.Serve` from Go, or `type: custom` | [Library use](#library-use) |
| several devices that depend on each other (RAID restored from data members only) | `device.ServeGroup` from Go | [Device groups](#device-groups) |

## Quick start

Write `/etc/blkmap/<id>.yml`:

```yaml
size: 10G             # optional; defaults to the end of the last segment
block-size: 512       # 512 (default) or 4096
read-only: false
cow:
  file: /var/lib/blkmap/disk1.cow     # default; the bitmap is <file>.bitmap unless "bitmap:" says otherwise
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
the layout, and for RAID-5 the ordered members) and refuses to start once writes exist if
it changed; an HTTP origin whose ETag (or, without one, Last-Modified) changes while running
fails reads rather than mixing old and new blocks, and so does one that stops sending the
validator it had at open. An origin that sends neither cannot be checked, so publish
immutable, versioned objects there. If the content is known to be the same (an image copied
with a new timestamp, a mirror, a RAID-5 set that lost or regained a member), accept it:

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

A partial local copy works as the fast tier as it is. A fast tier that knows which parts of
it hold data is asked first, and any range it does not fully hold is a miss: a sparse file
through its holes (`SEEK_HOLE`), any source through a `map:` of its data extents. So a copy
of just the blocks a boot needs, on local NVMe, in front of a slow archive:

```yaml
segments:
  - type: cache
    fast: {type: file, path: /nvme/vm42.cache}                       # sparse: holes are misses
    slow: {type: http, url: https://archive.example.com/vm42.raw}
```

How that copy is made is up to you (`dd` the ranges of a recording, rsync, anything that
leaves holes where it copied nothing; `blkmap map FILE` prints what a file holds). A dense
copy, or one on a filesystem without hole support, takes `map:` to say what is in it. A zero
region stored as a hole counts as absent and is served by the slow tier, correctly but
slowly, so write zeros out when they are data you want cached.

### Background hydration

With a `hydrate` block, blkmap copies the base into the COW file in the background, so the
device eventually serves everything locally and, once every chunk is there, no longer opens
its sources on start. A prefetch list (`offset length` per line, highest priority first) is
copied first, the rest follows at the configured rate, or not at all with `rest: false`.

Guest requests come first, but hydration is never starved: while the guest is active (a
request in flight or within the last 100 ms) hydration keeps one copy running, and half of
its workers while a timed prefetch list is behind the recording, since those chunks are the
guest's own next reads. An idle guest leaves hydration all its workers. Measured on a
recorded Ubuntu boot from a 10 MB/s mirror, pausing hydration entirely took 86 s, this share
73 s; from a 60 MB/s mirror it stays within noise of the 20 s local boot. `use-cache: never` sends background reads straight to the slow tier.
Ranges known to be zeros (zero segments, gaps) are marked without being copied. Background
reads run `concurrency` at a time (default 4; more helps a high-latency source). Progress
goes to the journal every `report-every` (default 30s).

```yaml
hydrate:
  prefetch-list: /etc/blkmap/img1.prefetch
  rest: true
  rate: 20M
  use-cache: never
```

### Recording a prefetch list

The best prefetch list is the order a real workload first reads the device. A `record` block
writes every guest request to a file, one `millis R|W offset length` line each, starting at
0 when the device comes up (the kernel's partition scan included):

```yaml
record:
  file: /var/lib/blkmap/img1.rec
  max-duration: 60s     # stop after this; default: until the device stops
  max-size: 16M         # stop when the file reaches this (default 16M)
```

Boot the workload once, then compact the recording into a list (reads only, each chunk once
at its first read, chunk-aligned, consecutive chunks merged; each range keeps its first
timestamp) and point `prefetch-list` at it:

```
$ blkmap recording compact /var/lib/blkmap/img1.rec > /etc/blkmap/img1.prefetch
$ blkmap recording stats /var/lib/blkmap/img1.rec
requests:  46 (45 reads, 1 writes) over 27ms
unique:    4352K read, in 21 ranges
needed:
  by 1s     4352K
rate:      everything was read within the first second
```

`recording stats` answers whether hydration can keep up: the list phase runs uncapped, and if the
source delivers at least that rate the workload never waits for a chunk that is still on the
way. A raw recording also works as a prefetch list as it is (writes skipped), just larger.
`--chunk-size` must match the device's `cow.chunk-size` if that is not 64K.

A recording, raw or compacted, carries timestamps, so hydration from it can tell whether it
keeps up. Each progress line compares the COW file with when the recorded workload first
read each listed chunk, counted from when the device came up:

```
vm: hydration list: 1275/57344 chunks (2%), 77696K copied; 1.8s ahead of the recording
vm: hydration list: 2494/57344 chunks (4%), 150720K copied; behind the recording by 1.6s (623 chunks due)
vm: hydration rest: 8958/57344 chunks (15%), 311360K copied; prefetch list complete, 1.2s after the recording needed the last of it
```

`blkmap status` shows the same as a `prefetch:` line, and metrics as
`blkmap_hydration_lead_seconds` and `blkmap_hydration_behind_chunks`. The cost of being
behind is counted directly: "listed chunks read on demand" is how many chunks of the list the
guest had to fetch from the source itself before hydration got there
(`blkmap_hydration_late_chunks_total`), and the source line of `blkmap status` separates
reads a guest request needed from hydration's own (`blkmap_source_demand_reads_total`).

A recording file is never overwritten, so a restarted or reloaded server does not clobber
it; delete it to record again. Recording costs about 1.5 MiB of memory and a few tens of
nanoseconds per request while it runs, and nothing once it has stopped: requests go into a
fixed in-memory buffer that a background goroutine writes out every second, and a burst the
buffer cannot hold is dropped and counted (in the file's last line and `blkmap status`)
rather than slowing the guest.

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


## Boot fast from a slow source with a local cache

The combination that makes a slow archive feel local: record what a workload reads, keep the
first seconds of it on local fast storage, and let hydration fetch the rest from the archive
in the order the workload will want it, ahead of the workload.

1. Record a run from any source you have (a local copy is fine):

   ```yaml
   segments:
     - type: file
       path: /srv/images/vm42.raw
   record:
     file: /srv/images/vm42.rec
     max-duration: 2m
   ```

   Start the device, run the workload (boot the VM), stop. Then compact the recording and
   look at what the workload needs by when:

   ```
   blkmap recording compact /srv/images/vm42.rec > /srv/images/vm42.prefetch
   blkmap recording stats /srv/images/vm42.rec
   ```

2. Build the cache: copy the blocks the first N seconds touched into a sparse file on the
   fast disk, by any means. The compacted list is `millis R offset length` lines; for
   example, for the first 20 seconds, 1 MiB aligned:

   ```
   truncate -s $(stat -c %s vm42.raw) /nvme/vm42.cache
   awk '$1 <= 20000 {print int($3/1048576), int(($3+$4-1)/1048576)}' vm42.prefetch |
     while read a b; do for ((m=a; m<=b; m++)); do
       dd if=vm42.raw of=/nvme/vm42.cache bs=1M count=1 skip=$m seek=$m conv=notrunc,sparse status=none
     done; done
   ```

   Holes in that file are what the cache does not have; `blkmap map /nvme/vm42.cache`
   shows what it holds.

3. Serve from the cache over the archive, hydrating from the list:

   ```yaml
   segments:
     - type: cache
       fast: {type: file, path: /nvme/vm42.cache}
       slow: {type: http, url: https://archive.example.com/vm42.raw}
   hydrate:
     prefetch-list: /srv/images/vm42.prefetch
     rest: true
   ```

   The journal says every 30 s whether hydration is ahead of the recorded workload or behind
   it, and how many listed chunks the guest had to fetch from the archive itself. Measured
   with a recorded Ubuntu boot and an HDD behind a 30 ms link: 42 s from a local file,
   256 s from the archive alone, 45 to 49 s with cache and list
   (details in [test-results/2026-10-03-prefetch-demo.md](test-results/2026-10-03-prefetch-demo.md)).

## Command reference

| Command | What it does |
|---|---|
| `blkmap serve ID [-c FILE]` | serve `/dev/blkmap/ID` from `/etc/blkmap/ID.yml` until stopped; what the unit runs |
| `blkmap validate ID\|FILE` | parse the config, open every source, print the resolved layout |
| `blkmap status [ID] [--json]` | state, chunks in the COW file, I/O and source counters, cache hits, hydration, recording |
| `blkmap metrics` | the same for all devices in Prometheus text format |
| `blkmap map FILE` | the data extents of a file as a map (`offset length` lines) |
| `blkmap recording compact REC [--chunk-size S]` | turn a recording into a prefetch list (reads only, each chunk once, merged) |
| `blkmap recording stats REC` | how much data the recorded workload needed by when, and the rate that keeps ahead of it |
| `blkmap pin ID` | accept the current sources as the ones an existing overlay belongs to (device stopped) |
| `blkmap reap ID` | fail the I/O of a device whose server will not come back (run by `blkmap-reap@`) |

`systemctl start|stop|restart|reload blkmap@ID` run the device; `reload` hands the device to a
new process without disturbing I/O (package upgrades do this). Files: `/etc/blkmap/ID.yml`,
`/var/lib/blkmap/ID.cow` and `.cow.bitmap`, `/run/blkmap/ID` (ublk id and pid), `.bitmap`
(the live bitmap) and `.sock` (status).

## Guarantees and limits


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


Known limits: one copy per request between kernel and process; request buffers of 64 MiB
per device whose reads can reach the network (one ublk queue: a network is kept busy by one
queue thread at depth 64) and up to 256 MiB for local sources (up to four queues), touched
lazily; HTTP fetches are 1 MiB blocks (a 4 KiB demand read costs
a 1 MiB fetch, the right trade for a disk behind a network, wasteful on a metered link);
Windows LDM RAID-5 is reassembled from the geometry you give, blkmap reads no LDM database.

## Library use

The daemon is a thin layer over three packages, and a Go program that computes or fetches
blocks can use them directly. Every recipe below has a complete, tested program under
`examples/` (`make examples` builds and tests them; serving needs root and `ublk_drv`).

| Recipe | Example |
|---|---|
| [Serve a source of your own](#serve-a-source-of-your-own) | `examples/lib-synthetic` |
| [Give a source optional abilities](#give-a-source-optional-abilities) | `examples/lib-custom-source` |
| [Add a segment type to YAML configs](#add-a-segment-type-to-yaml-configs) | `examples/lib-custom-source` |
| [Your own fast and slow backends behind a cache](#your-own-fast-and-slow-backends-behind-a-cache) | `examples/lib-tiered`, `examples/lib-dircache` |
| [Hydrate and record from code](#hydrate-and-record-from-code) | `examples/lib-tiered`, `examples/lib-dircache` |
| [A remote protocol that conveys holes](#a-remote-protocol-that-conveys-holes) | `examples/grpc-remote` |
| [Swap the source under a live device](#swap-the-source-under-a-live-device) | |
| [A Windows dynamic-disk mirror, and no-op writes](#a-windows-dynamic-disk-mirror-and-no-op-writes) | `examples/lib-ldm-mirror` |
| [Device groups in general](#device-groups) | `examples/lib-ldm-mirror` |
| [A block device without the overlay](#a-block-device-without-the-overlay) | |

### Serve a source of your own

Implement `source.Source` (`ReadAt`, `Size`, `Close`) and hand it to `device.Serve`; the COW
overlay, the kernel device and the `/dev/blkmap/<id>` symlink come with it. `ReadAt` follows
`io.ReaderAt`: fill the whole buffer or return an error (a short read with `io.EOF` only past
the end). `Serve` owns the source from then on and closes it, even when it fails.

```go
type synthetic struct{ size int64 }

func (s *synthetic) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = byte((off + int64(i)) / 4096) // block i reads as byte i
	}
	return len(p), nil
}
func (s *synthetic) Size() int64  { return s.size }
func (s *synthetic) Close() error { return nil }

dev, err := device.Serve(ctx, &device.Options{
	ID:      "synth",
	Base:    &synthetic{size: 256 << 20},
	COWFile: "/var/lib/blkmap/synth.cow", // bitmap next to it, 64 KiB chunks by default
})
// /dev/blkmap/synth is live until dev.Close()
```

`ReadAt` is called from several goroutines at once (one per in-flight request on a slow
source), so make it safe for concurrent use. Writes never reach the source.

### Give a source optional abilities

A source can implement more interfaces; blkmap uses each when it is there, through any
wrapper (`Concat`, `Cache`, `Mapped`, `ReadAhead` forward them).

| Interface | Method | What blkmap does with it |
|---|---|---|
| `source.Sparse` | `Holes(off, len) ([]Range, error)` | hydration marks hole chunks without reading; read-ahead skips hole blocks |
| `source.Present` | `Present(off, len) bool` | as a cache's fast tier: a range not fully present is a miss without a read |
| `source.Identifier` | `Identity() string` | recorded with the COW file; a different identity later refuses the old overlay (`blkmap pin` accepts it) |
| `source.Aborter` | `Abort()` | called when the device stops: fail reads blocked on something external now |
| `source.DirectReader` | `ReadAtDirect(p, off)` | hydration with `use-cache: never` reads around cache tiers |
| `source.Binder` | `Bind(source.Lookup)` | in a device group, read sibling devices as the guest sees them |

```go
// A computed disk whose second half is zeros (examples/lib-custom-source/logdisk.go has
// the full version)
func (d *logDisk) Holes(off, length int64) ([]source.Range, error) {
	half := d.size / 2
	if start := max(off, half); start < off+length {
		return []source.Range{{Offset: start, Length: off + length - start}}, nil
	}
	return nil, nil
}
func (d *logDisk) Identity() string { return "logdisk:" + d.version }
```

Pass `Identity: source.Identity(base)` in `device.Options` to pin the overlay to it.

### Add a segment type to YAML configs

Register a constructor, then load configs as usual: a segment `type: custom` with a matching
`name` calls it with the segment's `size` and `params`. The custom source then composes with
everything else a config can do (other segments, cache tiers, hydration, recording).

```go
source.Register("logdisk", func(size int64, params map[string]string) (source.Source, error) {
	return &logDisk{name: params["name"], size: size, version: params["version"]}, nil
})
conf, err := config.Load("logdisk", "/etc/blkmap/logdisk.yml")
dev, err := device.Start(ctx, conf, device.DevDir)
```

```yaml
segments:
  - type: custom
    name: logdisk           # the registered name
    size: 256M
    params: {name: logdisk, version: "1"}
  - type: zero
    size: 64M
```

The stock `blkmap serve` binary does not know your types; this is for your own program built
on the library. `blkmap validate` will reject such a config.

### Your own fast and slow backends behind a cache

`source.NewCache(fast, slow)` reads the fast tier first and falls back to the slow one,
without retries and without ever writing the fast tier. The fast tier says "I don't have
this" by returning `source.ErrNotFound` (any other error counts as a failure and also falls
back), or better, by implementing `Present`, so a missing range never costs a read.

```go
// A fast tier of your own: some 1 MiB blocks in a store (a map here; a key-value store,
// object cache or local directory in real life)
func (m *memTier) Present(off, length int64) bool {
	for b := off / mib; b <= (off+length-1)/mib; b++ {
		if _, ok := m.blocks[b]; !ok {
			return false
		}
	}
	return true
}
func (m *memTier) ReadAt(p []byte, off int64) (int, error) {
	data, ok := m.blocks[off/mib]
	if !ok {
		return 0, source.ErrNotFound
	}
	return copy(p, data[off%mib:]), nil // full version handles reads spanning blocks
}

// A slow remote of your own, abortable
func (s *remote) ReadAt(p []byte, off int64) (int, error) {
	select {
	case <-s.abort:
		return 0, errAborted
	default:
	}
	return s.fetch(p, off) // your RPC
}
func (s *remote) Abort() { s.once.Do(func() { close(s.abort) }) }

base := source.NewCache(fast, source.NewReadAhead(slow))
dev, err := device.Serve(ctx, &device.Options{ID: "tiered", Base: base, COWFile: cow})
st := base.Stats() // Hits, Misses, Failures
```

`source.NewReadAhead` gives a remote without a cache of its own what the HTTP source has
built in: 1 MiB blocks, single-flight fetches (concurrent readers of a block share one
request) and sequential read-ahead. Use it for anything with a round trip per request.
Full programs: `examples/lib-tiered` (in-memory fast tier, delayed remote, abort) and
`examples/lib-dircache` (a directory of block files as the fast tier).

### Hydrate and record from code

The `hydrate:` and `record:` config blocks are `device.Hydrate` and `device.Record`:

```go
ranges, at, _ := source.ParsePrefetchFileTimed("/srv/vm42.prefetch") // from blkmap recording compact
dev, err := device.Serve(ctx, &device.Options{
	ID: "vm42", Base: base, COWFile: cow,
	Hydrate: &device.Hydrate{
		Prefetch:   ranges,
		PrefetchAt: at,        // timestamps: progress then says ahead or behind
		Rest:       true,
		Rate:       64 << 20,  // rest phase cap, bytes/s
		Report:     10 * time.Second,
		OnProgress: func(p device.Progress) {
			log.Printf("%s %d/%d, late %d, %v", p.Phase, p.Hydrated, p.Total, p.Late, p.Schedule)
		},
	},
	Record: &device.Record{File: "/srv/vm42.rec", MaxDuration: 2 * time.Minute},
})
```

`dev.Status()` returns the same snapshot `blkmap status --json` prints.

### A remote protocol that conveys holes

A remote that knows where its data is can say so once, as a map, instead of answering reads
for zeros. `examples/grpc-remote` does it with two RPCs: `Open` returns the size and the data
extents (`source.Extents` of the exported file), `Read` returns bytes. The client attaches
the extents and the block cache:

```go
m, _ := source.NewMap(extents)
base := source.WithMap(source.NewReadAhead(&remoteFile{client: c, name: name, size: size}), m, 0)
```

Hole regions read as zeros locally, hydration marks them without a request, and read-ahead
never fetches them. A 1 TiB sparse export with 4 GiB of data hydrated by transferring 4 GiB.

### Swap the source under a live device

`source.NewSwappable(src)` is a source whose target can be replaced while the device serves:
`Swap(next)` waits for reads in flight on the old target, requires the same size, and returns
the old one for you to close. Use it to move a device from a failing origin to a mirror, or
from a remote to a local copy once it has arrived. The content must be the same; blkmap
cannot check that for you.

### A Windows dynamic-disk mirror, and no-op writes

A Windows mirrored volume is RAID-1 across two dynamic disks: each disk has its own
partition table and LDM metadata, and the volume's data sits on both, byte for byte the same.
With a backup of only one disk, both can be served again from one process: disk 0 from the
backup, disk 1 as its own header plus an alias onto disk 0's data range.

```go
dataOff, dataLen := int64(1<<20), size-(1<<20)-(1<<20) // up to the LDM database, last MiB
disk1, _ := source.NewConcat([]*source.Segment{
	{Offset: 0, Source: header1},                         // disk 1's own first MiB, or zeros
	{Offset: dataOff, Source: source.NewZero(size - dataOff)}, // never read in the alias range
}, size)
g, err := device.ServeGroup(ctx, []*device.GroupOptions{
	{Options: device.Options{ID: "ldm0", Base: disk0, COWFile: cow0}, ElideIdenticalWrites: true},
	{Options: device.Options{ID: "ldm1", Base: disk1, COWFile: cow1}, ElideIdenticalWrites: true,
		Aliases: []device.Alias{{Offset: dataOff, Length: dataLen, Target: "ldm0", TargetOffset: dataOff}}},
})
```

Reads of disk 1's data range see disk 0's bytes and writes there land in disk 0's store, so
the plexes cannot diverge and nothing is stored twice. `ElideIdenticalWrites` handles what
Windows does next: it does not trust the restored mirror and resyncs it, rewriting every
block of disk 1 with disk 0's content. Those writes are identical to what the device already
reads, and elision drops them before they reach the COW file:

```
$ sudo ./lib-ldm-mirror -disk0 disk0.img -demo
resync of 16 MiB onto ldm1: chunks stored before 0, after 0
4 KiB change through ldm1: chunks stored 1; ldm0 reads it back: true
```

Elision works per device without groups too (`cow.Store.SetElision`). It compares against the
base only when the base cannot change (no `Binder` in it), otherwise only against chunks
already in the COW file, and a whole-chunk write never waits on reading the base. The full
program is `examples/lib-ldm-mirror`.

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

### A block device without the overlay

`ublk.Create(&ublk.Params{Backend: b})` serves any `ublk.Backend` (`ReadAt`, `WriteAt`,
`Size`, `Flush`, optionally `Discard` and `WriteZeroes`) as `/dev/ublkbN`, with no COW file,
no symlink and no systemd integration: the transport alone. `Device.Close` stops and deletes
it; with `Params.Recovery` the device survives the process and `ublk.Recover` re-attaches.
