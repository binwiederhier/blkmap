# lib-tiered

Two backends of your own behind a cache tier, the shape of "local fast store in front of a
slow remote":

- `memTier`: holds some 1 MiB blocks (an in-memory map standing in for a key-value store, an
  object cache, a local NVMe directory). It returns `source.ErrNotFound` for blocks it lacks
  and implements `source.Present`, so the cache sends ranges it does not hold to the slow
  tier without a failed read. blkmap never writes to it.
- `slowTier`: a remote with a round trip per request (an image file behind a delay). It
  implements `source.Aborter`, so stopping the device fails reads still waiting on the
  remote instead of hanging, and `source.Identifier`.
- `source.NewCache(fast, source.NewReadAhead(slow))`: the read-ahead gives the remote 1 MiB
  blocks, single-flight fetches and sequential read-ahead.

The device hydrates in the background with `device.Hydrate` and a progress callback that
prints the cache's hit and miss counts.

```
go build ./examples/lib-tiered
truncate -s 256M disk.img && dd if=/dev/urandom of=disk.img bs=1M count=256 conv=notrunc
sudo ./lib-tiered -image disk.img -cached 0:32 -delay 20ms
sudo dd if=/dev/blkmap/tiered of=/dev/null bs=1M count=32 iflag=direct   # cached: fast
```

Hydration copies the rest at 32 MiB/s in the background, so a 256 MiB image is local after
about 8 s and then everything reads fast. On the scratch VM with a 128 MiB image: the device
matched the image, hydration finished in about 4 s, and the progress callback reported
`hydration done: 2048/2048 chunks; cache 39 hits, 170 misses`.

`go test ./examples/lib-tiered` checks the composition and that an abort reaches the slow
tier through the cache and the read-ahead.
