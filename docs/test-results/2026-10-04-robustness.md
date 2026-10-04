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
