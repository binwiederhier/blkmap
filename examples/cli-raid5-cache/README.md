# cli-raid5-cache

A degraded 4-member RAID-5 (Windows dynamic-disk geometry: 64 KiB stripes, left-symmetric)
whose present members are each a `cache` of a partial local copy over a full copy served by
HTTP, with a prefetch list and background hydration that bypasses the caches. Run as the
systemd unit `blkmap@raid5`.

```
sudo ./run.sh
```

`run.sh` builds the members with `scripts/mkraid5.py`, keeps 4 MiB of each in the fast
tier, validates and starts the unit, verifies the array reads back the original image with
member 1 rebuilt from parity, follows hydration in the journal, then deletes every source and
starts the device again: it comes up from the COW file alone.
