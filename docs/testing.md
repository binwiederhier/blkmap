# Testing blkmap

How the suite is organized and how to run it. What it verifies, use case by use case with
expected outcomes, is [test-plan.md](test-plan.md). Run the suite after every change that
touches the I/O path, the COW store, device lifecycle, packaging or the unit files, and
record the outcome in `docs/test-results/YYYY-MM-DD.md` (`make test-machine` writes a draft).

## What runs where

| Layer | Command | Needs | Time (about) |
|---|---|---|---|
| Everything below against one machine, with a summary | `make test-machine HOST=ip [POWER=5] [SOAK=120]` | root VM with `ublk_drv`, fio | 15 min + options |
| Release verification: test-vm then a 2 h soak on every kernel template, unattended | `make verify-release` (`nohup scripts/verify-release.sh > logs/verify.log &`) | Proxmox host with the templates | about 5 h |
| Unit tests (race detector) | `make test` | Go | 1 min |
| Examples | `make examples` | Go | 1 min |
| Everything below, on a fresh VM | `make test-vm` | Proxmox host, see below | 16 min |
| ublk and device integration tests | `make test-remote HOST=ip` | root VM with `ublk_drv` | 2 min |
| End-to-end through the deb and systemd | (part of `test-remote`) | same | 1 min |
| Stress workloads | `make test-remote HOST=ip SUITE=stress` | same, plus fio | 3 min |
| Real-life scenarios | `make test-remote HOST=ip SUITE=scenarios` | same | 6 min |
| Daemon kills under a verifying writer | `make powercut HOST=ip MODE=kill CYCLES=5` | same | 2 min |
| Power cuts under a verifying writer | `make powercut HOST=ip MODE=power CYCLES=10` | same; reboots the VM | 4 min |
| Soak: verified I/O under chaos for hours | `make soak HOST=ip MINUTES=120` | same | 2 h |

Never run the root-level layers on a workstation. A bug in the ublk transport can wedge a
kernel until reboot, and the power-cut test reboots the machine on purpose.

## When to run what

- Every change: `make test` and `make examples`. GitHub Actions runs both on every push,
  plus `gofmt`, `go vet` and a package build.
- Before merging anything that touches `ublk/`, `cow/`, `device/`, `cmd/serve.go`, the unit
  files or the maintainer scripts: `make test-vm`. It is the only gate for crash recovery,
  durability, upgrades and systemd behaviour.
- Before a release, and after changes to recovery, handoff or hydration: `make soak` for two
  hours on each kernel, one VM at a time or at least never alongside `make test-vm`.
- After a kernel or systemd upgrade on the target fleet: `make test-vm` with a template that
  runs that kernel (`TEMPLATE=...`). On box11, template 9000 is Ubuntu 26.04 (kernel 7.0) and
  9002 is Ubuntu 24.04 (kernel 6.8, systemd 255); run both before a release.

## make test-vm

`scripts/ci-vm.sh` does the whole run and cleans up after itself:

1. Takes the next free VM id on the Proxmox host (`PROXMOX`, default `root@box11`) and makes a
   linked clone of the cloud-init template (`TEMPLATE`, default 9000: Ubuntu 26.04 server).
2. Waits for the guest agent to report an address, copies the cloud-init user's SSH key to
   root, installs fio, xfsprogs and btrfs-progs, and makes sure `ublk_drv` loads. If the
   template's kernel is older than what the Ubuntu archive carries `linux-modules-extra` for,
   it installs the current kernel and reboots into it.
3. Runs `scripts/remote-test.sh IP all`: builds the deb and the root test binaries here,
   copies them over, and runs the ublk and device suites, e2e, stress and all scenarios
   detached on the VM (a dropped SSH session cannot kill a scenario halfway). It fails if any
   suite prints a FAIL line or a suite never prints its OK line.
4. Runs `scripts/powercut.sh IP 5 kill` and `scripts/powercut.sh IP 10 power`.
5. Destroys the VM, pass or fail. `KEEP=1 make test-vm` leaves it running for debugging.

The run passes when it ends with `== ALL PASSED on <kernel>` and exits 0.

Prerequisites on the machine running it: Go, goreleaser, SSH access to `root@<proxmox host>`,
and the template's cloud-init SSH key in your agent. The template must have the QEMU guest
agent enabled and DHCP networking.

## The layers in detail

**ublk tests** (`ublk/*_test.go`, root): device lifecycle, concurrency, read-only, discard and
write-zeroes, backend errors, two devices, deletion after server death, parallel dispatch,
draining in-flight I/O on close, and user recovery (kill the server process, check that I/O
waits instead of failing, re-attach, check the waiting read completes with the right data;
incompatible and non-recoverable devices are refused).

**device tests** (`device/*_test.go`, root): start from config, read-only, stale symlinks,
library sources, dead predecessors, hydration, detached start, flush on close, periodic flush,
successors racing a close, read-ahead and parallelism against a slow source, aborting a hung
source on close, crash recovery with an unflushed write, SIGHUP handoff by exit and by
re-exec (same pid), replacing an incompatible predecessor, refusing a live server, and reap.

**e2e** (`scripts/e2e.sh`): install the deb, start `blkmap@e2e`, check a zero segment, a gap
and that data written through the device survives a restart.

**stress** (`scripts/stress.sh`): content checks for mixed file and HTTP segments, degraded
RAID-5 and 4K block size; throughput against a 20 ms origin; fio verify workloads;
throughput on a 4 GiB device; ext4, xfs and btrfs with trim, fsck and restart; SIGKILL under
load; cache tiers and hydration with a prefetch list; a sparse image with a map over HTTP
(checks only the data is transferred); stop while mounted; restart churn. Results go to
`/var/tmp/blkmap-stress/results.txt` on the VM.

