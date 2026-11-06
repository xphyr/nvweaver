# Testing

## Suites

| Suite | Needs | Covers |
|---|---|---|
| `make test` | Go | API, rollback, ordering (fake configfs), SPDK reconcile (fake RPC), sd_notify |
| `test/e2e.sh` | Privileged containers | Real kernel target and Linux initiator: see below |
| `BACKEND=spdk test/e2e.sh` | Same | The same scenarios on SPDK, plus fallback and moving between backends |
| `test/systemd-e2e.sh` | Same | `make install` in a disposable container, the unit, reboot, crash, retries, the Quadlet |
| `test/vm/vm-e2e.sh` | QEMU with KVM, no root | Separate initiator, emulated NVMe, SATA and virtio disks, passthrough I/O, real install and reboot, Quadlet: see below |
| `test/bench.sh` | Same, and `null_blk` | NVMe/TCP overhead, control plane and SPDK CPU use and memory |

`test/e2e.sh` covers CRUD, discovery, access control, data integrity to each backing store,
block sizes, live resize, IPv4 and IPv6, wildcard addresses, overlayfs, multi-target,
multi-host, multipath, DH-HMAC-CHAP, restarts and teardown.

`test/vm/vm-e2e.sh` boots two VMs from `test/vm/lab.sh`, with their own kernels and SELinux
enforcing, linked to each other only. On both backends it exports each disk type by its
`/dev/disk/by-id` path to the other VM, and checks the data on the target's disk. Then it installs
for real as root, and reboots the VM for the native unit and for the Quadlet.

`internal/api/openapi_test.go` compares `api/openapi.yaml` with the code, in both directions: every
route, every response field, and every request field the spec lists. It doesn't check types or status
codes. It reads the spec by indentation to avoid a YAML dependency, so it fails if the layout changes.

The fakes encode what we believe the real systems do, so only the end-to-end suites can
prove behaviour. Anything new that touches a target needs end-to-end coverage.

## Latest results

Fedora 44 VMs on KVM, kernel 7.2.15, SELinux enforcing, 4 vCPUs and 4 GB each, rootful Podman
5.8, SPDK v26.09. Images were built with rootless Podman on the VM host.

| Suite | Path | Result |
|---|---|---|
| Unit tests | | Pass |
| `e2e.sh`, kernel target | Loopback in one VM | 270 of 270 |
| `e2e.sh`, SPDK target | Loopback in one VM | 284 of 284 |
| `systemd-e2e.sh` | Loopback in one VM | 76 of 76 |
| `vm/vm-e2e.sh` | Between two VMs | 906 of 906 |

`go test -race` also passes. Earlier, smaller versions of these suites passed on GitHub's hosted runners, in
CI run 41887410583 on commit `391e438`, with 2-vCPU VMs and Ubuntu's QEMU 8.2.

Earlier versions of the first three also passed on Docker, with 101, 107 and 67 checks. Docker
hasn't been tested since Podman became the default.

`v0.1.0` was also installed from its published tarball and images on fresh VMs, with a second VM as
the host: native unit, SATA and file namespaces, data, a real reboot with no SELinux denials, and both
images. Its tarball's Quadlet can't start, because it names a local image. Fixed for later releases.

### Scale baseline

`test/vm/scale-bench.sh` with 5,000 volumes across 100 node subsystems, before any scale work, on loopback in one VM
with 4 vCPUs and 8 GB, using sparse files and with disk flushes made cheap. Other tests ran at the same time on the kernel
target's run, so treat these as a baseline to compare against, not absolute numbers.

| | Kernel target | SPDK target |
|---|---|---|
| Volumes reached | 5,000 | 5,000, with `fs.aio-max-nr` raised |
| Add one volume, at 1,000 / 5,000 | 50 / 264 ms | 148 / 488 ms |
| Move a volume between nodes, at 1,000 / 5,000 | 135 / 644 ms | 350 / 1,094 ms |
| List everything, at 5,000 | 94 ms | 71 ms |
| Building all 5,000 | 13 minutes | 26 minutes |
| Stop, removing everything | 47 s | 113 s |
| Start, restoring everything | 12 s | 144 s |
| Discovery, and connecting to one subsystem | 0.03 s each | 0.03 s each |
| Target memory | Not measured | 303 MB |
| Service memory | 21 MB | 36 MB |

Adding a volume takes longer as total volumes grow, because every change reconciles everything. With one subsystem per
volume instead, the kernel target took 402 ms per volume, for five API calls, at only 1,000 volumes.

Not measured: many connected hosts, I/O across many volumes, or a real disk.

### Scale, after the work on it

The same benchmark and VM shape, except with 4 GB instead of 8 GB, and nothing else running. These suites and numbers include
the incremental reconcile, the direct copy of the state, and storage work outside the lock.

