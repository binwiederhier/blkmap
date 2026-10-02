# cli-stitch

One YAML file, three source types, served with `blkmap serve` straight from the shell (no
systemd). `run.sh` creates an 8 MiB random image, serves it over HTTP with the Range-capable
test server from `scripts/`, validates and serves `stitch.yml`, checks that both segments
read back the image, formats and mounts the device, writes a file, restarts the server and
shows the file still there: the write lives in the COW file next to the image, which is
unchanged.

```
sudo ./run.sh
```

For a permanent device, copy `stitch.yml` to `/etc/blkmap/stitch.yml` and run
`systemctl enable --now blkmap@stitch`.
