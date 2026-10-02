# lib-dircache

A fast cache tier implemented as a directory of 1 MiB block files, `<dir>/<index>.blk`: a
read whose block file is missing (or short) returns `source.ErrNotFound`, which the
`source.Cache` counts as a miss and serves from the slow tier (here a local image file; the
gRPC example's client would do just as well). blkmap never writes the directory; `-populate`
stands in for whatever process fills it. Hydration runs in the background with
`UseCache: never`, so the cache only ever serves guest reads, and a statistics line reports
hits and misses every 5 seconds.

```
go build ./examples/lib-dircache
head -c 64M /dev/urandom > /tmp/img; mkdir /tmp/cache
sudo ./lib-dircache -slow /tmp/img -cache /tmp/cache -populate 0:8
sudo dd if=/dev/blkmap/dc bs=1M count=4 of=/dev/null     # hits
sudo dd if=/dev/blkmap/dc bs=1M skip=32 count=4 of=/dev/null   # misses, served by the image
```

`go test ./examples/lib-dircache` exercises the tier without a device.
