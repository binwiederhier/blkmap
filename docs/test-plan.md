# blkmap test plan

Every use case blkmap is meant to support, as a test a person or an agent can run: what to
set up, what to do, what must happen. Most cases are automated; the last column says by
what, so a run of the suite (`make test-machine HOST=ip`, see [testing.md](testing.md))
covers them, and the manual ones are the few that need a human judgement or hardware the
suite does not have. IDs are stable; refer to them from results files and bug reports.

Conventions: `$dir` is a scratch directory with `img64` (64 MiB of random bytes) and
`img32`; "the VM" is a throwaway machine with root SSH, `ublk_drv`, fio and the deb
installed, never a workstation. Every case ends with the leak check: no `ublk` device in
`/sys/class/ublk-char`, no `blkmap serve` process, no mount, no active `blkmap@*` unit left
behind. Unless a case says otherwise, "start" means `systemctl start blkmap@ID` and the
device must appear as `/dev/blkmap/ID` before the command returns.

## TP-U: unit level (no root)

| ID | Case | Expected | Automated by |
|---|---|---|---|
| U1 | `make test` | every package passes with the race detector | CI, `make test` |
| U2 | Hot paths allocate nothing: store read/write, concat lookup, cache hit and miss, RAID-5 read, map lookups, recording | `AllocsPerRun` is 0 | `*NoAlloc*`, `TestRecorderAddDoesNotAllocate` |
| U3 | Fuzz the parsers for 1 min each: config, sizes, ranges and maps, Content-Range, HTTP replies | no panic, no accepted invalid value | `go test -fuzz` targets in `config`, `util`, `source` |
| U4 | `make examples` | the examples vet, test and build | CI |
| U5 | Static analysis: `make vet` (gofmt, go vet, staticcheck, govulncheck) | no findings, no known vulnerability in a called dependency | CI, `make vet` |
| U6 | The COW store against a model: random writes (random, identical, zero, base bytes), reads, discards, write-zeroes, hydration runs, zero marking, nopwrite toggles, reclaim passes with random budgets (a stored chunk holding its base's bytes is dropped, punched at the next flush), flushes, clean reopens, crashes with and without a live bitmap, writeback; 300 seeds of 400 operations | every read equals the model; after a crash, chunks whose bit changes no flush persisted read what the disk bitmap describes (the base, or a dropped chunk's old bytes); `BLKMAP_MODEL_SEED=N` replays one seed checking after every operation | `TestStoreModel` |
| U7 | The COW store under concurrency: four writers on their own chunks race hydration, zero marking, a flush loop, readers and, every other seed, a sweeper, then a crash | every writer reads back what it wrote; after the crash each chunk reads that, or the base where no flush persisted its bit; clean under `-race` | `TestStoreModelConcurrent` |
| U8 | Reclaim under torture: a mirror plex over a live view of its sibling and a parity column over the live XOR of two data columns, nopwrite on all, writers landing the halves of mirrored writes in either order against a sweeper with small budgets and a flusher, for 3 s; the mirror also with crashes of the plex between rounds, with and without a live bitmap | a quiet chunk reads the same on both plexes (the live XOR on the parity column); once quiet and swept the derived store stores nothing, owes no punch and has nothing allocated; after a crash no chunk reads zeros or bytes it never held, and what stays allocated is exactly what is stored | `TestReclaimMirrorTorture`, `TestReclaimMirrorCrashTorture`, `TestReclaimParityTorture` |
| U9 | Reliance on a sibling only where durable; writeback against reclaim (external review 2026-10-05, findings 01 and 02) | b flushed 0x42, a gets the same bytes unflushed, b reclaims (or skips a write through nopwrite), reboot; and Writeback paused between its bit test and the chunk lock while a reclaim and flush punch the chunk | b reads 0x42 after the reboot; once a has flushed, b skips and reclaims again; the writeback destination keeps the right bytes | `TestReclaimNeverReliesOnAnUnflushedSibling`, `TestNopWriteNeverReliesOnAnUnflushedSibling`, `TestNopWriteAndReclaimRelyOnADurableSibling`, `TestWritebackSkipsAChunkReclaimedMeanwhile` |
| U11 | HTTP identity per resource; short io_uring submissions (external review 2026-10-05, findings 03 and 04) | two URLs with equal size and ETag; the same URL with another query string or credentials; an overlay over `/a` reopened over `/b`; an overlay recorded with the pre-v0.4.2 identity; a scripted io_uring_enter that consumes one entry per call, fails with EINTR or EAGAIN, or never progresses | identities differ per resource and not per query or credentials; the reopen is refused; the legacy identity is accepted and re-pinned; every published entry is submitted, and only a submission that never progresses is an error | `TestIdentityHTTPScopesResource`, `TestStoreRefusesAnotherResourceWithTheSameValidator`, `TestStoreAcceptsALegacyIdentityAndRepins`, `TestRingFlushSubmitsEverythingAfterShortSubmissions`, `TestRingFlushRetriesWithoutProgress` |
| U10 | Coverage | `make coverage HOST=ip` merges unit and root test coverage into `dist/coverage.html`; review functions under 50% in `cow`, `device`, `ublk` after larger changes | `scripts/coverage.sh` |