| 5,000 volumes across 100 node subsystems | Kernel target | SPDK target |
|---|---|---|
| Add one volume, at 100 / 1,000 / 5,000 | 7 / 8 / 20 ms (was 264) | 52 / 52 / 60 ms (was 488) |
| Move a volume, at 5,000 | 84 ms (was 644) | 153 ms (was 1,094) |
| Building all 5,000 | 76 s (was 13 minutes) | 291 s (was 26 minutes) |
| Stop | 46.5 s (47) | 15.2 s (113), by killing `nvmf_tgt` at our time limit |
| Start | 11.1 s (12) | 147 s (144) |
| Discovery, and connecting to one subsystem | 0.03 s, 0.05 s | 0.03 s, 0.03 s |

| 5,000 volumes with a subsystem each, five API calls per volume | Kernel target | SPDK target |
|---|---|---|
| Provision one, at 1,000 / 5,000 | 45 / 221 ms (was 402 at 1,000) | 145 / 286 ms |
| Building all 5,000 | 631 s | 1,046 s |
| Start | 11.1 s | 310 s |
| Target memory | Not measured | 2.1 GB, about 0.37 MB per subsystem |
| Discovery for a host allowed on all of them | **Fails, see below** | 5,000 entries in 5.1 s |

**Every SPDK RPC takes about 8 ms,** measured by timing all 54,000 in a run to 2,000 volumes. `bdev_aio_create` takes about 18 ms.
The time didn't change with the number of volumes, and was the same for a call that does almost nothing. So what a change
costs on SPDK is how many RPCs it makes. The first incremental reconcile barely helped SPDK (440 ms at 5,000) because it checked
that each bdev in the changed subsystem existed. A unit test counted writes only, so it couldn't see that. Now it counts reads
too, and adding a namespace takes 4 RPCs at any size. The cause of the 8 ms hasn't been found. It may be how SPDK polls its socket.
Start on SPDK is mostly this cost, at about 26 ms per volume. Sending RPCs without waiting for each answer might help, and hasn't been tried.

**The kernel limits discovery to about 4,095 subsystems for one host on one port.** Tested directly on configfs on kernel 7.2: 4,091
entries worked. At 4,101, `nvme discover` fails with "failed to get discovery log", and a warning trace appears in `dmesg`. This fits an
allocation limited to 4 MiB, at about 1 KiB per entry. Connecting by NQN still works.

Stopping SPDK doesn't take the target apart first any more. At thousands of volumes `nvmf_tgt` doesn't finish exiting within our 15 s,
so it's killed. Hosts' acknowledged writes have already finished by then. How long it would take left alone hasn't been measured.

**Under load:** `vm-e2e` sends 16 clones, snapshots, creates and grows at once on real storage, on both targets and pool types. Each clone is
then read by a host and compared. 8 identical clone requests at once gave one `201`, and `503` for the rest. The 16 workers themselves were
never told to retry, so a `503` from a full pool or queue is only tested in unit tests, with a fake driver and a blocked backend.

**Not tested at this scale:** the new systemd timeouts, under systemd itself. They come from the timings above.

### Sharing a volume between hosts

On both backends, at the `v0.1.0` code plus the list fix, with host A on a separate VM and host B on the target VM's
own kernel. Host B was added while A had I/O running, with no errors and A's controller unchanged. Both saw the same
NGUID, and each read what the other wrote. Then A was removed from the allow list while connected:

| After removal | Kernel target | SPDK |
|---|---|---|
| A's controller | Stays `live` | `connecting`, retrying |
| A writes | **Succeeds** | Blocks |
| A reconnects after disconnecting | Refused | Refused |

That was the behaviour then. The kernel target now drops the subsystem's connections. `vm-e2e` tests it while all three writers run, with hosts A and B sharing a
subsystem, and B on another one too:

| After removal | Kernel target | SPDK |
|---|---|---|
| A's writes | None finish | None finish |
| A's write reaching the disk, read back by B | No | No |
| A's name, from another machine | Refused | Refused |
| A's controller | Removed, so it has to connect again | Stays `live` until its I/O times out |
| B on the shared subsystem | No errors, 10.3 s stall | No errors, 0.01 s stall |
| B on the other subsystem | No errors, no stall | No errors, no stall |

### Storage pools

Run directly on a Fedora 44 VM with LVM 2.03, before any nvweaver code uses them. On LVM thin, snapshots
and clones survived deleting their origin, read-only snapshots refused writes, and thin snapshots needed
`lvchange -ay -K`. XFS and btrfs reflink worked, and ext4 refused it.

