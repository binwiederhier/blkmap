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
