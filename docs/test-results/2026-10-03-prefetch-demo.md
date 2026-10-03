# Prefetch demo, 2026-10-03

Recorded boots on the scratch VM (box11 vmid 990, Ubuntu 26.04, kernel 7.0, 4 vCPU, nested
KVM). Guest: Ubuntu 24.04 cloud image (3.5 GiB raw, 1.8 GiB data) booted with qemu from
`/dev/blkmap/vm`, cloud-init runs the workload and powers off. Assets in `/srv/demo` on the VM
(`run2.sh TAG "SLOW ARGS" CACHE HYDRATE`, `seedcache.py`, `mirror.sh`).

## Workload and recording

Boot, then `tar cf - /usr /var /etc /lib /boot | wc -c` (1.7 GiB), then poweroff: 42.3 s from
the local file. `blkmap prefetch --stats boot3.rec`:

```
requests:  93255 (92375 reads, 880 writes) over 42.223s
unique:    1764288K read, in 10510 ranges
needed:
  by 1s     51520K
  by 5s     82816K
  by 10s    199488K
  by 30s    1348672K
  by 1m     1764288K
rate:      45820K/s from the start keeps ahead of the reads after the first second
```

## Slow origin: a hard disk behind a network

`rangehttpd -disk 10ms,150M -delay 30ms`: one request at a time, a 10 ms seek unless the
request continues the previous one, bytes at 150 MB/s, plus 30 ms per request that overlaps
across requests. Measured with curl: a lone random 1 MiB reader 18 MB/s, eight parallel random
readers 50 MB/s, ordered sequential 56 MB/s. The fast tier is a local `rangehttpd -holes-404`
over a sparse copy holding every 1 MiB block the first 20 s of the recording touched
(1,055 MiB), in a `cache` segment over the slow origin.

| Source | Hydration | Guest time | Reads to the disk |
|---|---|---|---|
| local image file | none | 42.3 s | |
| HDD over HTTP | none | 256.3 s | 92,042 |
| HDD over HTTP | recorded list | 117.9 s | 22,351 |
| 20 s cache + HDD | none | 95.7 s | 39,473 cache misses |
| 20 s cache + HDD | recorded list | 47.2 s, 44.6 s | 11,280 |

With cache and list, hydration ran up to 17 s ahead of the recording and completed the list
1 to 2 s before the recording needed its last chunk.

## Earlier rounds (short 20 s boot workload, 291 MiB)

Token-bucket mirror, half-image cache not derived from the recording:

| Mirror | Hydration pauses for the guest | Never yields | Fair share (shipped) |
|---|---|---|---|
| 60 MB/s, 20 ms | 20.6 s | 22.7 / 33.4 / 29.1 s | 21.0 / 23.4 s |
| 10 MB/s, 50 ms | 86.4 s | 64.2 s | 73.5 s |

Local boot 19.9 s. Pausing hydration for guest I/O starved the prefetch list on the slow
mirror (copied nothing for 70 s); never yielding slowed fast-mirror boots once the uncapped
rest phase competed with the guest; the fair share (one copy while the guest is busy, half the
workers while the list is behind) is what shipped.
