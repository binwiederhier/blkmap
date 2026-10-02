# grpc-remote

A two-call gRPC protocol that exports sparse files from one host and lets blkmap on another
host serve one of them as a block device without ever transferring holes:

- `Open(name)` returns the file size and its data extents (the server walks
  `SEEK_DATA`/`SEEK_HOLE`). The client turns the extents into a `source.Map` and wraps its
  Source with `source.WithMap`, so holes read as zeros locally and hydration skips them.
- `Read(name, offset, length)` is a plain `pread`, at most 1 MiB per call.

The client is an ordinary `source.Source`, so it can also sit behind a cache tier (see
`lib-dircache`) or inside a raid5 segment. This is its own Go module (it pulls in gRPC); the
generated code in `remotepb/` is committed, `protoc` is only needed to change `remote.proto`.

```
cd examples/grpc-remote && go build .
# host A: export a directory
./grpc-remote serve -dir /srv/images -listen :9555
# host B (root, ublk_drv loaded): mount one file, hydrating in the background
sudo ./grpc-remote mount -server hostA:9555 -name disk.img -id remote -hydrate
```

`go test .` runs the protocol over an in-memory connection against a sparse file: holes are
known without a read, reads straddling data and holes fetch only the data.

There is no authentication or TLS; put it on a private network or wrap the listener.
