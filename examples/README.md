# blkmap examples

Each directory is self-contained and says what it needs. The CLI examples need the `blkmap`
deb installed and root; the library examples build with `go build ./examples/...` from the
repository root (the gRPC one is its own module, see its README) and need root to create the
device. All of them were run on the scratch VM used for blkmap's tests; `run.sh` scripts
leave nothing behind.

| example | shows |
|---|---|
| `cli-stitch` | one YAML stitching zero + file + http segments, served by `blkmap serve`, formatted and mounted |
| `cli-raid5-cache` | a degraded 4-member RAID-5 whose members sit behind cache tiers, with a prefetch list and background hydration, run as a systemd unit |
| `lib-synthetic` | the smallest library use: a `source.Source` computed on the fly served by `device.Serve` |
| `lib-dircache` | a fast cache tier implemented as a directory of block files (missing file = miss) in front of a slow source, with hydration and cache statistics |
| `grpc-remote` | a gRPC protocol exporting sparse files from a remote host: size, data-extent map, reads; the client is a `source.Source` with holes, so hydration never transfers zeros |

## Verified runs (2026-10-02)

All examples were run on the scratch VM (Ubuntu 26.04, kernel 7.0) with the blkmap deb
installed; `grpc-remote` additionally across hosts, with the export server on the VM and
the block device on a second machine:

- `cli-stitch`: both segments read back the image, ext4 formatted and mounted, the file
  written survived a server restart in the COW file (70 chunks), the image untouched.
- `cli-raid5-cache`: the degraded array read back the original image, hydration finished
  (384 chunks, 24 MiB), and the device came up again with every source deleted.
- `lib-synthetic`: block 5 reads as `0x05`.
- `lib-dircache`: the device matched the image; the statistics line showed 180 hits and
  984 misses for 8 cached blocks out of 64 while hydration (cache bypassed) completed.
- `grpc-remote`: a 2 GiB sparse export with 16 MiB of data at offset 1 GiB mounted on the
  other host; the data region matched, the hole region read zeros without requests,
  hydration copied 16 MiB and the COW file is 16 MiB.

### grpc-remote at scale (2026-10-02)

Export: a 1 TiB sparse file on the scratch VM (2 vCPU) holding 4 GiB of random data in eight
512 MiB regions. Client: codebox (12 vCPU KVM guest) over a 1 GbE LAN with a NAT hop in
between. Device chunk size 64 KiB, so the bitmap has 16,777,216 bits.

| step | result |
|---|---|
| Open RPC (size + 8-extent map) to live device | 0.11 s |
| 512 MiB of data, one `dd bs=1M iflag=direct` stream over gRPC | 94 MiB/s (was 22.6 before read-ahead and parallel dispatch) |
| sequential 4K direct reads of data over gRPC | 16,885 IOPS (was 2,892) |
| 4 GiB of hole region, before hydration | 10.9 GB/s, no requests (zero-filled from the map) |
| full hydration of the 4 GiB, 4 workers | 47 s, 87 MiB/s; 4,106 MiB received for a 1 TiB device |
| client during hydration | 236 MiB peak RSS (request buffers of up to 256 in-flight reads plus the 64 MiB block cache), 28 s CPU |
| COW file after hydration | 4.1 GiB allocated of 1 TiB; bitmap 2.0 MiB |
| 512 MiB of data after hydration (local COW file) | 3.8 GB/s |
| 4 GiB of hole region after hydration | 884 MiB/s (now read from the sparse COW file instead of the map) |

Reading the same region on both sides gave the same SHA-256. The single-stream figure was
latency bound before the source-side read-ahead and parallel dispatch (one RPC per request as
the kernel issued them); now a single reader runs within about 10% of what hydration's four
workers get, which is the link's practical limit.