## TP-C: configuration and validation

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| C1 | Validate a good config | `blkmap validate ID` with every segment type | prints device, size, COW paths, hydration and record lines, the layout table | `cmd/validate_test.go`, scenarios `status_and_metrics` |
| C2 | Reject bad configs | no segments; unknown type; raid5 with two missing members; a map URL that 404s; a misspelled key | `validate` exits non-zero with a message naming the problem | scenario `bad_configs_rejected`, `config` tests |
| C3 | A missing source file | `path:` that does not exist | start fails; journal says "no such file" | scenario `missing_source` |
| C4 | Sizes and offsets | binary suffixes, implicit offsets, gaps, explicit `size:` smaller and larger than the segments | layout as documented; gaps read as zeros; a size inside a segment is an error | `config` tests, `source/concat_test.go` |
| C5 | Geometry pinned by the bitmap | start, stop, change `chunk-size`, start; then change the device size | both starts refused: "chunk size mismatch", "device size mismatch" | scenario `geometry_change_refused` |

## TP-S: sources

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| S1 | File and zero segments stitched | `zero 1M` + `file img64` + gap + `file img32` | `cmp` of the device against the expected concatenation matches | e2e, stress "content" |
| S2 | Block device segment | `type: device` on a loop device | content matches; `blkmap map` on it prints one extent | stress, `source/file_test.go` |
| S3 | HTTP segment | serve `img64` with `scripts/rangehttpd`; a window with `source-offset` and `size` | content matches; a server without Range support is refused at `validate` | stress, `source/http_test.go` |
| S4 | HTTP origin changes the object | after reads, the origin serves a new ETag (or Last-Modified) | further reads fail with "changed on the origin", never mixed data | `TestHTTPVersionChange` |
| S5 | RAID-5 reassembly, all members and degraded | `scripts/mkraid5.py` builds a 4-member left-symmetric set; serve with all members, then with one `missing: true` | content matches the original image both ways | stress "raid5 degraded reassembly", `source/raid5_test.go` |
| S6 | RAID-5 member order is part of the identity | swap two members of a set with an overlay that has writes | start refused as a changed source; `blkmap pin` accepts it | `TestRAID5IdentityOrdersMembers`, `TestStorePinsSourceIdentity` |
| S7 | Cache tier, HTTP fast copy | `cache` with a fast HTTP tier that 404s some blocks | misses fall through to the slow tier; `status` shows hits and misses | stress "cache + hydration" |
| S8 | Cache tier, sparse local copy | fast `file` is a sparse copy with holes punched; slow is the full image | device equals the image everywhere; hits and misses both non-zero, no failures | scenario `partial_cache_file`, `TestCachePartialCopyAsFastTier` |
| S9 | Cache tier, dense copy with a map | fast `file` holds garbage outside its `map:` | the unmapped part is served by the slow tier | `TestFromConfigMappedFastTier` |
| S10 | Cache tier disappears | truncate the fast copy while serving | reads fall through; no error to the guest | scenario `cache_tier_vanishes` |
| S11 | Holes and maps | a sparse image with `blkmap map` next to it over HTTP, hydrated | only the data bytes are transferred (bytes log of the origin); hole regions read zeros | stress "sparse image + map over http" |
| S12 | Custom source | a program registers `type: custom` and serves it | content as the program computes | `source/custom_test.go`, `examples/lib-synthetic` |