| Thin volumes | `lvcreate` | `lvs` | Metadata area free | Thin metadata used |
|---|---|---|---|---|
| 1,000 | 0.14 s | 0.09 s | 8.4 MB | 2.0% |
| 5,000 | 0.45 s | 0.29 s | 7.3 MB | 3.5% |

With `--metadatasize 16m`, a 1 GB metadata volume and `--config backup/archive=0`. The volumes were empty,
so thin metadata use is a minimum. With default settings, the 507 KB area fell to 221 KB at 1,000 volumes,
and `/etc/lvm/archive` reached 375 MB.

### Snapshots and clones

On reflink and LVM thin, on both targets, checked two ways, as a status code proves little here. These checks are in
`vm-e2e`, the kernel suite and the SPDK suite.

**As raw data, in `e2e.sh`:** a host writes 16 MiB, the volume is snapshotted while the host is connected, and the host
overwrites part of it. The checksums then show that the snapshot holds the old data, that a clone seen by a host holds
the snapshot's data and not the volume's, and that writing to one changes neither of the others.

**As files on a filesystem, in `vm-e2e.sh`,** from a separate initiator:

| Check | How |
|---|---|
| A snapshot is a valid disk | XFS on the volume, and 256 MiB of random files. A clone of a snapshot mounts, and every checksum matches |
| It's a moment in time | Nothing written after the snapshot is on that clone |
| It's crash-consistent under load | A writer records each file once `fsync` has finished. A snapshot taken during that holds every file recorded before it was asked for, after XFS replays its log |
| The filesystem is sound | `xfs_repair -n` reports nothing, on both clones |
| They're independent | Changes on the original or a clone don't appear on the other, and old data stays on the clone |
| It outlives its sources | A clone of a snapshot whose volume has gone, with the snapshot then deleted too, still verifies |
| It shares data rather than copying | `btrfs filesystem du`: 336 MB in a new clone of the 256 MiB of files, none of it exclusive |

Limits: one snapshot under load per run, so a rare race might not show; XFS only; reflink on btrfs in the VMs, and XFS as
a reflink pool only on the CM4, without snapshots; nothing with applications such as databases, or a real power loss.

### LVM thin

On Fedora 44 VMs with an emulated disk, on both targets, with a container using the host's `lvm` through `nsenter`, and on
the kernel target under the systemd unit, as `vm-e2e`:

- **The same real-file snapshot checks as reflink,** including under load. A clone left the pool at the same usage, 4.70%
  before and after, where a full copy of the files would have added over 4%.
- **`kill -9`:** three rounds per target, catching 2 and 3 changes in progress among 132 and 74 confirmed. LVM then held
  exactly what the service knew of.
- **A real reboot,** on the kernel target and the unit: data survived, a snapshot stayed inactive, and it could still be cloned.
- **Its rules:** sizes rounding up, a thin pool that's missing or isn't one, growing while connected, and other LVs ignored.

Not tested: physical disks, activating thousands of volumes in this path, and other distributions' LVM.

### Losing power

`vm-e2e` cuts the target's power with `echo b > /proc/sysrq-trigger`, which reboots with no sync, while a separate host keeps
writing. Afterwards, the host unmounts and re-reads, so that what it sees comes from the target, not its own cache. Every file
whose `fsync` finished before the cut must be intact, and `xfs_repair -n` must find nothing.

| Target | LVM thin | Reflink |
|---|---|---|
| Kernel | Pass | Pass |
| SPDK | Pass | **Corrupted in 5 of 8 rounds, then clean in 8 of 8 with the fix:** see decision 28 |

**What this doesn't show:** QEMU's disks keep every write they've been given, so this loses the target's RAM, but not a disk's
volatile cache. To make up for some of that, the test also counts flushes on the backing disk, showing that a host's `fsync`
reaches it. FUA isn't covered by that count, which is the open question for block devices on SPDK in decision 28.

### A thin pool that fills up

On both targets, a host writing past the pool's size stalls for 60 seconds as LVM queues its I/O, and then gets an I/O error.
The API keeps answering in about 60 ms, warns that the pool is out of space, and refuses new volumes and snapshots with `507`.
After `lvextend`, the same host writes again, with no reconnect, and all its data reads back.

### Crash recovery

- **At each exact point, in `internal/api/intent_test.go`:** a request is stopped after recording its intent, after
  changing the pool, or after saving, for creating a volume, clone or snapshot, deleting a volume or snapshot, and growing.
  Each time, the pool must hold exactly what the state refers to. With recovery turned off, these tests fail.
- **With `kill -9`, in `e2e.sh`, on both targets:** three rounds of killing the service during a stream of creates,
  snapshots and deletes, on real files. Everything the client was told succeeded must be there, with no strays. This
  depends on timing, so it may not hit the danger window on a given run. It prints how many changes were in progress:
  on x86 it caught 1 on the kernel target and 3 on SPDK, among 397 and 60 confirmed changes, and all were settled.

