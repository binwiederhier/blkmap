# lib-custom-source

A source your own program computes, used two ways:

- **As a config segment type.** `source.Register("logdisk", newLogDisk)` makes `type: custom`
  with `name: logdisk` available to every config the program opens, so the custom source
  sits next to files, HTTP, RAID-5 and cache tiers, with hydration and recording as usual.
  `logdisk.yml` is such a config.
- **Directly**, with `device.Serve(ctx, &device.Options{Base: source, ...})`.

The source (`logdisk.go`) also shows the optional abilities: `Holes` (`source.Sparse`), so
hydration and read-ahead skip its zero regions; `Present` (`source.Present`), which a cache
asks when the source is a fast tier; and `Identity` (`source.Identifier`), recorded with the
COW file so an overlay is refused when the content version changes.

```
go build ./examples/lib-custom-source
sudo ./lib-custom-source -config examples/lib-custom-source/logdisk.yml
sudo head -c 26 /dev/blkmap/logdisk          # "block 0 of logdisk" repeated
sudo ./lib-custom-source -direct -id logdisk2 -size 256M
```

`go test ./examples/lib-custom-source` checks the content, the holes, the config path and that
a changed version is refused, without a kernel device.