**scenarios** (`scripts/scenarios.sh`, 38): each runs in a subshell with a 300 s limit, asserts
its outcome, and is followed by a leak check (no ublk device, `blkmap serve` process, mount or
active unit left behind). Run a subset with `scripts/scenarios.sh NAME...` on the VM.

| Group | Scenarios |
|---|---|
| Lifecycle | start_stop_cycles, rapid_restart_reuses_id, sigterm_twice, restart_storm_under_reads, many_devices, huge_device (8 TiB), unprivileged |
| Sources failing | missing_source, origin_down_at_start, origin_dies_mid_flight, origin_hangs_then_stop, cache_tier_vanishes, partial_cache_file |
| Crashes and recovery | kill9_under_write_load (mounted, fio verify keeps running), kill9_during_hydration, origin_down_across_crash, crash_loop_reaps |
| Handoff and upgrades | reload_handoff_under_load, package_upgrade_under_load |
| State and config mistakes | two_devices_one_cow, geometry_change_refused, cow_file_lost, bitmap_lost, cow_disk_full, bad_configs_rejected, source_changed_refused |
| Hydration | stop_during_hydration, hydration_survives_origin_outage, hydration_vs_guest_writes, detached_after_sources_gone, prefetch_beyond_end, record_then_prefetch |
| Filesystems and systemd | partition_table_survives_restart, fstab_mount_dependency, stop_while_mounted, read_only_device, discard_reclaims_space |
| Observability | status_and_metrics |

**power cuts and daemon kills** (`scripts/powercut.sh`, `scripts/powercut/`): a writer stores
checksummed, numbered 4 KiB records in 16384 slots of a 1 GiB device and flushes after every
16; each flush prints an acknowledgment that this side records over SSH. In power mode the VM
is reset mid-write with `sysrq b`, which loses guest RAM without syncing; after it comes
back, `powercut verify` checks that every slot holds at least the newest acknowledged record
mapped to it, and that only slots with an unacknowledged write in flight may be torn. In kill
mode the daemon is SIGKILLed three times per cycle while the writer runs; the writer must
never see an error, and the same verification runs.

**soak** (`scripts/soak.sh`, `scripts/soak-vm.sh`): four devices for the whole run.
`sk-fs` (ext4) and `sk-raw` (file base) take fio random writes with continuous verification,
`sk-http` hydrates from an HTTP origin while a reader checks random blocks against the image,
and `sk-leak` takes the same steady load but is never disturbed. Every one to three minutes a
chaos action hits one of the first three: SIGKILL of the server, `systemctl reload`, an origin
outage of 15 to 40 s, or a page cache drop; after each, every device must be served again
within a minute. Every minute the servers' memory, descriptors and threads are sampled into
`metrics.csv`. It fails on any verify error, a wrong byte, a device not served after chaos, an
unclean fsck at the end, or `sk-leak` growing past 1.5x its memory at 10 minutes (plus 20 MB),
10 descriptors or 20 threads. The fio writers are capped at 150 IOPS each: the soak is about
time and chaos, and uncapped random writes into fresh chunks amplify through copy-up enough to
saturate a shared host disk, which then measures the host.

## Adding a scenario

Add a `sc_<name>` function to `scripts/scenarios.sh` before `# --- main` and its name to the
`all=` list. Use the helpers: `cfg ID <<YML` writes a config and removes its old state,
`unit start|stop ID`, `wait_dev ID`, `origin PORT [args]` starts the HTTP origin, `s=$(since)`
then `logged ID "$s" PATTERN` waits for a journal message, `ok NAME` or `bad NAME REASON` give
the verdict. Clean up everything the scenario started; the leak check fails it otherwise.

## Gotchas

- A VM with a ublk device whose server died hangs on shutdown (sync on the dead device).
  Reset it from the Proxmox host instead of rebooting it from inside:
  `ssh root@box11 'qm stop VMID --skiplock --timeout 15; qm start VMID'`.
- Under `set -o pipefail`, `cmd | grep -q` can fail at random when grep exits early and
  `cmd` dies of SIGPIPE. The scripts use here-strings (`grep -q X <<<"$(cmd)"`).
- journald stores the last lines of a process that just exited a moment later; check the
  journal with `logged`, which polls, not with a single grep.
- `pkill -f PATTERN` kills the calling shell if PATTERN appears in its own command line.
- fio needs `--ioengine=io_uring` for queue depth to mean anything (psync ignores it).

## Fuzzing

Native fuzz targets cover the config parser (`config.FuzzParse`), size parsing
(`util.FuzzParseSize`), prefetch lists and maps (`source.FuzzParseRanges`), Content-Range
(`source.FuzzParseContentRange`) and arbitrary HTTP block replies (`source.FuzzHTTPReply`).
`go test` runs their seeds; to fuzz, `go test -run xxx -fuzz '^FuzzParse$' -fuzztime 60s ./config`
(one target per invocation). The first run found a nil-pointer crash on a YAML list entry
with nothing under it (`segments:\n  -`), fixed 2026-10-03.

## Older systemd

Template 9003 on box11 is Ubuntu 22.04 with the HWE kernel 6.8 and systemd 249, where
`RestartMode=direct` is ignored and the reap unit's 3 second check carries recovery.
`TEMPLATE=9003 make test-vm` passed everything on 2026-10-03 (36/36 scenarios, kills, power
cuts). Building such a template: resize the cloud image first (`qemu-img create 8G` and
`virt-resize --expand /dev/sda1`), or `virt-customize --install` of a kernel fails on disk
space without saying so; check with `virt-ls -a img /boot`.

## Not covered yet

- Soak runs longer than two hours (days).
- A device group (aliases) under chaos: groups are covered by unit tests and one
  kernel-level test only.