### Arm

A probe on GitHub's `ubuntu-24.04-arm` runner (Neoverse-N2, 4 vCPUs, Azure kernel 7.0), at the `v0.1.0`
code, in run 41906823419:

| Check | Result |
|---|---|
| Unit tests, with the race detector | Pass |
| `e2e.sh`, kernel target, on a real Arm kernel | 106 of 106 |
| `e2e.sh`, SPDK target | 117 of 117 |
| SPDK image, `SPDK_ARCH=armv8-a+crc` | Built in 126 seconds |
| `/dev/kvm` | **Absent**, so the VM suites can't run there |
| `nvmet-tcp`, `nvme-tcp` | Present, compressed with zstd, which the image's `modprobe` handled |

Every suite in emulated Arm VMs, using `LAB_ARCH=aarch64` with images and a Fedora 44 base built under emulation, on
an x86-64 build host, with `TEST_TIMEOUT_SCALE=10`. The code was `aa94560`, plus the test fixes described below:

| Suite | `-cpu max` | `-cpu cortex-a72` |
|---|---|---|
| `vm-e2e` | 129 of 129 | 129 of 129 |
| Kernel target | 159 of 159 | 159 of 159 |
| SPDK | 171 of 171, see below | 171 of 171 |
| systemd | 67 of 67 | **66 of 67,** before the Quadlet fix |

- **The A72 failure was probably a real timeout in the Quadlet, now fixed.** After `systemctl stop` on the SPDK Quadlet,
  the unit was `failed`, not `inactive`. The VM's journal was deleted with the run, but the same result was reproduced on
  a real CM4 with the service's CPU limited, and fixed there: see "A real Cortex-A72".
- **`-cpu max` SPDK:** the full `-cpu max` run gave 166 passed, 4 failed, from the two test bugs under "Test pitfalls".
  The later re-run gave 171 of 171, but its two process-failure checks passed because `nvmf_tgt` was killed by hand,
  as the fix being tested was itself wrong. The corrected test has only run unaided on A72.
- **Time:** about 143 minutes on `-cpu max`, and 202 on A72. A base image took 59 to 66 minutes, and the SPDK image 22.

### A real Cortex-A72

A Raspberry Pi CM4 with 2 GB, on Raspberry Pi OS (Debian 13, kernel 6.18.50+rpt-rpi-v8, 4K pages, CPU features
`fp asimd evtstrm crc32 cpuid`), using the SPDK image built under emulation. Its kernel has `NVME_TCP` and `NVME_TARGET`
switched off, so it's a real host with no kernel target. `e2e.sh` can't run on it, because it needs a local initiator.
So a throwaway script drove it, with an x86 VM on another machine as the initiator, through QEMU's user networking and
over a physical 1 Gb network. **43 of 43 passed**, and that script isn't in the repository:

| Check | Result |
|---|---|
| `NVWEAVER_BACKEND=auto` with no `nvmet-tcp` | Falls back to SPDK, logs why, and `/v1/system` reports it |
| Startup | API answering in under 2.5 seconds, measured over SSH |
| Discovery, access control, and a 128 MiB round trip | Pass |
| `fio` random writes with `crc32c` verification | Pass |
| DH-HMAC-CHAP, with no AES or SHA instructions | No key and wrong key refused, right key accepted, data correct. It used `hmac(sha512)` and `ffdhe8192`, and took under 2 seconds |
| Restart with a host connected | Host reconnected itself; data and identity kept |
| Reflink pool on XFS, in a loop device | Volume mapped, with correct data |
| Killing `nvmf_tgt` | Service exits with status 1 |
| `podman stop` with a host connected | 1.1 seconds, exit status 0 |
| `dmesg` | No illegal instructions, segfaults or out-of-memory kills |
| Memory | About 1.25 GB still available after I/O, with 1 GB reserved for SPDK |

Sequential speed was 16 MB/s writing and 20 MB/s reading, which isn't a measure of the service: QEMU's user networking
and the CM4's eMMC or SD storage are both in the path, and we didn't find which limited it.

The SPDK Quadlet on the same CM4, with no capabilities, in the suite's sequence of start, restart, then stop, with no
host connected:

| CPU limit | Stop took | Unit after stop |
|---|---|---|
| None, 5 runs | About 1.1 seconds | `inactive` each time |
| 20% of a core | 6.8 seconds | `inactive` |
| 5% | 29.5 seconds | **`failed`, status 137:** "StopSignal SIGTERM failed to stop container in 10 seconds" |
| 2% | 60.2 seconds | **`failed`, status 137,** and `TimeoutStopSec=30` also ran out |

