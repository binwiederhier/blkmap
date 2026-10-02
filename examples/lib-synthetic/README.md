# lib-synthetic

The smallest library use: a `source.Source` whose `ReadAt` computes its bytes (block i of
4 KiB is filled with byte i), handed to `device.Serve`. The COW layer, the kernel device and
the `/dev/blkmap/synth` symlink come with it.

```
go build ./examples/lib-synthetic && sudo ./lib-synthetic -size 1G
sudo dd if=/dev/blkmap/synth bs=4k skip=5 count=1 status=none | xxd | head -2   # all 0x05
```