## TP-O: overlay, durability and the lifecycle

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| O1 | Writes persist across restart | write through the device, `systemctl restart`, read back | the written data is there; the base is untouched | e2e "blob survived restart" |
| O2 | Filesystems | mkfs, mount, write files, fstrim, unmount, fsck, restart, mount again: ext4, xfs, btrfs | fsck clean, files intact, trim reclaims COW space | stress "filesystems" |
| O3 | Discard and write-zeroes | `blkdiscard` a device with 64 MiB written | COW file allocation drops to about nothing; zeros read back | scenario `discard_reclaims_space` |
| O4 | Read-only device | `read-only: true` | writes fail; `blockdev --getro` is 1 | scenario `read_only_device` |
| O5 | 4 KiB logical blocks | `block-size: 4096` | `blockdev --getbsz`/`getss` report 4096; content matches | stress "4k block size" |
| O6 | Huge device | `size: 8T` over a zero segment; write at the very end | the write reads back; bitmap about 16 MiB; server RSS reasonable | scenario `huge_device` |
| O7 | Many devices at once | 12 devices started in parallel | all appear with the right content; all stop | scenario `many_devices` |
| O8 | Start and stop cycles | 10 start/stop rounds | each start sees the device, each stop removes it, no leak | scenario `start_stop_cycles` |
| O9 | Rapid restarts reuse the kernel id | 3 restarts 2.5 s apart | symlink, `/run/blkmap/ID` and content agree | scenario `rapid_restart_reuses_id` |
| O10 | Stop while mounted | stop with the filesystem mounted | stop waits (DEL_DEV), logs "still mounted"; after unmount the next start finds the data | scenario `stop_while_mounted` |
| O11 | Double SIGTERM | two SIGTERMs in a row | clean exit, no device left | scenario `sigterm_twice` |
| O12 | COW filesystem full | COW file on a 24 MiB tmpfs, write 48 MiB | writes fail with ENOSPC, reads still work, the bitmap still records what landed | scenario `cow_disk_full` |
| O13 | COW file lost, bitmap present | delete the `.cow`, start | refused: "cow file ... missing or truncated" | scenario `cow_file_lost` |
| O14 | Bitmap lost | delete the `.cow.bitmap`, start | device comes up on its sources alone | scenario `bitmap_lost` |
| O15 | Two servers on one COW file | second config pointing at the same `cow.file` | second start refused: "in use" | scenario `two_devices_one_cow` |
| O16 | Changed source refused and pinned | write, stop, touch the base (mtime), start; then `blkmap pin ID`, start | first start refused naming both identities; after pin it starts | scenario `source_changed_refused`, `TestPin`, `TestStorePinsSourceIdentity` |
| O17 | Partition table survives | `sfdisk` a GPT with two partitions, mkfs one, restart | partitions reappear, the file is there | scenario `partition_table_survives_restart` |
| O18 | Unprivileged user cannot serve | `su nobody -c "blkmap serve ID"` | fails; no device appears | scenario `unprivileged` |
| O19 | Package upgrade under load | `dpkg -i` a newer deb while fio verifies | every device keeps serving; the new binary is in charge afterwards | scenario `package_upgrade_under_load` |
| O20 | Faults under the COW file | `make faults HOST=ip`, three scenarios on a dm device under the COW filesystem: **writeback** (dm-flakey `error_writes` on ext4 without a journal, then heal), **journaled** (the same on ext4 with a journal, forced to abort), **readerr** (a one-block dm `error` segment under an acknowledged record) | writeback: the writer's flush fails, the server restarts from the last flushed state, and after healing, a restart, a power loss and a fsck every acknowledged record is there and every other slot reads as the base; journaled: while the COW filesystem refuses writes the server cannot start and reads fail instead of hanging (reap), after a fsck and a start nothing acknowledged is lost; readerr: reading the bad block fails with an error (never zeros or base bytes), other reads work, the server keeps running, reads are right once the block is good | `scripts/faults.sh` |
| O21 | A store fails inside a device group | a mirror group (`fb`'s base is a live view of `fa`); `fa`'s store fails | `Group.Done` closes, `Group.Err` names `fa`; `Group.Abandon` leaves both kernel devices to a successor, which re-attaches and serves the last flushed state, `fb`'s view still showing `fa` | `TestGroupStoreFailureHandsOffToSuccessor` |

## TP-R: crashes, recovery and power loss

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| R1 | SIGKILL under write load, mounted | fio verify on a mounted device, `kill -9` the server | I/O pauses, nothing fails; systemd restarts it within 2 s; fio verify passes; fsck clean | scenario `kill9_under_write_load`, `TestServeRecoversAfterCrash`, `TestLiveBitmapSurvivesCrash` |
| R2 | Daemon kills under a verifying writer | `make powercut HOST=ip MODE=kill CYCLES=3` | the writer never sees an error; every acknowledged record is on the device | `scripts/powercut.sh kill` |
| R3 | Power loss mid-write | `make powercut HOST=ip MODE=power CYCLES=10` (the VM is reset with sysrq) | after reboot every acknowledged record is present; only slots with an unacknowledged write in flight may be torn | `scripts/powercut.sh power` |
| R4 | Hours of chaos | `make soak HOST=ip MINUTES=120`: kills, reloads, origin outages, cache drops under continuous verification | SOAK OK; the undisturbed control device's memory, descriptors and threads stay flat | `scripts/soak.sh` |
| R5 | SIGKILL during hydration | kill the server while hydrating at a capped rate | the restart continues hydration to completion; content matches | scenario `kill9_during_hydration` |
| R6 | Crash while the origin is down | origin down, kill the server, origin back | the restarted server serves; reads recover | scenario `origin_down_across_crash` |
| R7 | Crash loop is reaped | an origin that never comes back | after 10 failed starts in a minute `blkmap-reap@ID` fails the device's I/O instead of hanging it; the unit is inactive | scenario `crash_loop_reaps` |
| R8 | Live handoff under load | `systemctl reload` while fio verifies | no I/O error; the new process has re-attached ("re-attached to ublk device") | scenario `reload_handoff_under_load` |
| R9 | Restart storm under reads | 4 restarts while a reader loops | content correct afterwards | scenario `restart_storm_under_reads` |
| R10 | Origin dies mid-flight | kill the HTTP origin during reads | reads of unhydrated chunks fail with EIO within about a second (3 attempts), the device survives, reads work again when the origin returns | scenario `origin_dies_mid_flight` |
| R11 | Origin hangs, then stop | origin answers after 90 s; a read is blocked; `systemctl stop` | stop completes in under 10 s (the read is aborted), no SIGKILL | scenario `origin_hangs_then_stop` |
| R12 | Origin down at start | start with the origin unreachable | start fails cleanly; it starts once the origin is back | scenario `origin_down_at_start` |
| R13 | Every crash state of the COW filesystem | `make crashreplay HOST=ip N=3000`: the COW file and bitmap sit on ext4 on `dm-log-writes` while a writer stores checksummed records over a random base and marks each acknowledged flush; each logged flush is replayed with the FUA writes and a random part (none, half, all) of the writes not yet flushed, and a device is started from it without a live bitmap | the COW filesystem mounts and fscks clean; every acknowledged record is present and every slot no record reached still reads as the base (a bitmap bit persisted ahead of its data shows as a lost copy-up) | `scripts/crashreplay.sh`, `scripts/logreplay` |
| R14 | Crash states with a guest filesystem and hydration | `make crashreplay HOST=ip MODE=fs N=200 CHECKS=300`: ext4 inside the device over a sparse ext4 base hydrating at 4 MiB/s; the guest creates, fsyncs and deletes files and trims, marking each fsync | in every replayed state: the device reads the same before and after hydration completes (zero marking), the guest filesystem mounts and fscks clean, every file is as its last fsync left it and deleted files stay deleted | `scripts/crashreplay.sh fs` |
| R15 | A ublk queue loop dies while the device is live | close a queue's io_uring | `Device.Done` closes, `Err` names the queue, `Close` still tears the device down | `TestQueueDeathFailsDevice` |
| R17 | Crash states of a mirror group | `make crashreplay HOST=ip MODE=mirror N=2000 CHECKS=400`: `scripts/mirrorcrash` serves two devices as a mirror (m1's base a live view of m0, nopwrite and reclaim, 4 KiB chunks); writes reach the plexes in either order, one plex alone, or the sibling much later, flushed at different times; COW files on ext4 on `dm-log-writes` | in every replayed state (a host reboot, no live bitmaps) each plex holds every write it acknowledged with a flush and every untouched slot reads as the base. Against v0.4.0 it fails (m1 loses acknowledged writes: external review finding 01); fixed it passes 414 of 414 states while the sweeper drops most of m1's chunks | `scripts/crashreplay.sh mirror`, `scripts/mirrorcrash` |
| R16 | Windows with SQL Server for hours | `scripts/windows/soak.sh 24 srv2025 srv2022` (and the other golden images): guests on VirtIO SCSI over blkmap devices, `sqlsoak run` from outside, a disruption every 10-20 min (blkmap kill -9, reload, guest hard reset, power cut of the soak VM) | after each: SQL Server back, every acknowledged commit present with a valid checksum, `DBCC CHECKDB` and `chkdsk /scan` clean; blkmap RSS and descriptors flat | `scripts/windows/soak.sh` |

## TP-H: hydration, recording and prefetch

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| H1 | List then rest, paced | `prefetch-list` with two ranges, `rate: 64M`, `use-cache: never` | listed ranges first, then the rest at about the rate; "hydration done: N/N chunks, 0 errors" | stress "cache + hydration", `device/hydrate_test.go` |
| H2 | Detached start | after hydration completes, remove the source and start | "fully hydrated ... not opening the sources"; content intact | scenario `detached_after_sources_gone` |
| H3 | Hydration never overwrites a guest write | fio verify writes while hydrating | fio verify passes | scenario `hydration_vs_guest_writes` |
| H4 | Hydration survives an origin outage | kill the origin for 4 s mid-hydration | "pass 1: N reads failed, retrying", then "hydration done: N/N" | scenario `hydration_survives_origin_outage` |
| H5 | Prefetch ranges beyond the end | list ranges past the device size | completes with 0 errors | scenario `prefetch_beyond_end` |
| H6 | Stop during hydration | `systemctl stop` while hydrating | stop takes under 5 s | scenario `stop_during_hydration` |
| H7 | Record, compact, replay | `record:` with `max-duration`; read and write through the device; `blkmap recording compact REC > list`; a new device hydrates from it with `rest: false` | the recording ends at max-duration with the right requests; the list has reads only, chunk-aligned, merged; hydration copies exactly the listed chunks | scenario `record_then_prefetch` |
| H8 | Recording limits and restarts | `max-size`, a restart with the file present | the file stops at the limit and stays parseable; a restart does not overwrite it ("exists, not recording again") | `device/record_test.go`, `TestServeRecordsGuestIO` |
| H9 | Ahead or behind a timed list | hydrate from a recording; watch the journal | each progress line ends with "Ns ahead of the recording" or "behind the recording by Ns (N chunks due)"; `status` has a `prefetch:` line; `blkmap_hydration_lead_seconds` is exported | `TestHydratorSchedule`, manual on the demo (see [test-results/2026-10-03-prefetch-demo.md](test-results/2026-10-03-prefetch-demo.md)) |
| H10 | Fair share | hydrate while a guest keeps requests in flight | hydration keeps copying (one copy, or half the workers while behind); an idle guest gets full concurrency | `TestHydratorShare` |
| H11 | Late reads are counted | guest reads a listed chunk before hydration | "N listed chunks read on demand" in progress and status; `blkmap_hydration_late_chunks_total` | `TestHydratorCountsLateReads` |
| H12 | Boot from a slow source with a recorded cache (manual) | record a VM boot; build a sparse cache of the first 20 s; boot again over an HDD-like origin with the cache as fast tier and the list | guest time within a few seconds of the local boot; hydration ahead of the recording; few on-demand source reads | manual; procedure and numbers in the prefetch demo results |

## TP-F: systemd, udev and packaging

| ID | Case | Steps | Expected | Automated by |
|---|---|---|---|---|
| F1 | Install, upgrade, remove the deb | `dpkg -i`, then a newer one, then `dpkg -r` | unit, udev rule, modules-load entry present; upgrade reloads running devices; remove stops nothing it did not start | e2e, scenario `package_upgrade_under_load`, manual for remove |
| F2 | fstab mount depends on the device | an fstab line with `x-systemd.requires=blkmap@ID.service`; `systemctl start <mount unit>` | the mount unit pulls the device unit up and mounts; udev published `/dev/blkmap/ID` | scenario `fstab_mount_dependency` |
| F3 | Type=notify | `systemctl start` returns only when the device exists | `wait_dev` never needed after start | every scenario |
| F4 | Status socket and metrics | `blkmap status`, `status --json`, `metrics` | the documented fields and metric names; root-only socket in `/run/blkmap` | scenario `status_and_metrics`, `cmd/status_test.go` |
| F5 | Older systemd | `TEMPLATE=9003 make test-vm` (Ubuntu 22.04, systemd 249, no `RestartMode=direct`) | all suites pass; recovery relies on the reap unit's check | manual per release |
| F6 | Kernel without user recovery | a kernel older than 6.0 (or `ublk_drv` without the feature) | start logs "the kernel lacks ublk user recovery"; a crash fails I/O instead of pausing it | manual; the code path is `recoverySupported` in `device/service.go` |

## TP-X: performance baselines

| ID | Case | Expected (12 vCPU KVM guest, kernel 6.8; the scratch VM is about 3x slower) | Automated by |
|---|---|---|---|
| X1 | 4K random read, zero base, 4 jobs qd32 | about 1.2M IOPS | stress "throughput" (prints, does not assert) |
| X2 | 4K random write into fresh chunks | about 200k IOPS with the COW file on disk | stress |
| X3 | Slow origin, 20 ms per request, one `dd` stream | about 280 MiB/s (one request at a time would be 50) | stress "slow source" |
| X4 | Benchmarks | `go test -bench . ./source/ ./cow/` | no allocation on hot paths; numbers within 2x of the README's | manual |

## TP-L: library

| ID | Case | Expected | Automated by |
|---|---|---|---|
| L1 | `device.Serve` with a custom `source.Source` | `/dev/blkmap/<id>` serves the computed bytes; writes land in the COW file | `TestServeLibrarySource`, `examples/lib-synthetic` |
| L2 | A fast tier of your own returning `source.ErrNotFound` | misses fall through | `examples/lib-dircache`, `source/cache_test.go` |
| L3 | Holes conveyed once over a remote protocol | the client never requests holes; hydration transfers only data | `examples/grpc-remote` tests |
| L4 | Hydration from code with `OnProgress` | callbacks at every report and once at the end | `device/hydrate_test.go` |
| L5 | `source.NewSwappable` | the target is replaced between requests, sizes must match | `source/swappable_test.go` |
| L6 | Device groups: Binder bases, nopwrite over them, writeback | a mirror plex as a live view of its sibling whose writes never reach the sibling; parity sees guest writes; identical writes cost nothing and the range keeps following | `device/group_test.go`, `TestServeGroupThroughKernel` |
| L7 | `ublk.Create` with a bare backend | a device without the COW layer; stop and delete | `ublk/service_test.go` |
| L8 | A computed source with holes, presence and identity, as a `type: custom` segment and directly | content as computed; holes reported; a changed version is refused | `examples/lib-custom-source` tests |
| L9 | Own fast and slow backends behind a cache | cached blocks skip the round trip; misses fall through; abort reaches the slow tier through cache and read-ahead | `examples/lib-tiered` tests |
| L10 | LDM mirror from one disk's backup, with nopwrite | disk 1's data range reads disk 0; a resync stores nothing; a mirrored write stores one chunk; a write to disk 1 alone is stored on disk 1 and leaves disk 0 untouched | `examples/lib-ldm-mirror` tests (`TestMirrorThroughKernel` needs root) |

## Running it

- The automated part: `make test-machine HOST=ip` (add `POWER=5` for R3, `SOAK=120` for R4);
  the summary names the sections above. On a fresh VM created and destroyed for the run:
  `make test-vm`.
- Manual cases (F1 remove, F5, F6, H12, X4) are run before a release and recorded with the
  rest in `docs/test-results/YYYY-MM-DD.md`.
- A new feature adds its rows here and a scenario or test that covers them; a bug fix adds
  the case that would have caught it.