Podman's own stop timeout was left at its default of 10 seconds, so a longer shutdown was killed, well before systemd's
30 seconds, and the unit was reported as failed. The Quadlet now sets `--stop-timeout=25`, and with that:

| CPU limit | Stop took | Unit after stop |
|---|---|---|
| None, 3 runs | About 1.1 seconds | `inactive` each time |
| 20% of a core | 6.8 seconds | `inactive` |
| 10% | 11 seconds | `inactive` |
| 5%, 3 runs | 33 to 34 seconds | `inactive` each time |
| 3% | 72 seconds | `failed`: `TimeoutStopSec=30` ran out, and systemd killed it |
| 2% | 80 seconds | `failed`, in the same way |

So the limit moved from about 10 seconds of shutdown to about 30, which was left there as a host this short of CPU has
bigger problems. The emulated A72 run hasn't been repeated with the fix.

Not covered on real Arm: the kernel target on this OS, which needs a different kernel; the plain systemd unit; a Pi 5
and its 16K-page kernel; a physical initiator, as this one was a VM; physical NVMe; snapshots and clones, which aren't built.

### A published release, installed as a user would

`v0.2.0-rc2`, downloaded from its public release, following the README, with a throwaway script that isn't in the repository.
Each did 64 MiB through a separate initiator, read back after reconnecting, and again after restarting the service. No step failed:

| Architecture and target | Installed with | Path |
|---|---|---|
| arm64, SPDK, real CM4 | Tarball's Quadlet, published image | From a VM on another machine, over a physical 1 Gb network |
| x86-64, kernel target, Fedora VM | Tarball's native unit | Between two VMs |
| x86-64, SPDK, Fedora VM | Tarball's Quadlet, published image | Between two VMs |

Checksums and attestations for both tarballs and both multi-arch tags verified, and the published `-spdk` and kernel arm64 images
report the right version on the CM4. CI on the same code gave the full counts on a real Arm runner: 209 and 223, with none skipped.

### A client's LVM on the target

`vm-e2e.sh` runs this on both targets, as `client_lvm_tests`: it detects the exposure, sees the pool go down after a real reboot, and recovers with the documented fix. Before that, throwaway
scripts tried each setup in fresh VMs, with a client making a volume group, and another named like the pool's. All on Fedora's LVM 2.03.38, with its settings changed:

| Setup | Exports | Result, including after a real reboot |
|---|---|---|
| Devices file | Thin volumes, or disks | Client's LVM never seen. Also with `scan_lvs=1` |
| No devices file, `scan_lvs=1` | Thin volumes | Pool `missing`, and changes fail, as soon as the client names a VG like the pool's |
| No devices file | Disks | Fine until a reboot. Then this machine activates the client's LV and the pool goes `missing` |
| No devices file, `global_filter` | Disks | Client's LVM never seen |
| Devices file added later, before clients made LVM | Disks | Client's LVM never seen |

Hosts kept connecting and reading their data throughout, and this machine's copy of the client's LV wasn't open. `lvmdiskscan` took about 0.05 s with 300 thin volumes, on a VM. It hasn't been timed at 5,000.
The defaults for Debian 13 and Ubuntu 22.04 and 24.04 were read from their packages in containers. Nothing has run on those distributions.

### The web interface

`test/webui-e2e.sh` runs the real API and interface on a fake target, in Chromium, through Playwright in a container: every screen, real data, filtering, markup staying text, the security policy really applying, signing in with a token, warnings, a phone-sized screen, and
losing the service. There's no real target or real browser against one in this. `e2e.sh` only checks that the real images serve the files.

**A real web page from another origin attacked the API here.** With `guardBrowsers` switched off, in a throwaway copy, its request created a host, and that check failed. With it on, the request had no effect. Chromium reported "sent" both times, so what the browser says proves nothing: check what happened.

### Web pages using the API

`internal/api/browser_test.go` first failed with the attack working: a cross-site request got 201 and took effect, and DNS rebinding read `/v1/system`. `e2e.sh` sends the same requests to both real targets, using `curl` to send what a browser would. A real browser's attack is under "The web interface".

### The client and command line

`internal/api/client_contract_test.go` compares every field with the spec, in both directions, and reaches every operation in the spec through the client, against the real handlers. Breaking each on purpose was caught: renaming a
field, not calling an operation, and a wrong path. `cmd/nvweaver` runs against the real handlers too, with a fake configfs and no real storage, so what needs pools is only checked by what it sends there. `e2e.sh` runs the binary from the image against both
real targets, including pools, a host writing data, and HTTPS.

### Without `thin_check`

