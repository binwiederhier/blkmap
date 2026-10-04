# Robustness pass 2026-10-04

Static analysis, coverage, a model-based test of the COW store and crash-point replay of
the COW filesystem (test plan U5-U8, R13).

## Static analysis (U5)

- staticcheck 2026.2.1: no findings. govulncheck v1.8.0: no vulnerabilities. Both now run in
  `make vet` and CI.
- golangci-lint 2.14 with gosec, errcheck, errorlint, nilerr, unparam, gocritic (not in CI,
  too noisy): 219 findings outside tests, nearly all unchecked `Close` and `Fprintf` errors
  on cleanup and output paths, `unsafe` in the ublk uapi code, and integer conversions. One
  was real: `ublk.Params` did not bound `NumQueues` or `MaxIOSize`, so a library caller
  could pass values that wrap when converted to the kernel's 16 and 32 bit fields. Now
  1..4096 queues (UBLK_MAX_NR_QUEUES) and at most 32 MiB requests (UBLK_IO_BUF_BITS).

## Model tests (U6, U7)

`TestStoreModel` (300 seeds x 400 operations, and once 5000 seeds) found a real bug on its
first run, in 3 of 300 seeds: after a crash without a live bitmap (a power loss empties
/run), the COW file can still hold data whose bit never reached the disk; hydration marking
that chunk as a base hole (`MarkZero`) set the bit without touching the data, so a chunk
the guest had been reading as zeros suddenly read the lost write. `MarkZero` now punches
the chunk first. Regression test: `TestMarkZeroAfterCrashReadsZeros`.
`TestStoreModelConcurrent` (20 seeds, run 10 times under `-race`): clean.

## Crash-point replay (R13)

`make crashreplay HOST=192.168.1.9 RECORDS=3000` on the scratch VM (kernel 7.0): 3000
records, 187 acknowledged flushes, 387 logged flushes on the COW filesystem; all 387 crash
states mount, fsck clean, and hold every acknowledged record with the rest reading as the
base. Harness check: with `Store.Flush` deliberately committing the bitmap before syncing
the COW file, the run fails ("slot N holds no record and no longer reads as the base").

## Coverage (U8)

`make coverage HOST=192.168.1.9`, unit plus root tests: 87.0% of statements. Per package
(mean over functions): cow 94.0, config 97.8, util 96.3, source 91.4, device 87.8, ublk
86.3, cmd 74.4. Untested: a queue loop dying while the device is live (`ublk.Device.fail`,
`Done`/`Err`, which make `blkmap serve` exit), the hydration report loop's periodic lines,
and `hydrateFromConfig` with a prefetch file (covered only through the deb in e2e).

## Second round: write errors, guest-filesystem crash replay, dead queues

### Write errors under the COW file (O20): a real bug, fixed

`make faults HOST=192.168.1.9` puts the COW filesystem (ext4 without a journal) on
device-mapper and swaps in dm-flakey `error_writes` mid-write. Before the fix: 240 slots
(15 chunks) read zeros instead of the base after the disk healed, after a restart and after
a power loss. The failed fsync had dropped the chunks' copy-ups (Linux marks failed pages
clean), the retried flush succeeded without them and committed their bits. Now the store
stops at the first failed COW sync (`cow.ErrCOWFailed`: all I/O fails, no bit is committed
again, the live bitmap is dropped) and `blkmap serve` abandons the device to a successor
(exit 75), which re-attaches and serves the last flushed state. After the fix all five
checks pass (healed, restart, power loss, after a fsck of the COW filesystem, writing
again). Unit test: `TestFailedCOWSyncStopsTheStore`. A journaled COW filesystem aborts and
goes read-only on the first failed commit instead, so the test runs without a journal.

### Crash replay with a guest filesystem and hydration (R14)

`make crashreplay HOST=192.168.1.9 MODE=fs N=200 CHECKS=300`: ext4 inside the device over
a sparse ext4 base hydrating at 4 MiB/s; 200 files created, 20 deleted, 220 fsynced changes,
fstrim every 25 files; 1932 logged flushes, 322 crash states checked: all consistent (the
device reads the same before and after hydration completes, the guest filesystem fscks
clean, every file is as its last fsync left it). With the old `MarkZero` the same check
fails 6 of 85 states ("hydration changed what the device reads").

### Dead ublk queue (R15)

`TestQueueDeathFailsDevice`: a queue loop whose ring breaks fails the device (`Done`, `Err`
naming the queue); reads mapped to the dead queue fail at once (the loop's thread exits and
the kernel cancels its commands) and `Close` tears the device down; 15 of 15 runs and three
full ublk suites pass. Killing the queue before udev's probe and the partition scan finish
wedged the kernel twice (udev-worker and an io_uring worker in D state): the known ublk
limitation that a server dying while the device is opened wedges it; the test now settles
udev first. The first version of the test closed the ring's fd, which a parallel test's file
reused and the ring's teardown then closed: `TestDeviceConcurrent` failed with "bad file
descriptor". It replaces the fd with /dev/null instead.

Root suites after these changes (scratch VM, kernel 7.0): ublk and device tests, e2e,
stress (STRESS OK) and 38/38 scenarios pass.