By hand, on Fedora's LVM: with it moved away, `vgchange -ay` still activated a thin pool and its volumes, with exit status 0, saying "Check is skipped, please install recommended missing binary".
A new thin pool could be created too. So it's a lost safety check, rather than an outage. What LVM does on Debian without it hasn't been tried.

### The API's TLS under SELinux

`systemd-e2e.sh` runs nested, with no SELinux, so the SPDK Quadlet with its TLS `Volume=` line was also tried by hand on a Fedora VM,
enforcing, in `container_t`: HTTPS answered, plain HTTP got 400, and a renewal written inside the directory was picked up. Files moved in
from `/root` kept `admin_home_t` and couldn't be read, so the old certificate kept serving and nvweaver logged why, until `chcon` fixed them.

That denial appears nowhere: not in the journal, `audit.log` or `dmesg`. To see whether "no SELinux denials" in `vm-e2e.sh` can fail at all, a
confined container was made to touch unlabelled host paths. All 3 denials were in the journal, matching `audit.log`, so it does catch denials
that the policy logs, but not ones it silences.

## Not tested

Deferred until hardware is available, as tracked in `TODO.md`:

- Physical NVMe drives, including passthrough. So far these are emulated by QEMU
- A physical initiator. One target has been tested over a physical network, from a VM: see "A real Cortex-A72"
- Install and reboot on physical hardware, whose device timing at boot may differ from a VM's

Not tested for other reasons:

- Initiators other than Linux, or any formal NVMe conformance suite
- `compose.yaml`: Compose wasn't available
- Docker since Podman became the default. It's a fallback: see `TODO.md`
- SPDK on a real older CPU. Nehalem was under QEMU, and only as far as a running reactor
- Scale. Nothing has been measured beyond a handful of objects
- Moving `latest` to a multi-arch list on GitHub, which only a final release does. It passed against a local registry, on the
  runners' Podman 4.9

## Benchmarks

`null_blk` over loopback on a loaded VM, with load average changing between runs. These show
that the target isn't the bottleneck there, and little else. Raw loopback TCP on that VM was
653 MiB/s for one stream, and 2957 MiB/s for four.

| Test | Kernel | SPDK interrupt | SPDK polling |
|---|---|---|---|
| 4k random read, QD32 x 4 | 23k IOPS | 41k IOPS | 48k IOPS |
| 4k random write, QD32 x 4 | 22k IOPS | 43k IOPS | 38k IOPS |
| 1M sequential read | 809 MiB/s | 955 MiB/s | 994 MiB/s |
| 1M sequential write | 779 MiB/s | 922 MiB/s | 993 MiB/s |
| 4k mean latency, QD1 | 192 us | 412 us | 218 us |
| Idle CPU | Not measured | About 1% of a core | 100% of a core |
| Target memory | Not measured | About 240 MB | About 240 MB |

The control plane used about 8 MB, and at most 1 CPU tick across all NVMe/TCP tests. SPDK
used one core, and the kernel used all of them. Kernel CPU use wasn't measured, so these
don't show which uses less.

### Reading performance numbers

Results are limited by the hardware available, so record which path a number used:

| Path | Limit |
|---|---|
| Loopback | Memory and CPU. No network limit, so not what a real host would see |
| Between lab VMs | QEMU's userspace link, on one machine. A virtual path, not a real network |
| Physical network | 1 Gb Ethernet: at most about 115 MiB/s, or about 28k IOPS at 4k, before overhead |

On 1 Gb Ethernet the link is the likely limit for throughput on either backend, so a run there
says little about the target's own capacity. Latency, CPU use and behaviour under load remain
useful to compare. A throughput figure from these tests isn't a claim about a faster network.

## Failed experiments

| Tried | Result | Conclusion |
|---|---|---|
| `idle_poll_period_usecs` of 200 and 1000 on `nvmet_tcp` | 4k IOPS fell from about 22k to about 3.7k | Leave it off |
| SPDK with `-s 256` | Bdev pool allocation fails during startup | 256 MB is too little; the default is 1024 |
| SPDK listening on a wildcard address | Every host rejected | Refused: decision 7 |
| Passthrough of a controller on the same kernel | Host logs "ignoring nsid 1 because of duplicate IDs" | A single-machine limit, not a bug. Check with Identify |

## Misleading symptoms

Things that looked like one problem and were another:

- **`/dev/nvmeXnY` missing, sysfs showing the disk:** looked like a timing problem, and
  waiting appeared to help. The namespace was on overlayfs, so hosts got a hidden, zero-size
  disk. Check `/sys/block/nvmeXnY/size` and `hidden`.
- **"Subsystem does not allow host" on SPDK:** the host was allowed. The listener was a
  wildcard.
- **"Failed to initialize OpenSSL" when `nvmf_tgt` starts:** the image lacks `openssl.cnf`,
  from the `openssl` package.
- **`bdev_aio_create` returning `Invalid argument`:** SPDK detects block size with an ioctl
  that fails on regular files, so files need an explicit `block_size`.

## False passes so far

Checks that passed while proving nothing. Watch for these patterns:

- `publish-images.sh` printed `kernel=$(publish_tag ...)` with `echo`, which hides a failure, and `set -e` doesn't apply inside
  `$(...)`, so by reading the code, a wrong-architecture image would still have been pushed. It now assigns first, with `inherit_errexit`. Testing it, comparing
  two digests passed when both reads had failed and were empty, and a check for a refusal passed on an unrelated error: check the
  reason, and that the read worked.
- "No reconnect happened" grepped for a log line that was never written. It now compares the
  controller ID.
- A teardown check counted leftovers with `grep -c`, so a failed mount gave the expected
  zero. It now confirms configfs is readable first.
- The systemd suite's file namespace was on overlayfs, so hosts couldn't use it, yet its data
  checks sometimes passed. They only looked at one device, probably the loop device's
  namespace. The suite now requires both namespaces to be usable, and checks the data on each
  backing store.
- A "stopping closes the port" check would pass if `ss` were missing. It's paired with a
  check that the port was open first.

- "A stranger can't connect" and "a stranger discovers nothing" passed on the VMs while `nvme`
  couldn't connect to anything, because `nvme-tcp` wasn't loaded. They now require a real
  discovery log, and a refusal that isn't that setup error.
- "A mounted disk is refused" skipped itself where `/boot` is a btrfs subvolume, because `findmnt`
  added a `[/boot]` suffix. It now uses `-v`, and fails if it can't find a disk to test with.
- The SPDK failure test hid the output of its `kill`, so a kill that never happened looked just like a service that
  ignores its target dying. The first fix repeated this, calling a `kill` binary the image doesn't have. It now checks
  that it found the process, uses the shell's builtin, and leaves errors visible.

## Test pitfalls

- **`/proc/PID/task/TID/children`** lists only that thread's children. Go can start a process from any thread, so
  reading thread 1 found no `nvmf_tgt` in 1 of 5 starts under emulation. Read `/proc/1/task/*/children`.
- **`dd` to a device that has gone makes a file:** once a host lost its controller, `dd of=/dev/nvme0n1` created a regular file in `/dev` and appeared
  to succeed, which also broke later tests. Use `conv=nocreat`.
- **A flag file left by an earlier run:** `rm` without `sudo` couldn't remove root's file in `/tmp`, so a check for it passed on the next backend, having
  shown nothing. The test now checks that it's gone first.
- **A bare `wait` also waits for a background watcher,** which may be waiting for the caller. Wait for specific PIDs.
- **`nvme connect -l`:** `-l 5` gives a host one reconnect attempt, after 10 seconds, and then it removes the controller.
  A slow target restart misses that, and the device is gone. `e2e.sh` now scales this with `TEST_TIMEOUT_SCALE`.
- **A wait with no check after it:** in `e2e.sh`, after the `KEEP_ON_EXIT` refusal on SPDK, waiting for a host that gave
  up costs 25 to 60 minutes under emulation, and nothing fails. Long quiet periods there aren't a hang.
- **Fedora waits 45 seconds for disks** (`DefaultDeviceTimeoutSec`). An emulated A72 misses that and ends in emergency
  mode, with `lab: tgt did not come up`. `lab.sh` now sets 600 seconds when it builds a base image.
- **`lab.sh down` deletes the console logs,** so a failed boot leaves no evidence. Copy them first.
- **Many short SSH connections:** a test opening a new connection for each step had connections from its machine start
  timing out, while other machines and ping still worked. It cleared within minutes, and didn't recur with one shared
  `ControlMaster` connection. The cause wasn't confirmed, and network changes were being made around the same time.
- **A power cut straight after `podman load`** loses the image, as nothing has synced it, and the service can't start. This looks
  like the service failing. The power-loss test syncs the target first. `POWER_ONLY="spdk reflink 8"` repeats just that test.
- **`pkill -f` over SSH** can match the shell running it, whose command line contains the same text, and end the session.
- **LVM's warnings don't fail anything.** Inside the unit's sandbox it created volumes correctly while being unable to back
  up its metadata or lock itself in memory, and every test passed until a direct check of the backup file. The unit now
  allows these, and the suite checks them. The check on memory only works because the service logs LVM's warnings.
- **Skipped reflink tests:** Go's temporary directory may be tmpfs, where `internal/storage`'s reflink tests skip
  themselves and `go test` still says `ok`. Use `TMPDIR` on btrfs or XFS, and `-v` to see that they ran.
- **Disk names:** don't assume `nvme0n1` is namespace 1 of a subsystem with several. The names
  appear not to follow NSID, though this was inferred rather than confirmed.
- **BusyBox:** `head` rejects `16M`, and `stat -f -c %T` can't name overlayfs, so compare the
  magic number with `%t` (`794c7630`).
- **Containers and `/dev`:** a privileged container copies `/dev` at startup, so it misses
  devices added later unless `/dev` is bind-mounted. The systemd machine deliberately doesn't
  bind-mount it, so loop devices must exist before it boots.
- **systemd in a container:** it shares the host's kernel, so mask anything that could change
  the host (device nodes, sysctls, modules). See `test/systemd/Containerfile`.
- **Quadlet:** units start at boot and can't be disabled, so one retries until its image
  exists. Run `reset-failed` before starting it. `EnvironmentFile=` has no `-` prefix in
  Quadlet. `docker save` names an image `docker.io/library/...`, while a Podman build gives
  `localhost/...`.
- **Nested Podman:** `storage.conf` needs `runroot` and `graphroot` as well as `driver`.
- **SPDK build:** `configure` needs Python's `yaml`, `jinja2` and `tabulate` for its RPC code
  generator.
- **`nvme` says "Failed to scan topology":** `nvme-tcp` isn't loaded. A machine with no NVMe
  devices has no `/dev/nvme-fabrics` until it is.
- **Scripts piped over SSH:** with `ssh host bash -s < script`, the first command that reads stdin
  eats the rest of the script, and it stops silently. Copy the script over instead.
- **`mv` from a home directory under SELinux:** the file keeps its `user_home_t` label, and the
  service fails with status `203/EXEC`. Use `install`.
- **`rsync --exclude nvweaverd`** also skips `cmd/nvweaverd`, and everything still looks fine at
  first. Anchor it, as in `--exclude /nvweaverd`.
- **`podman --device` with a `/dev/disk/by-id` path:** the symlink doesn't exist in the container.
  Give a destination, as in `--device /dev/disk/by-id/x:/dev/nvweaver0`.
- **SELinux checks:** the native unit runs as `unconfined_service_t`, so "no denials" proves little
  there. The Quadlet's `container_t` is the meaningful test.
- **QEMU:** with extra disks, the root disk needs `bootindex`. An SSH agent with many keys can use
  up a VM's login attempts, hence `IdentitiesOnly`. `if=virtio` drives take IDs like `virtio0`.
- **Podman versions:** the runners have 4.9, which refuses `podman manifest inspect docker://...`, as 5.8 doesn't. That would have
  failed a release after pushing its images. Try release steps in `ubuntu:24.04`, using `vfs` storage.
- **A green Arm job can hide skipped tests.** On the runner's ext4, `e2e.sh` skipped the reflink sections and reported 115 and 129
  rather than 209 and 223. CI now puts Podman's volumes on XFS, and sets `REQUIRE_REFLINK=1` so that skipping fails. Compare the counts.
- **`linux-modules-extra` on Arm runners:** there's no such package for the `azure` 7.0 kernel on arm64, and installing it fails the
  job. `nvmet-tcp` and `nvme-tcp` are in its base modules. The first probe saw this, but tolerated the error, so it went unnoticed.
- **`gh attestation verify` prints nothing when not on a terminal,** even on success. Check its exit status, not its output.
- **`test/webui-demo` keeps sparse volumes in `.webui-demo-*` in the repository,** which `rsync` copies at full size unless it's excluded: tens of GB of zeros, as happened once. Sync with `--exclude '/.webui-demo-*'`, or stop the demo first.
- **Playwright's `waitForFunction` needs `eval`,** which the interface's security policy refuses. Use `waitForSelector`, rather than turning the policy off for tests.
- **Moving between screens only changes the hash,** so a check right after that can read the old screen. Wait for something on the new one.
- **A unit test can depend on the machine by accident:** one looked up `/dev/sdb` for real, and passed where that exists.
- **`use_devicesfile=0` deletes the devices file,** at the next LVM command of any kind, keeping a backup. So a test that turns it off and on again has to make a new one. It looks as if the setting did nothing.
- **`pvs -a` doesn't list everything LVM can read:** it leaves out a disk with no PV on it, whatever else is on it, though the first experiment suggested otherwise. Use `lvmdiskscan`.
- **`ausearch` reads stdin when it isn't a terminal,** so in a script piped to `bash -s`, it swallows the rest of the script. Use `</dev/null`.
- **One machine:** an initiator can't see the same namespace twice. Two host NQNs on one
  kernel also share a multipath device, so test multi-host by disconnecting one and reading
  through the other.
