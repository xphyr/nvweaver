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
| `test/vm/k3s-lab.sh check` | QEMU with KVM, `python3`, no root | The Kubernetes lab for the CSI driver: pods on every node, and NVMe/TCP from every node to nvweaver. See below |
| `test/vm/k3s-csi.sh deploy`, `test` | The k3s lab | The CSI driver in Kubernetes, end to end: see "The CSI driver" below |

`test/e2e.sh` covers CRUD, discovery, access control, data integrity to each backing store,
block sizes, live resize, IPv4 and IPv6, wildcard addresses, overlayfs, multi-target,
multi-host, multipath, DH-HMAC-CHAP, restarts and teardown.

`test/vm/vm-e2e.sh` boots two VMs from `test/vm/lab.sh`, with their own kernels and SELinux
enforcing, linked to each other only. On both backends it exports each disk type by its
`/dev/disk/by-id` path to the other VM, and checks the data on the target's disk. Then it installs
for real as root, and reboots the VM for the native unit and for the Quadlet.

`test/vm/k3s-lab.sh` is the Kubernetes lab for the CSI driver, from the same base image: a k3s
server and two agents, and nvweaver on a fourth VM with an emulated NVMe disk and a virtio disk
for LVM. QEMU's stream links join only two VMs, and multicast would need a loopback that allows
it, so they share a link through `k3s-switch.py`, a small Ethernet switch on a Unix socket. k3s is
a pinned release, checked against its checksum, and installed only in the VMs, with SELinux
enforcing. `check` proves the lab works: a pod on each node, the system pods, a different
`hostnqn` on each node, and each node connecting to nvweaver over NVMe/TCP and seeing the right
size, with nothing left on either side. With the switch killed, it fails both pod checks on the
agents and all three connections, so it does test the link. The lab uses about 10 GB: 3 GB for
the server and nvweaver VMs, 2 GB per agent, 2 vCPUs each. It runs beside `lab.sh`'s VMs only if
there's memory for both.

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
| `e2e.sh`, kernel target | Loopback in one VM | 311 of 311 |
| `e2e.sh`, SPDK target | Loopback in one VM | 327 of 327 |
| `systemd-e2e.sh` | Loopback in one VM | 76 of 76 |
| `vm/vm-e2e.sh` | Between two VMs | 949 of 949 |
| `webui-real-e2e.sh` | A browser, both targets in VMs | 93 of 93 |
| `webui-e2e.sh` | A browser, no root | 245 of 245 |
| `vm/k3s-csi.sh` `test`, `sanity` | k3s on three VMs | 34 of 34, 75 of 75 |

The CSI lab ran on 2026-11-09, with decisions 48 and 49. The rest ran on 2026-11-10, on the working tree with changed block
tracking (decision 50), by `run-all.sh` and `webui-e2e.sh`, with `vm-e2e.sh` run again after its new checks were fixed.

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

### iSCSI: is it feasible?

A probe, with throwaway scripts that aren't in the repository, before any design. Fedora VMs, kernel 7.2.15, driving LIO through configfs and SPDK v26.09's `iscsi_tgt` through RPC, with `open-iscsi` on a separate initiator. x86-64 and emulated disks only. **The answer is yes, on both targets, with differences that would have to be designed around.**

| Question | LIO | SPDK `iscsi_tgt` |
|---|---|---|
| Thin LV and reflink file export, 16 MiB round trip after reconnecting | Pass | Pass |
| A clone, with its source's data and a new identity | Pass, for an LV and a reflink file | Not tried |
| A host not on the list | Refused | Discovery shows it nothing. A direct login wasn't tried |
| Keeps its identity when recreated under another target | Yes | Yes, if the bdev's name stays the same |
| Where identity comes from | A serial you set. NAA `6001405` + the first 25 hex digits, so it can come from `nguid` | Only the bdev's name: a chosen `--uuid` was ignored. An 8-byte hash of the name as NAA, and the name as serial, cut to 31 characters. Vendor is always `INTEL AIO disk` |
| Removing a host from live sessions: its marker write | Fails, and doesn't reach the disk | The same, but only after `iscsi_target_node_request_logout`. Removing it from the list alone leaves the session writing |
| Can it log back in | No | No |
| Longest stall for a host sharing the target | 0.07 s, so none | 2.05 s, with no errors |
| A host on another target | Unaffected | Unaffected |
| Growing, with a host connected | Only seen after the host rescans: NVMe tells hosts by itself | Not tried |
| Power cut, thin LV, one round | 22 files, none lost, `xfs_repair` clean | 22 files, none lost, clean |
| Power cut, file, one round | 20 files, none lost, clean. The same with `fd_buffered_io`, though its identical flush counts suggest looking again at whether that applied | **Corrupted in one round of two:** the filesystem wouldn't mount, and all 55 journaled files were lost. It's the same as NVMe without `chattr +S` |
| Power cut, file with `chattr +S`, one round | | 36 files, none lost, clean. It needs more rounds to mean much, as the failure above is intermittent |

Other findings:

- **Fencing is better than on NVMe's kernel target,** whose other hosts stall for about 10 s.
- **A volume would have different identities on the two iSCSI targets,** which NVMe doesn't have, so moving between them couldn't be promised. LIO's and SPDK's can't be made to match, as SPDK's comes from a hash and a fixed vendor string.
- **Nothing stops two protocols exporting one device:** with LIO holding a device, `nvmet` enabled it too. A volume being mapped once, as now, would have to cover both.
- **The SPDK image would need another binary,** as `iscsi_tgt` is separate from `nvmf_tgt`. The build makes it already, and `spdk_tgt` too, which wasn't tried and may do both.
- **Raspberry Pi OS has no LIO** (`CONFIG_TARGET_CORE` is off), as with `nvmet`, but has the `iscsi_tcp` initiator. SPDK's target wasn't tried on Arm.

Not tried: CHAP, multipath, snapshots under load, performance, Arm, real hardware, and `iscsi_tgt` at scale.

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
losing the service. There's no real target in this: see the next section. `e2e.sh` only checks that the real images serve the files.

**A real web page from another origin attacked the API here.** With `guardBrowsers` switched off, in a throwaway copy, its request created a host, and that check failed. With it on, the request had no effect. Chromium reported "sent" both times, so what the browser says proves nothing: check what happened.

### The web interface on real targets

`test/webui-real-e2e.sh` drives both real targets from Chromium, in VMs, through a tunnel, with what the fake target can't show: a real LVM thin pool adopted on the Pools page, a port and host added there, a volume created and exported, and over real HTTPS, with a real password from `nvweaver passwd`. The initiator VM runs the `nvme connect` command that the page displayed, and writes 16 MiB, which reads back and is checked in the LV on the target. Then a snapshot and clone, the clone reaching the host in the same subsystem with the snapshot's data, removing the host on the page really stopping its I/O, and everything taken apart from the interface, leaving no LVs. It checks that LVM has 3 volumes first, so that finding none afterwards means something.

93 checks passed, on x86-64. Its first run failed twice, both in the test: un-exporting a subsystem's last volume removes the subsystem by default, so there was nothing left to delete, and it expected `used_bytes` to be at least what was written. LVM reports a percentage to two places, so 16 MiB came back as 16,750,372 bytes, which the API now documents.

The browser is told to accept the test's own certificate, so trusting a CA isn't covered. It's the interface's happy path: errors and every field are in `webui-e2e.sh`.

### SBOMs

`make-sboms.sh` ran on real amd64 images and packages. Syft 1.57 listed the Go standard library, `x/crypto` and the distribution's packages, but not SPDK, DPDK or ISA-L, and `nvweaver` had no version, which the script adds. It failed as it should for a row in the notices that it doesn't know, a component that the notices list and an SBOM lacks, and an SPDK image with nothing recorded. Not tried until a release: arm64's images, from the publish job, or whether a vulnerability scanner uses the added packages.

**The SBOMs found a real problem in `v0.3.0-rc1`.** Its tarballs and CLI were built with Go 1.26.0, and its images with 1.26.10. Vendoring `x/crypto` had left `go 1.26.0` in `go.mod`, which `setup-go` followed. No test looked at this. CI now asks for the newest 1.26, and `make-sboms.sh` fails if everything wasn't built with one version.

### Who's connected

Two probes in VMs came first. Fedora's 7.2 kernel has no `/sys/kernel/debug/nvmet`, though a connected host showed as 5 established sockets, one for each queue, in `/proc/net/tcp` as seen from the service's container. SPDK's `nvmf_subsystem_get_qpairs` gave the host, controller and both addresses, and an unknown subsystem is refused with "not found". The unit tests use those formats. `e2e.sh` and `webui-real-e2e.sh` connect real hosts to both targets. Not tested: a kernel with debugfs, which isn't used, IPv6, or listing everyone with thousands of subsystems.

### Signing in

`internal/auth` is tested with a real `htpasswd -B` hash. `e2e.sh` runs on both real targets, with `nvweaver passwd` from the image, against a service on `0.0.0.0`, reached by a real non-loopback address: a password over plain HTTP is refused, on loopback it's fine, and with the override it's allowed. The browser test signs in and out, and has a hostile page
in the same browser, with the session in its cookies, try to read and change things through it, which had no effect. Signing in over real TLS in a browser is in the next section. Not tested: signing in under the Quadlet, with SELinux. `e2e.sh` checks that the service refuses plain HTTP, and a unit test that TLS is accepted and marks the cookie `Secure`.

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

### The suites, audited with deliberate bugs

To find what each suite is worth, 26 bugs were put into the code one at a time, each a single small edit, on x86-64 VMs. Each ran
through the suites it could reach, and then the unchanged code through all of them, which passed: 297 unit tests, 235 in the browser,
301 on the kernel target, 318 on SPDK, 76 under systemd, 906 in the VMs and 93 on real targets in a browser.

| Suite | Time | Caught alone, among the end-to-end suites |
|---|---|---|
| Unit | 3 s | SPDK's discovery listener (see below), and 8 more that otherwise only `vm-e2e.sh` caught |
| `e2e.sh`, kernel | 160 s | A host's DH-HMAC-CHAP key not written |
| `e2e.sh`, SPDK | 150 s | None, but it's all that runs on Arm |
| `systemd-e2e.sh` | 70 s | The unit's sandbox removed |
| `vm-e2e.sh` | 17 to 18 min | The state file not synced: only a real power cut finds it. Also fencing, durable SPDK files, LVM snapshots made writable, LVM deletes leaking, no reservations, a full pool unreported, no LVM warning |
| `webui-real-e2e.sh` | 11.5 min, now about 2 | The session cookie without `Secure` |
| `webui-e2e.sh` | 27 s | None alone: it shares the connect command and cross-origin checks |

So no suite can go. A bug in code that both backends share was caught by both backends' copies of a section in `vm-e2e.sh`, and a
bug in one backend only by that backend's copy. Storage-only sections could therefore run on one backend, saving about 3
minutes. That isn't done, as it's a loss of coverage for the user to decide on.

What else it found, and what changed:

- **Nothing caught a reflink snapshot made writable.** The one unit test that checks skips itself without reflink, as on tmpfs and
  ext4, saying that the end-to-end suites cover it, which they didn't. `e2e.sh` now checks the snapshot's mode.
- **`webui-real-e2e.sh` spent 614 of its 694 seconds on one check,** a write that should fail after the host is removed. On SPDK
  it blocks uninterruptibly until the host gives up on the controller, after 600 seconds, which `timeout -s KILL` can't end. It
  now runs in the background, as in `vm-e2e.sh`, and disconnecting ends it.
- **The crash test found no change in progress in 11 of 54 runs,** and passed, with recovery unexercised. It now adds rounds,
  up to 12, until a crash catches one, and fails otherwise. Its cleanup made one `podman exec` for each of about 480 deletes,
  40 seconds per backend, and now makes one.
- **SPDK hosts discovered every subsystem with the code that adds discovery listeners removed,** in both `e2e.sh` and
  `vm-e2e.sh`, so SPDK 26.09 serves discovery without it. Only a unit test notices. What the code is for is in `TODO.md`.
- **A test bug, not a product one, failed Arm CI once:** reading a counter that `echo n > file` was rewriting found it empty. The
  counter is now renamed into place.
- `make-sboms.sh` left a container behind when copying from it failed, as `set -e` exited before removing it.

### The outside review's findings

Each fix came with a test that failed first, or, for crash points that didn't exist before, failed with the fix turned off. Then
every suite ran on x86-64 VMs: 330 unit tests, 240 in the browser, 308 on the kernel target, 76 under systemd, 915 in the VMs and
93 on real targets in a browser, all passing. SPDK failed 1 of 325: a host whose key changed stayed connected after
`nvmf_subsystem_set_keys`, still `live` after 10 s. SPDK now removes the host and adds it again, and passed all 325.

- **Passthrough, on a real controller:** on emulated PCIe NVMe, a mounted namespace, an exported one, and the same controller twice
  were all refused, and so was exporting a namespace of a controller that's passed through. Native multipath, which these
  controllers don't use, was checked by hand on the same VM, with `nvme-loop`: the controller's directory listed only
  `nvme2c2n1`, which has no device node, and `nvme2n1` was only in `nvme-subsys2`. The unit tests use both layouts.
- **SPDK says a missing bdev is "No such device",** code -19, which is now the only error that means absent. Every namespace
  created in the SPDK run went through that check.
- **Not tested:** passthrough of a controller with native multipath through the API, as none is available to pass through, and
  a key change with two hosts on one SPDK subsystem, where only the changed one should drop.

### The second pass

Each fix came with a test that failed first, or, where the functions were new, failed with that part of the fix turned
off. The key dialog's three new browser checks all failed against the old dialog. Then, scoped by decision 39, on x86-64
VMs: unit tests and lint, 243 in the browser, 311 on the kernel target, 327 on SPDK and 915 in the VMs, all passing.
`systemd-e2e.sh` and `webui-real-e2e.sh` weren't run: nothing they cover changed.

- **The new `e2e.sh` check failed at first, on its own mistake:** it read the capability from a response that an earlier
  step had already replaced with a connection list. The check was moved, and SPDK passed all 327.
- **`vm-e2e.sh` maps several thin volumes of one pool at once,** on real LVM, which would fail if volumes built on the same
  devices were taken to overlap. They weren't. The development machine has no device-mapper devices, so the test of the
  walk on a real device skips there.
- **`webui-e2e.sh` needs its tree on XFS or btrfs,** as the demo makes a reflink pool. Run from a tmpfs, it stops with "the
  demo didn't start".

### The CSI driver

`test/vm/k3s-csi.sh` deploys nvweaver on the k3s lab's fourth VM, serving its API over TLS with a token on the lab link, on an
LVM thin pool, and the driver with the upstream sidecars and snapshot controller. Its `test` passed all 25 checks, against
the kernel target: a filesystem volume written on one node and read on another, its move to that node's subsystem, growing
the volume and then its filesystem, a snapshot and a clone with the snapshot's data, a raw block volume written on one node
and read on another while both had it, and nothing left after everything was deleted, in nvweaver or on the nodes.

**On Kubernetes 1.31,** the oldest supported (decision 51): `K3S_VERSION=v1.31.14+k3s1`, on 2026-11-10, `test` passed all 34
checks and `sanity` 75 of 75, with `csi-provisioner:v5.3.0` and `csi-resizer:v1.14.0`, which `k3s-csi.sh deploy` picks below
1.34 as `csi/README.md` says to, and the other sidecars as on 1.37. `make lint` renders the chart as 1.31, 1.33, 1.34 and
1.37, and checks each picks the right two (`test/chart-versions.sh`); with the older provisioner left out, 1.31 rendered v6.4.0,
which it reports, and 1.30 is refused.

**After the third review (decision 48) and the scale fixes (decision 49):** `test` passed all 34 checks, and `sanity` 75 of 75,
in the lab on the build host on 2026-11-09. A new check restores the snapshot as a ReadOnlyMany filesystem and mounts it on two nodes at
once: each pod reads the snapshot's data, sees `ro` though it didn't ask, and can't write. Before the fix, the staging mount
was read-write, so a pod could. Its first run failed on the test itself: the pod's command had double quotes, which `pod`
puts inside a double-quoted YAML string, so neither pod was made, and the pending restore then held the snapshot, so
"nor snapshots" failed too. Not run for these changes: `e2e.sh`, `vm-e2e.sh` and the browser suites, which they don't touch.

- **The first runs failed on the tests, not the driver,** and each failure was traced before anything changed. `k3s-lab.sh
  kubectl` passed its arguments through SSH, which split JSON and `sh -c` scripts on the server VM: a `kubectl exec` ran
  most of its script on the server, as an ordinary user, which looked like the pod being refused its device. It quotes them now.
- **A raw block volume shared by two nodes needs direct I/O.** Read through the page cache, the second node saw zeros that
  udev had read when the device appeared, though the first node's write had reached the volume. Read with `iflag=direct`,
  it saw the write. That's true of any shared block device, so it's in the README.
- **The test found one real leftover:** both agents kept a device from the shared volume after it was deleted, as nothing
  disconnected them from its subsystem, and the kernel was retrying it. A node now disconnects from a shared volume's
  subsystem when it unstages it. The unit test failed first; then the lab passed all 25, and `remove` left no subsystem,
  device or mount on any node.
- **A claim's size changes only after its filesystem grows,** when a pod next mounts it. The test checks the volume, then the
  filesystem, then the claim, in that order.
- **On SPDK too:** with `BACKEND=spdk` and the SPDK image, all 25 passed, against SPDK v26.09 in interrupt mode.
- **An outage, and a node rebooting:** a pod writing a line a second kept going after nvweaver stopped for 90 s, which is
  longer than several reconnect attempts. Its writes waited, then carried on, with no restart and its filesystem still
  writable. Then agent1 rebooted: the pod came back on its volume, carrying on from where it was, with every earlier line.
  The suite has 30 checks now, and passed them all on the kernel target.
- **`csi-sanity` v5.6.0 passes, 75 of 75,** with 27 skipped for capabilities the driver doesn't claim. `k3s-csi.sh sanity`
  builds it at that tag and runs it against the driver on agent1, as a cluster of its own. Its first run failed 97, all from
  one leftover mount: the driver let a SINGLE_NODE_SINGLE_WRITER volume be published at a second path, which CSI forbids,
  and the test never cleaned up after a publish it expected to fail. Then two more: a node ID that isn't an NQN has to be
  not found, and the driver claimed `PUBLISH_READONLY`, which nvweaver can't do. Each was fixed, with a unit test first.
- **Scoped tokens, decision 46:** the lab's driver now uses a scoped token, `lab`, made by `nvweaver token new` in the image,
  and the service has a second, `other`. All 32 checks passed: every volume the driver made is owned by `lab`, and `other` is
  refused with `403` when it tries to delete one. `csi-sanity` still passes, 75 of 75. In the unit tests, every rule failed
  its test when turned off: without the ownership check, one cluster could delete another's subsystem; with every route open,
  a scoped token could adopt a pool; without the forced owner, what a token made had none. The browser test checks the Owner
  column and filtering by it, 245 of 245. Scoped by decision 39, `e2e.sh` passed on both targets, 311 and 327, and the
  browser against real targets 93 of 93, unchanged by the new checks on every request.
- **OpenShift's nodes share a host NQN.** A fresh compact OpenShift 4.22 cluster, three nodes on vSphere, had the same
  `/etc/nvme/hostnqn` and `/etc/nvme/hostid` on every node, from the image, which would have put every node in one subsystem.
  Not worked around in nvweaver, as decided: the MachineConfig in `csi/deploy/openshift/`, applied to the `master` pool, as the
  cluster is compact and the `worker` pool has no machines, runs `nvme gen-hostnqn` at each boot. That was checked first: it
  gives the same NQN twice on one node, from the DMI product UUID, and a different one on each node. Applied during an upgrade
  to 4.22.22: one node rebooted twice, and two went straight to the combined configuration. Afterwards every node's NQN and host
  ID were its own, matched each other and the node's product UUID, and the unit had exited 0. A reboot with it in place, to show
  the NQN stays the same, hasn't been done separately.
- **On OpenShift 4.22.22,** a compact three-node cluster on vSphere, with nvweaver on a separate Fedora 44 VM on the kernel
  target, an LVM thin pool on a 250 GB disk, over a routed 1 Gb network, and the driver on a scoped token from the lab
  registry. The node plugin ran with the privileged SCC, as the guide says, and each node registered with its own NQN once the
  MachineConfig was applied. The k3s lab's checks, run as restricted, non-root pods, passed 13 of 13: a filesystem written on
  one node and read on another, growing it on a third, a snapshot and a clone, and a raw block volume shared by two nodes.
- **KubeVirt, with the StorageProfile in `csi/deploy/openshift/`:** CDI took it as written, and imported a CirrOS disk onto a
  ReadWriteMany raw block volume without being asked for one. The VM ran, live-migratable, and migrated between nodes in 3 s,
  staying Running and Ready. **The new node's I/O then stalled for 10 s:** when the old node was removed from the volume's
  subsystem, the kernel target dropped the new node's connection too, as decision 30 says it does, and the node queued I/O
  until it reconnected, 10 s later, its reconnect delay. The VM never saw an error, as nodes retry for ever. A shorter
  reconnect delay would shorten this, and hasn't been measured. Afterwards, nothing was left in nvweaver but the three nodes'
  subsystems, which are kept, and no node had a device left.
- **On a dedicated storage network, from a multi-homed nvweaver.** The nvweaver VM then had two interfaces: the routed one at MTU
  1500, carrying the API and port 1, and a non-routed storage VLAN at MTU 9000, carrying port 2, which only the OpenShift nodes
  could reach, each through an extra interface at 9000. With the StorageClass on port 2, all 13 checks passed again, the VM
  live-migrated in 3 s, and every NVMe connection ran between the two VLAN addresses, with a 9000-byte path MTU and 8948-byte
  segments. 64 MiB of random data written with direct I/O on one node read back identically on another. Moving the cluster to
  port 2 meant recreating the StorageClass, which reset CDI's StorageProfile, and clearing the nodes' old subsystems, as a node
  keeps using a live connection: both are in `csi/README.md`.
- **nvweaver installed as a service, and its host rebooted.** In a container started with `podman run --restart=always`, nvweaver
  didn't come back when its VM rebooted: Podman doesn't restart those at boot without its own restart service. Installed from
  `make package` with the systemd unit, it did: rebooted with a KubeVirt VM running on one of its volumes, it was back in 22 s
  with both ports and every volume, the node reconnected over the storage VLAN on its second attempt, and the VM stayed Running,
  never paused.
- **The stall after a live migration, against the reconnect delay.** Two pods shared a raw block volume on two nodes; one
  wrote with direct I/O every 100 ms, timing each write from `/proc/uptime`, and the other's pod was deleted, which takes its
  node off the volume's subsystem, as a migration's end does. The longest gap between writes was 10.17 s, 2.19 s and 1.11 s at
  a reconnect delay of 10, 2 and 1 s, matching the kernel's log, and no write failed. The default is 2 s now: decision 45.
- **A DataImportCron, and VMs from its golden image.** CDI imported Fedora's latest container disk onto nvweaver and kept it as
  a 40 GiB VolumeSnapshot, as the StorageProfile asks, under a DataSource. **The first VM from it went to the wrong storage:**
  its disk named no class, and with no default class for KubeVirt, CDI used the cluster's, vSphere's, copying 40 GiB from the
  snapshot for 6 minutes onto a ReadWriteOnce disk. With `storageclass.kubevirt.io/is-default-virt-class` on nvweaver's class,
  as the guide now says, the next VM was running 41 s after it was made, on a ReadWriteMany raw block disk, live-migratable,
  and the driver's CreateVolume from the snapshot took 159 ms: a thin clone.
  **Changing the default class had a side effect:** seconds later, CDI deleted OpenShift's built-in golden images, three claims
  on vSphere storage, and imported them again onto nvweaver as snapshots, as the API server's audit log showed. Nothing was lost,
  but the claims were gone. The guide warns of it now.
- **Metrics, decision 47.** On the nvweaver VM, with the OpenShift cluster's volumes, `/metrics` refused a request with no token
  (401) and answered one with a `monitoring` scoped token made as `docs/monitoring.md` says: 55 lines, with the LVM pool's data
  and metadata use, object counts and no warnings. Prometheus's `promtool check metrics` found no problems in it. The unit tests
  check the names, values and format, and failed with request recording turned off, and with label escaping turned off.
- **Found on the way, outside nvweaver:** the nvweaver VM's interface was set to an MTU of 9000 on a network that passes only
  1500. NVMe/TCP to the cluster worked, as TCP settled on the nodes' 1500, but pulling an image from the lab registry stalled,
  as its storage host also used 9000.
- **Not tested yet:** more than three nodes or a handful of volumes, a DataImportCron, a guest writing during a migration, Arm, and the
  outage test against SPDK, or for longer than the kernel's default 10-minute timeout. `e2e.sh` doesn't run with a tokens
  file: the k3s lab is the only real service that has.

### The CSI driver on Arm

On 2026-11-10, on the user's single-node OpenShift 5.0.1 cluster on arm64 (Kubernetes 1.36.5, RHCOS 9.8), installed with the Helm
chart, `snapshotMetadata` on, its own `nvweaver` class beside the cluster's default, against nvweaver-1 over the routed network.
The image was CI's arm64 build of PR 20, from the arm job's artifact. As restricted pods, one node, so "another node" is the
next pod after the first has gone: writing, detaching, attaching again and reading; growing the volume and its filesystem; a
snapshot and a clone with its data; ReadOnlyMany; raw block shared by two pods; and changed block tracking, with the sidecar's
example client built on arm64, told of both 4 KiB writes, 128 KiB in all, and the first snapshot's 4 MiB. Then everything
taken away, with nothing left in the cluster or in nvweaver but the node's own subsystem.

- **23 of 25: the two ReadOnlyMany checks failed,** as the user noticed while it ran: `relabel failed ... lsetxattr`. On SELinux
  nodes the runtime relabels a volume for each pod, and decision 48 had made ReadOnlyMany filesystems read-only, so it
  couldn't, and the pods never started. The k3s lab missed it, as its runtime doesn't relabel. Decision 53 mounts them with
  one label; a unit test failed on the code before it.
- **25 of 25 with the fix,** in an arm64 image built from the fix by emulation on the dev VM.
- `nvme-tcp` wasn't loaded on the node; `csi/deploy/openshift/99-master-nvme-tcp.yaml` loads it, and was applied with a reboot.
- Not yet: a KubeVirt VM on the class, and the end-to-end suites in native Arm VMs.

**The same checks on x86-64 OpenShift,** the user's three-node 4.22 cluster (Kubernetes 1.35.9), with the next pod on another node
each time: **25 of 25**, ReadOnlyMany and changed block tracking included, the first time CBT ran on OpenShift, on an amd64
build of the same commit. Three things on the way:

- **`helm upgrade --reuse-values` from the v0.4.0 chart failed,** as it reuses the old chart's defaults too, which have no
  `snapshotMetadata`. `--reset-then-reuse-values` worked, keeping the release's own values; `csi/README.md` says so now.
- **The cluster's class had stopped making volumes the day before,** with "there are 2 pools, so pool is required": its class
  names no pool, and a second pool had been adopted on its nvweaver for the scale tests. That pool was forgotten again;
  `csi/README.md` and the chart's values say to name the pool even when there's one.
- **x86 k3s, for decision 39:** on 1.37, `test`, csi-sanity 75 of 75 and `cbt`; on 1.31, `test` and csi-sanity 75 of 75.

### The Helm chart

On the OpenShift cluster, the manifest-installed driver was removed, keeping its namespace and API secret, and the chart
installed in its place with the existing secret, the class on the storage network's port and KubeVirt's default set. It detected
OpenShift and granted the node plugin the privileged SCC by itself, made the snapshot class as the CRDs were there, and the
driver came up. The 13 checks passed, as restricted pods, and a VM from the Fedora golden image was running in 40 s on a
ReadWriteMany raw block disk. `helm upgrade` changed the reconnect delay on every node, and back. Rendering refuses a missing
or badly formed `cluster`, and a missing API, with messages saying which. `make lint` lints it with `--strict` and renders it as
on plain Kubernetes and as on OpenShift. Not tested yet: the chart on k3s, and installing it on a cluster that never had the
driver.

### Changed block tracking

`test/vm/cbt-check.sh` checks, before anything is built on them, whether the ways nvweaver would report a snapshot's changed
and allocated blocks ever miss one, as a missed block silently corrupts every incremental backup built on it. It runs
`cbt-check.py` as root in a fresh lab VM, on loop devices: Fedora 44, kernel 7.2.15, LVM 2.03.38,
`device-mapper-persistent-data` 1.3.4, xfsprogs 7.1.1.

A 256 MiB volume goes through seven rounds, with a snapshot after each, taken as nvweaver takes them: half written with
random data; single bytes at chunk edges and the last byte, a write across a chunk boundary, one unaligned at both ends over
several chunks, and one from written into never-written space; 1,000 random 512-byte writes; TRIM of whole chunks, of parts
of chunks, and of never-written space; the same data written again, and zeros into never-written space; nothing; and a full
overwrite. Nine pairs of snapshots are compared, consecutive and further apart, and three snapshots' allocated ranges.

| Method | Cases | Result |
|---|---|---|
| LVM thin: `thin_delta` and `thin_dump` on a reserved metadata snapshot of the live pool | 64 KiB chunks with zeroing on and off, 128 KiB, 256 KiB, 1 MiB | No byte unreported |
| Reflink on XFS: FIEMAP of two `cp --reflink` snapshots, where a different physical extent, or written on one side only, is a change | 4 KiB blocks | No byte unreported |

**72 checks and 66 negative controls passed.** Each check reads both snapshots and fails if any byte that differs, or any
non-zero byte for allocated ranges, is outside what was reported. So that it can't pass by mistake, each check with anything
to find runs again without the reported range that holds the first such byte, and must then fail. The six pairs with nothing
changed have no control.

- **Over-reporting grows with the chunk size,** as LVM reports whole chunks: 1,000 scattered 512-byte writes, 0.5 MB of
  change, came back as 55 MB at 64 KiB chunks and 248 MB, nearly the whole volume, at 1 MiB. Reflink reported 3.9 MB.
- **TRIM:** dm-thin unmaps only whole chunks, and reports those. A partial TRIM leaves the chunk as it was, and nothing is
  reported, which is right, as nothing changed.
- **The "same data again" round did change 509 to 2,550 bytes:** it read the data back through the page cache, where the
  earlier unaligned TRIM had zeroed the partial page while dm-thin kept the chunk. Writing that back made the zeros real. They
  were reported. Worth knowing for anything that reads a block device after an unaligned discard.

**Through the API, end to end:** `vm-e2e.sh` checks nvweaver's own implementation, in its container, through `nsenter` to
the VM's LVM, on both targets and both pool types. A host writes files to an XFS filesystem on a volume, and `filefrag`
gives where each file's data is on the volume. `changes` must hold the rewritten file and every file written after the base
snapshot (those not on a clone of it), and none of the 31 unchanged 8 MiB files in full; `changes` across a snapshot taken
under load must hold every file whose fsync finished before it; and `allocated` must hold every file a snapshot has. All 20
checks passed. Its first version bounded the reported size at 64 MiB, which failed on SPDK with reflink at 83 MB: files the
writer finished just after the snapshot really had changed, so the bound was a guess. It was replaced by the exact checks
above. The helper that compares ranges was tested by hand first, and an awk array indexed from an unset variable was found
to hide the first range.

**Through the CSI driver:** `k3s-csi.sh cbt`, on k3s 1.37 on 2026-11-10, passed 13 of 13. It installs the driver with the Helm
chart and `snapshotMetadata.enabled`, which also tests the chart in the lab, and the sidecar's CRD from v1.1.0. A pod writes
4 MiB to a raw block volume, which is snapshotted; then 4 KiB at 10 MiB and 30 MiB, and it's snapshotted again. The
sidecar's own example client, `snapshot-metadata-lister`, asking through the sidecar with a Kubernetes token, was told of both
writes and 128 KiB in all, two 64 KiB chunks, and of the first snapshot's 4 MiB, and an account with no access to the
snapshots was refused as not allowed. Its first runs failed on the test, each traced: the CRD's path in the release is a
symlink, which a raw download gives as its target's name; the chart set `requireBaseSnapshotForDelta`, which v1.1.0's CRD
doesn't have; the lister at the release's commit still used the alpha API, so it's built from the commit that moved it to
v1beta1; and the refusal check passed on any error, which hid a missing option, so it now wants `PermissionDenied`.

**The end-to-end check failed in CI three times, each time on the check, never on what CBT reported,** which took four
rounds of fixes to show. Each wrong turn is worth knowing:

1. It treated a file missing from a clone of the base snapshot as new data. A file being written when a snapshot is taken
   can have its data in it but not its name, which is what a crash-consistent snapshot holds; its blocks hadn't changed.
   Timing decided whether it happened, so it passed on the build host and failed in CI.
2. It then compared what wasn't reported between the live original and that clone. The original isn't a moment, and the
   clone had been mounted, so neither stood for a snapshot. Now both snapshots are read where they're stored on the target,
   LVM snapshots activated read-only, reflink snapshots as their files, with no host or clone in between.
3. Its byte comparison read 512 bytes at a time with direct I/O: through SPDK in interrupt mode, at about 35 ms a request,
   an 8 MiB extent took 10 minutes, which looked like a hang. Its reads were in uninterruptible sleep, and SPDK's reactor in
   `epoll_wait`, which suggested a lost wakeup in SPDK; one more read, which finished at once, showed the "stuck" reads were
   new ones. It reads 1 MiB at a time now.
4. It asked whether one reported range held a whole extent of a file. On SPDK with reflink, about one round in three, the
   file being written was half there in the base snapshot and a hole after; CBT rightly reported only the half that
   changed, and the check, comparing the whole extent, called the rest a miss. Asked again, the API gave the same ranges,
   which ruled out timing. Now only the parts of an extent that no range holds are compared, and only a difference fails.
   `TestReflinkChangesAcrossManyExtents` builds the same layout, in about 1,700 extents, over several FIEMAP calls.

With all four, `SNAPSHOT_ONLY="spdk reflink 6"` passed 384 of 384, meeting the half-written file in four rounds of six, its
unreported half the same in both snapshots each time. The client also syncs before the last snapshot now, as a guest agent
or backup hook would; the one before it is still taken mid-write, unsynced. Also from these runs: the power-loss tests leave
their background `lab.sh ssh` loops running on the build host, on connections to a VM that was reset, until killed.

**Not checked:** a pool whose metadata is changing while `thin_delta` reads it (the metadata snapshot holds what was
committed when it was reserved, and nvweaver's snapshots are committed when taken), btrfs, Arm, SPDK's own snapshots, which
nvweaver doesn't use, and real disks.

**The fourth outside review, decision 55.** Unit tests only, each failing before its fix:

- `TestAThinIDReusedMeanwhileIsntRead` makes an LV go after its ID was read, with another taking ID 7. Before the fix, the old
  code returned the other device's ranges, empty here, with no error, for both `Changed` cases.
- `TestSnapshotsBeingReadArentDeleted` deleted a snapshot mid-read with a 204 before the fix; now it's a 503.
- `TestThinToolsOutputNotUnderstoodIsAnError` covers 14 kinds of bad output. A scratch run of the old parser over two of
  them showed the misses: it skipped an unknown `<moved>` and returned the rest, and it read `thin_dump`'s empty superblock
  for a missing device as nothing allocated.
- The new parser was also run over 1.3.4's real output, made with `thin_restore` on files. The metadata had a subtree shared
  between two devices, as snapshots have. `thin_dump --dev-id` expanded it, with no `<def>` or `<ref>`; without `--dev-id` it
  printed both. `thin_delta` printed `same` and `left_only`, and an empty `<diff>` for two empty devices. All were read
  correctly, and the missing device was an error.

`cbt-check.sh` wasn't run again, as the reviewer agreed it can't make a reused ID or a bad tool answer. `run-all.sh`, with
these and decision 54's changes, on 2026-11-10: vm-e2e 949, webui-real 93, kernel 311, spdk 327, systemd 76, all passed,
none failed, and nothing was left behind.

### Nodes leaving the cluster, and hosts' labels

Decision 54, on 2026-11-10. `k3s-csi.sh test` runs the janitor every 15 seconds, and checks:

- agent1's host is owned by the cluster's token, labelled `csi-cluster=lab` and `csi-node=agent1`;
- agent1 keeps its subsystem through its reboot, which takes longer than two of the janitor's checks;
- once agent2 is drained, its k3s stopped and its node deleted, its empty subsystem and its host go, while agent1's stay;
- agent2 joins again, and a pod there gets a new volume, with its host made again as the cluster's.

It passed on k3s 1.37.2 and 1.31.14: 44 checks each, and csi-sanity 75 passed, 0 failed, on both.

The unit tests cover what the lab doesn't:

- a departed node with volumes still exported is kept until they're unpublished;
- another cluster's subsystems are never touched;
- the janitor does nothing when the CSINode list fails or comes back empty;
- a host it doesn't own is reported and left;
- a scoped token can change and delete only its own hosts, never their keys.

`TestHostsAreOwned` failed before the API change.

**LVM snapshots showed as `missing` on a lab server** because they're inactive and have no device node. `TestInactiveLVMSnapshotsArentMissing` checks that LVM's list decides, and that 102 states cost at most two `lvs`. Before the change, the old check called such a snapshot missing.

**The web interface:** the browser test checks the overview's Problems section, and a CSI node's host showing its cluster, node and owner. It passed with 247 checks, and the screenshots were looked at.

**Not checked:** what a departed node's connection does. Stopping k3s doesn't disconnect the node, so agent2 was most likely
still connected when its subsystem went. With `--ctrl-loss-tmo` at -1, it would retry for ever, and reconnect once it joined
again; the test didn't look. Not tested on OpenShift yet.

### Pausing hosts' I/O for a consistency group

The experiments behind decision 56, on 2026-11-10, in the lab VMs. Raw configfs and raw SPDK RPCs were used, not nvweaver.
The target had four namespaces: two XFS reflink files and two LVM thin LVs. The initiator wrote 4 KiB with `O_DIRECT` to each
in turn and logged every write. The target moved some namespaces into a second ANA group, set its state, took snapshots,
held, and moved them back.

| Target, state, hold | Host | Longest pause | `dd` errors |
|---|---|---|---|
| kernel, `change`, 3 s, all four | multipath | 3.05 s | none |
| kernel, `change`, 15 s, past the 10 s ANATT | multipath | 15.03 s | none, and no controller reset |
| kernel, `inaccessible`, 15 s | multipath | 15.03 s | none |
| kernel, `change`, as short as possible, all four | multipath | 0.18 s, 4 snapshots in 154 ms | none |
| kernel, `change`, 3 s, the two LVs | multipath | 3.04 s | none |
| kernel, `change`, 3 s | **no multipath** | none: writes failed at once | I/O errors |
| SPDK 26.09, `inaccessible`, 3 s, all four | multipath | 3.2 s | none |
| SPDK 26.09, `inaccessible`, as short as possible | multipath | 0.36 s, 4 snapshots in 136 ms | none |

- With native multipath, the host logs "no usable path - requeuing I/O" and waits. Udev's reads on the hidden path devices
  (`nvme0c0nN`) log I/O errors, which multipath retries; nothing reaches the application.
- Moving a namespace between ANA groups works while it's enabled, on both targets. SPDK takes 8 ms per namespace, as it
  pauses the namespace to move it. A new kernel ANA group starts inaccessible, so set its state before moving anything in.
- An LVM thin snapshot took about 65 ms, and a reflink copy of a 1 GiB file about 5 ms.
- **Not proven by these:** that the snapshots are consistent with each other. That needs dependent writes across volumes,
  checked in the snapshots, and is planned for `vm-e2e.sh`.
- **The writer's status column was wrong** in the first three rounds: it logged `$?` after `$(date)`, the pitfall below.
  The `dd` error output was the evidence, and the fourth round logged `dd`'s own status.

### A KubeVirt VM snapshot, with its guest agent's freeze

On `nvw`, OpenShift 4.22.22, on 2026-11-11: a Fedora VM, its root disk a thin clone of the golden image, with two 1 GiB data
disks, all on nvweaver. In the guest, a service wrote a counter to a file on the first data disk, synced it, then the same to
the second, as `e2e.sh`'s check does. A `VirtualMachineSnapshot` of the running VM succeeded, with `GuestAgent` among its
indications: it froze the filesystems, then took a VolumeSnapshot of each disk. The VM was stopped and restored in place,
and on boot the disks held 391 and 390, consistent, and the writer carried on. One run, and with a freeze, consistency is
expected whatever the storage does: this shows that the pieces work together on nvweaver.

- **The first two snapshots failed,** "failed to open /d2: Permission denied" from `guest-fsfreeze-freeze`, and the snapshot
  timed out. The confined guest agent may open only mount points with a mount point's label, and a filesystem made in
  the guest has an unlabelled root. Mounting under `/mnt` wasn't enough; `restorecon -R` on the mounted filesystems was.
  `csi/README.md` says so.
- **The VM's root disk first waited for good.** Recreating the class on 2026-11-10 had reset its StorageProfile, so CDI
  had turned the golden images back into claims, and deleted their snapshots. Applying the profile again brought them
  back, but the waiting disk didn't retry: the VM had to be made again.
- **Group snapshots couldn't be tried there.** OpenShift 4.22's `VolumeGroupSnapshot` feature gate is off in the Default
  feature set, and the group snapshot CRDs aren't installed. Turning it on means `TechPreviewNoUpgrade`, which can't be
  undone and stops the cluster upgrading, so it wasn't.

### Group snapshots on OpenShift 5.0, on Arm

On `snoarm`, OpenShift 5.0.1 on arm64, on 2026-11-11, through OpenShift's own snapshot controller, which has group
snapshots on by default, with `v1` the only API version served. The driver's arm64 image was built from `main` at
`9c406a7`. As in the k3s lab, a pod wrote a counter to two block volumes in turn; a VolumeGroupSnapshot of both was ready
while it wrote, none of its writes failed, and the members, restored, held 690 and 689. The node's Lease said it could be
paused. Nothing was left afterwards.

- **The first try used `v1beta2`,** which 5.0 doesn't serve, and the test script hid `kubectl apply`'s refusal, then waited
  for a group snapshot that didn't exist. The script now shows a refused `apply`, and stops waiting at the first sign of
  failure: an error on the group snapshot or its content, or the driver refusing it.
- **The second got a 404 from nvweaver,** which hadn't been upgraded, and has no snapshot groups. The driver took that for
  a group that wasn't there. It now checks nvweaver's `snapshot_groups` capability, and refuses, saying to upgrade.
- nvweaver-1 was upgraded to `v0.4.0-19-g9c406a7` for this, which restarted it; `nvw`'s volumes and driver carried on, with
  no failed calls.
- **Hosts made before hosts had owners** were then given to `nvw`'s and `snoarm`'s tokens, the exact four from their CSINodes.
  The janitors labelled three at once, and the fourth, `nvw`'s master-1, at their next check, five minutes later: its node
  plugin was restarting with the driver's upgrade, so the first check didn't find it, which is the miss the janitor allows
  for. `nvw`'s driver was upgraded to an image of `main` at `9c406a7` for its janitor, without group snapshots; the
  StorageClass, and so its StorageProfile, were kept.

### A Synology's DSM, for SPDK first

A first, read-only probe on 2026-11-11, as an ordinary user, of a Virtual DSM: DSM 7.4.1 in Synology's Virtual Machine
Manager, which runs DSM's own kernel. A real Synology's model may differ, Arm ones especially.

- **Linux 4.4.302, with no `nvme_core`, `nvmet` or `nvmet-tcp` at all,** which arrived in Linux 5.0. So on DSM, the kernel
  target is out, and SPDK is the only way to serve NVMe/TCP. DSM's own iSCSI target, LIO, is loaded.
- **No `dm_thin_pool`,** so no LVM thin pools: reflink pools only.
- **Reflink works** on its btrfs volumes, `cp --reflink=always` in a home directory.
- Container Manager's Docker is 24.0.2, and its socket is root's only. AppArmor, not SELinux. `aio-max-nr` is 65536, and a
  user's memory-lock limit 64 KiB, which matters if SPDK locks memory in a container.

**Then nvweaver itself, in Container Manager's Docker, with no privileges,** as the SPDK Quadlet already runs it: host
networking, every capability dropped, `no-new-privileges`, and its data in a folder on a btrfs volume. No `--privileged`,
no devices, no `--pid=host`. The image is 102 MB.
- It started, and SPDK 26.09 served, in 2 seconds, without hugepages, in interrupt mode, on DSM's kernel 4.4.
- A reflink pool on the btrfs volume, a 1 GiB volume, a snapshot and a clone of it, and an export on the routed
  network, all worked.
- A host on another machine, the lab's initiator VM, connected over NVMe/TCP, and wrote 64 MiB that read back the
  same, and was the same in the volume's file on btrfs. The snapshot taken before held only zeros.
- A snapshot group of two volumes, taken while the host wrote: hosts paused 57 ms, and all 2,726 of its writes in
  20 s succeeded.

**Then `dist/synology/compose.yaml`, with `docker compose` on the DSM,** set up as a user would through File Station:
every file owned by that user, the TLS key, users file and `.env` mode 0600, the data folder made by them.
- With every capability dropped, it couldn't start: "users file /etc/nvweaver/users: permission denied". Root with no
  capabilities can't read another user's file.
- **Running as that user, `user: "1026:100"`, failed too:** it couldn't make anything in the data folder that user had
  made, "mkdir /var/lib/nvweaver/volumes: permission denied". Probably DSM's own ACLs on shared folders; not looked into.
- **With `DAC_READ_SEARCH` added back, it worked:** HTTPS from the user's certificate, signing in from their users file,
  the API token, a reflink pool in their folder, a volume, a snapshot and an export, all kept across a restart. That's the
  Compose file's one capability now.
- **The user followed the guide through Container Manager's Project screens** on the Virtual DSM, on 2026-11-11, and it
  worked. A users file with the wrong permissions, their own slip, was the only thing to fix.
- Not tried yet: a real Synology, an Arm model, and the storage network.

### OpenShift, through a second driver, to SPDK on a Synology's DSM, over the SAN

On `nvw`, OpenShift 4.22, on 2026-11-11: `dist/synology/compose.yaml` on the Virtual DSM, set up as its guide says, with the
API on the routed network, TLS, and a scoped token for `nvw`, and its NVMe/TCP port on the storage network. A second
instance of the CSI driver, with its own driver name, NQN prefix, liveness port and classes, beside the one for
nvweaver-1, which carried on undisturbed. Every node's Lease said it could be paused.
- A filesystem volume, written on master-0, connected from its SAN address, and read on master-1.
- A snapshot, and a clone of it read on master-2.
- A raw block volume, 16 MiB written directly and read back the same.
- Deleted, nothing was left in nvweaver.

**Found on the way:**
- **The chart had the node plugin's liveness port fixed at 9899,** on the host's network, so a second instance's would have
  failed to start. It's `node.livenessPort` now.
- **A second instance needs its own NQN prefix,** or each node's subsystem has the same name on both servers, which a host
  would take for two paths to one subsystem. `csi/README.md` says so.
- **The test script's own faults:** pods pinned with `nodeName`, which skips the scheduler, so a `WaitForFirstConsumer`
  claim never bound and nothing was provisioned; a `}` inside `${4:+...}`, which ended the expansion early; a Warning check
  across the whole namespace, which took one pod's events for another's; and "Waiting for detach", which is normal while a
  volume moves, taken for a failure. The waits did stop at the first sign of trouble, as they should.

### Snapshot groups

Decision 56, on 2026-11-10. `e2e.sh` checks that a group's snapshots are consistent with each other, on a reflink pool,
on both targets. The host writes a counter to two volumes in turn with `O_DIRECT`, each write only once the one before it
has finished, so at any instant the first holds the second's number or one more. The check reads the numbers at the start
of each snapshot's file, on the target.

| Target | Five groups, while the host wrote | Hosts paused | Snapshots a second apart, the negative control | Host's writes |
|---|---|---|---|---|
| kernel | 500/499, 840/839, 1184/1184, 1525/1524, 1892/1892 | 3 to 4 ms | 2241 and 2501: caught | none failed |
| SPDK 26.09 | 365/365, 593/593, 832/832, 1062/1062, 1300/1300 | 55 to 68 ms | 1518 and 1703: caught | none failed |

kernel: 348 passed, SPDK: 364 passed, none failed, and nothing was left in the VM. SPDK pauses longer as it pauses each
namespace to move it between groups, twice, which the kernel doesn't.

The first run of this check failed all nine of its checks on both targets, the negative control too, so it proved
nothing. Its 512-byte direct writes were refused by the 4 KiB namespaces, so the snapshots held nothing, and `dd`'s
statistics filled its error log. A negative control that can't fail is what showed that it wasn't testing anything.

Unit tests cover:
- both targets moving namespaces into and out of an inaccessible ANA group, in order, without disabling them;
- a pause ended by a restart, as it's never saved, and a port removed while it has a pause group;
- the API holding hosts paused for every snapshot, and not for volumes that aren't exported;
- undoing everything when a snapshot fails, or the pause passes its limit;
- one group at a time, the volumes held meanwhile, requests checked, and scoped tokens kept to their own;
- crashes at each point, and a delete that fails partway carrying on;
- the driver refusing a group that would pause a node that can't be paused, while a node using none of the volumes
  doesn't matter, and each node's Lease.

The browser test checks a group's snapshot showing its group, offering only to delete the group, and the dialog naming every
snapshot that goes with it, then the group gone: 250 passed, on 2026-11-11.

**Through Kubernetes, `k3s-csi.sh groups`,** on fresh labs on k3s 1.37.2 and 1.31.14, 14 checks each, none failed:
- every node's Lease said it could be paused;
- a VolumeGroupSnapshot of two block volumes was taken while a pod wrote the counter to both;
- the member snapshots, restored to new claims, held 1043 and 1042 on 1.37, and 1267 and 1266 on 1.31;
- with one node's Lease set to "can't be paused", a group of volumes it used was refused, naming it, and nvweaver
  wasn't asked;
- deleting the VolumeGroupSnapshot deleted the group and its snapshots in nvweaver.

The full CSI suite and csi-sanity passed on both, with group snapshots on in the driver.

**On the way, two things that weren't nvweaver's:**
- The snapshot controller that external-snapshotter 8.7.0's manifest runs, 8.6.0, has group snapshots off, though its
  README says on. A VolumeGroupSnapshot then sat there with no status and no error; see the pitfalls.
- The node-leaving test (decision 54) stopped k3s on the node and deleted it, which leaves its containers running. On
  one 1.37 lab, a snapshot controller was on that node, and kept its leader lease, unseen: a VolumeSnapshot's deletion
  then never started, and nothing more was provisioned. It passed on the other labs, where nothing that elects a leader
  was there. The test now drains the node first, as removing one would, and passed with a snapshot controller on it.
  Turning the group gate on was suspected first; the same snapshot deleted with it on, off and on again showed it
  wasn't.

**On LVM, `vm-e2e.sh`'s `group_tests`,** with the host in its own VM, three groups each, read through clones as LVM keeps
snapshots inactive. `GROUP_ONLY=nvmet` and `=spdk`, 36 passed each, none failed:

| Target | Three groups | Hosts paused | Negative control | Host's writes |
|---|---|---|---|---|
| kernel | 499/499, 845/845, 1196/1195 | 91 to 115 ms | 1551 and 1836: caught | none failed |
| SPDK 26.09 | 413/413, 684/684, 967/966 | 160 to 167 ms | 1251 and 1470: caught | none failed |

`run-all.sh` with all of this, on 2026-11-11: vm-e2e 1004, webui-real 93, kernel 348, spdk 364, systemd 76, all passed, none
failed, and nothing was left behind.

**Not tested yet:** groups on Arm, and on OpenShift; a group of more than two volumes with a host writing; and a host
without native multipath through the CSI driver, which only the unit tests and a Lease edited by hand cover.

### The CSI driver at scale

`csi/test/scale` drives the driver's controller, the code Kubernetes calls, against a real nvweaver, with 16 calls at once
as the sidecars' workers make them: 5,000 volumes of 1 GiB, published 50 each to 100 simulated nodes (their host NQNs; no
node connects), listed, 500 of them snapshotted, then all of it taken away. It ran from a separate VM, over TLS with a scoped
token, against a lab server: a 4-vCPU, 7 GB Fedora 44 VM with kernel 7.2.15 and SELinux enforcing, on the kernel target,
with an LVM thin pool on a virtual disk that's an iSCSI LUN, made as `docs/pools.md` says (16 MB metadata area, 1 GB pool
metadata). Every call in every run succeeded.

| Phase, 5,000 volumes | v0.4.0 | Decision 49 | And the driver reading first |
|---|---|---|---|
| Create | 1,069 s, 15 a second at first, 2.5 at the end | 1,080 s | 1,084 s |
| Publish | 1,225 s, 3.3 a second at the end | 291 s | **236 s** |
| List, 500 a page | 0.66 s | 0.67 s | 0.63 s |
| 500 snapshots, then deleting them | 215 s, 204 s | 216 s, 203 s | 216 s, 205 s |
| Unpublish | 278 s | 279 s | 279 s |
| Delete | 953 s | 956 s | 961 s |
| All of it | 3,944 s | 3,026 s | 2,984 s |

- **Exporting a volume looked up every other one.** The API's `checkUnshared` resolved and looked up each volume, snapshot
  and namespace's path on every export, under the lock: on LVM, a symlink walk, a `stat` and sysfs reads apiece. `strace`
  counted 313,000 `stat` calls in 10 seconds, and the service used a whole CPU while publishing. The earlier scale runs used
  sparse files, where this is cheap, so it didn't show. Decision 49 compares a volume only with what was exported by path.
- **Every publish made two creates that changed nothing.** The driver created the node's host and subsystem on every publish,
  and took the 409. Each still waited for the lock, about 200 ms at 16 at once. It reads first now. That gained less than
  expected, because the namespace's own change is now nearly all of a publish.
- **Each change costs about 50 ms at 5,000 volumes,** mostly rewriting the whole state with an fsync (`TestWhereTimeGoes`:
  clone 10 ms, validate 5, save 51). That's now the floor: about 15 to 20 changes a second. In `TODO.md`.
- **Creating, snapshotting and deleting are LVM's time.** A bare `lvcreate` took 0.7 to 0.8 s with 5,000 LVs in the volume
  group, and `lvs` of one LV 0.46 s; each waits for the volume group's lock, so throughput falls as the group grows.
- **Resources stayed small:** the service's memory between 30 and 62 MB, 5,000 LVs used about 1.6 MB of the 16 MB metadata
  area, and the thin pool's metadata reached 3.6% of 1 GB with the volumes empty.
- **A scoped token can't delete hosts,** which have no owner, so the harness leaves its 100 simulated hosts for an admin.
  The driver would have the same problem removing a node that's gone.

**Not measured:** real nodes connecting at this count, I/O, the sidecars and the API server themselves, SPDK, or a real disk.

### Many connections to one target

The same lab server, as its own initiator over its real address, with 4 vCPUs shared by both sides: 1,000 controllers to one
subsystem on the kernel target, each as a host of its own (its own host NQN and host ID) with 4 I/O queues, so about 5,000
TCP connections. Every connect succeeded.

| Controllers | `nvme connect`, median | Memory available | Slab | Service's memory | `GET /v1/connections` |
|---|---|---|---|---|---|
| 25 | 19 ms | 5.3 GB | 406 MB | 24 MB | 6 ms |
| 200 | 106 ms | 5.0 GB | 622 MB | 22 MB | 18 ms |
| 500 | 271 ms | 4.3 GB | 996 MB | 25 MB | 27 ms |
| 1,000 | 516 ms | 3.3 GB | 1.6 GB | 28 MB | 53 ms |

- **About 2 MB of memory per controller,** both sides together, 1.25 MB of it kernel slab, all given back after disconnecting.
  The target's own share can't be told apart here, as the initiator is on the same machine.
- **The growing connect time is nvme-cli's, not the target's.** At 310 controllers, writing the connect straight to
  `/dev/nvme-fabrics` took 8 to 10 ms, and `nvme connect` 180 to 270 ms: it reads every controller already on the host
  first. A real node has a few controllers, not hundreds.
- **Disconnecting 1,000 took 27.5 s,** by `nvme disconnect -n`, one at a time.
- **The service doesn't notice:** its memory stayed under 30 MB, and listing every connection took 53 ms at 1,000.

**Not measured:** connections from many machines, I/O on them, SPDK, or what a target with less memory does when it runs out.

### v0.4.0-rc1, as published

The release run passed every job, and published a pre-release. Its 14 files matched `SHA256SUMS`, and each had a valid
attestation from the tag, as did all three images. Each image was multi-arch, amd64 and arm64, and pulled with no credentials,
the driver's once its new package was made public; each reported `0.4.0-rc1`, and `latest` stayed on 0.3.0, as a pre-release
leaves it. Every SBOM said `go1.26.10`, and the driver's listed its seven modules on both architectures, but gave its own
module, `nvweaver/csi`, the version `UNKNOWN`, as `make-sboms.sh` stamped only `nvweaver`'s: fixed, and checked, for v0.4.0. The
OpenShift cluster's Helm release was moved to the published driver image, and the 13 checks passed.

### Before v0.4.0

Every suite on x86-64 VMs, with images built from the release branch: unit 347, browser 245, kernel `e2e.sh` 311, SPDK `e2e.sh`
327, systemd 76, `vm-e2e.sh` 915, and the browser against real targets 93, all passing. Then the driver in the k3s lab with those
images, 32 of 32 including the outage and reboot, and `csi-sanity`, 75 of 75. `make-sboms.sh` made the driver image's SBOM for the
first time and found every module the notices list for it, and the Go runtime. Its last check, that everything was built with one
Go, failed there, as the tarball was built with Fedora's Go, which reports `go1.26.10-X:nodwarf5`, while the images used the Go
container's `go1.26.10`. That's the check doing its job: the release builds the tarball with `setup-go` in CI, as for v0.3.0.

### Before v0.5.0

On 2026-11-11, with images built with Go 1.26.11 from the release branch, on x86-64 VMs:
- **The upgrade from v0.4.0, `test/vm/upgrade.sh`, new for this release,** 68 of 68 on each run, on both backends. The published
  0.4.0 images made a port, a host, a subsystem, a reflink and an LVM pool, a volume, a snapshot and a clone, and a file
  namespace, and a host wrote to the volumes. This version started on that data with no errors or warnings, kept every
  object as it was, the host read its data back the same, and a snapshot group of both pools' volumes worked on it. Its
  first version could have passed with nothing recorded, as an error both times compares equal: it now checks that what it
  recorded holds every object.
- `run-all.sh`: `vm-e2e.sh` 1004, the browser against real targets 93, kernel `e2e.sh` 348, SPDK `e2e.sh` 364, systemd 76,
  all passing.
- The k3s lab on 1.37.2: `k3s-csi.sh test` 44, `groups` 14, `cbt` 13, and `csi-sanity` 75 of 75; on 1.31.14, `test` 44,
  `groups` 14 and `csi-sanity` 75. All passing, and nothing was left behind.

**CI's end-to-end job couldn't install QEMU** from about 14:00 UTC that day: `azure.archive.ubuntu.com`, the runners' first
Ubuntu mirror, didn't resolve, nor from the user's own network, while other jobs' installs worked. It's a CNAME to an Azure
Traffic Manager profile, which answers by region. `.github/apt-install.sh` now falls back to `archive.ubuntu.com` when it
doesn't resolve. PR 24's CI and the v0.5.0 release run both passed with it, but the mirror resolved again by then, so
neither took the fallback: it has never run for real (see "Not tested").

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
- `.github/apt-install.sh`'s fallback to `archive.ubuntu.com`, as the runners' mirror resolved again before it ran

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

### SPDK on Synologies, against DSM's own iSCSI (2026-11-11 and 12)

`test/bench-classes.sh` from an Arm OpenShift node over a 10 Gb storage network with jumbo frames, to nvweaver 0.5.0 (SPDK, a
reflink pool, `dist/synology/`) and to DSM's iSCSI (Synology's CSI driver) on the same box, while `test/synology-capture.sh`
recorded the box. 16 GiB raw block volumes, filled first; seven fio jobs, 60 s each, three rounds alternating the classes;
medians. Two boxes, both DSM 7.4.1 on Linux 4.4: a hard-disk one (8 disks in RAID 10, 32 GB, later with an NVMe read-write
cache) and an all-flash one (11 SATA SSDs in RAID F1, 32 GB), and a Virtual DSM on the all-flash one. Eight runs in all.

| 4K, QD16×4 unless said | HDD: iSCSI | HDD: nvweaver as shipped | Flash: iSCSI | Flash: as shipped | Flash: sync attribute off |
|---|---|---|---|---|---|
| Random read, QD1 (IOPS) | 6,448 | 84 | 4,502 | 955 | 949 |
| Random write, QD1 (IOPS) | 6,182 | 19 | 6,205 | 358 | 1,196 |
| Random read (IOPS) | 46,088 | 1,377 | 42,584 | 61,060 | 58,100 |
| Random write (IOPS) | 26,791 | 78 | 36,671 | 14,452 | 38,985 |
| 70/30 (IOPS) | 40,580 | 221 | 38,711 | 30,495 | 46,084 |
| 128K sequential read (MB/s) | 555 | 199 | 638 | 1,028 | 1,041 |
| 128K sequential write (MB/s) | 330 | 19 | 720 | 320 | 722 |

What made the differences, each isolated by a run that changed one thing:

- **DSM's iSCSI reads come from RAM.** On the hard-disk box, 8,400 reads a second at QD1 caused 31 on the disks; nvweaver,
  with SPDK's `O_DIRECT`, reaches the disks one for one. Most of the hard-disk gap on reads is that.
- **The sync attribute (decision 28) costs most of the write gap,** and more than a flush: on btrfs over hard disks, each
  synchronous 4K write caused 17-89 writes on the disks (btrfs's log and tree commits, doubled by RAID 10), against 1.2-5
  without it. Clearing it: sequential write 18 → 269 MB/s, QD1 writes 24 → 963 IOPS on hard disks; on flash, deep random
  writes 14,452 → 38,985 and sequential writes 320 → 722 MB/s. **On flash, with it cleared, nvweaver matched or beat iSCSI
  on every job at depth and on sequential I/O** (sequential reads at the 10 Gb link's limit). See decision 28.
- **Copy-on-write pool files fragment under random writes:** sequential reads 199 → 415 MB/s with the pool folder set
  `chattr +C` before its files were made. DSM makes its own LUN files that way. Reflink still works between such files
  (checked on a Virtual DSM, Linux 4.4).
- **An SSD read-write cache** (hard-disk box) made the forced flushes cheap: QD1 writes 24 → 540 IOPS with the attribute on.
  It never takes sequential I/O, and while it writes back to the disks, sequential I/O suffers, nvweaver's reads most.
- **iSCSI still leads on single I/Os** (about 150 µs writes, 210 µs reads on flash), from DSM's page cache. nvweaver's QD1
  reads took about 1 ms on bare flash but 590 µs in the Virtual DSM on the same SSDs, both reaching the SSDs one for one: the
  bare box's read path has about 0.45 ms more, not explained yet.
- **A Virtual DSM gets about 70% of its host at depth** (67-84%), with or without the sync attribute, so before/after ratios
  there carry over. Its single writes are faster than bare metal's: the host caches the virtual disk's writes (fewer reach
  the SSDs than the guest makes), so a power-cut test there would test the hypervisor's cache mode.
- Neither box was CPU-bound (at most 61% busy); SPDK's one reactor was at 85% of a core in the Virtual DSM at its deepest.

### Honouring FUA on SPDK: the first experiment (2026-11-12)

On branch `fua-experiment`: `spdk-patches/0001-nvmf-make-FUA-writes-durable-with-a-flush.patch`, carried in
`Containerfile.spdk`, follows each FUA write with a flush of its blocks before answering the host, and
`NVWEAVER_SPDK_FILE_SYNC=off` (experimental) leaves the sync attribute off (decision 28).

**Safe.** `vm-e2e.sh` with `POWER_ONLY="spdk reflink 8"` and `SERVICE_ENV="-e NVWEAVER_SPDK_FILE_SYNC=off"`: unpatched, 3
rounds of 3 corrupted the host's XFS (it wouldn't mount, and every file whose `fsync` had finished was lost), so the test
still catches this; patched, **8 of 8 clean, 192 checks, all 410 such files intact**. The target's disk took 86-88 flushes
for 20 host `fsync`s, against 46 unpatched: each FUA write now flushes.

**Not yet fast enough.** On a Virtual DSM (`bench-classes.sh` with `DURABLE=1`, nvweaver only, medians of three rounds),
against 0.5.0 as shipped: plain writes as fast as with no attribute (4K QD1 304 → 3,742 IOPS, QD16×4 9,980 → 27,198,
128K sequential 318 → 764 MB/s); durable 4K writes, O_DSYNC and fsync, unchanged (about 300 IOPS at QD1, 10,000 at QD16×4);
but **O_DSYNC 128K sequential writes at QD16 fell from 320 to 50 MB/s.** SPDK's aio flush is a blocking `fsync()` on its one
reactor, so queued FUA writes are made durable one at a time, with the reactor idle meanwhile (16% of a CPU, against 46%).
The next version should send FUA writes through a second `O_DSYNC` file descriptor, asynchronously, as all writes are made
durable today: see decision 28.

### Honouring FUA on SPDK: the second version (2026-11-12)

`spdk-patches/0001-bdev-aio-make-FUA-writes-durable-with-O_DSYNC.patch`, in SPDK's aio module only: each file is opened a
second time with `O_DSYNC`, and a write with FUA set (bit 30 of its NVMe CDW12, which SPDK's NVMe-oF target already passes
to the bdev unchanged) goes through that descriptor, still asynchronously through libaio. It works on Linux 4.4.

**Safe:** the same power-cut test, 8 of 8 clean, 192 checks, all 393 files whose `fsync` finished intact.

**Fast:** on the same Virtual DSM, medians of three rounds, against 0.5.0 as shipped: plain writes with the full gain (4K
QD1 304 → 3,807 IOPS, QD16×4 9,980 → 26,832, 128K sequential 318 → 706 MB/s) and **durable writes at the shipped speed**
(O_DSYNC 4K QD1 305, QD16×4 9,980, 128K sequential 316 MB/s against 320; fsync after each write unchanged).

**On bare metal too:** the all-flash Synology, against 0.5.0 as shipped, medians of three rounds: 4K writes QD1 354 →
1,209 IOPS, QD16×4 14,081 → 38,735, 70/30 28,550 → 46,445, 128K sequential 309 → 679 MB/s; durable writes the same (O_DSYNC
4K QD1 350/356, QD16×4 14,034/14,035, 128K 296/303 MB/s). With power-cut safety kept, nvweaver then matched or beat DSM's
iSCSI on that box for deep random writes, mixed load and sequential I/O.

**One caveat, on both:** durable writes are slow on a newly filled volume until the host's first flushes. Round 1's, about 8
minutes after the fill, were 20-60× slower than as shipped at QD16×4; rounds 2 and 3, after the `fsync` job's flushes,
matched it. On a fresh 4 GiB volume, a flush straight after the fill didn't help, but one after more writes did. Not yet
explained; a guess is that newly allocated space makes btrfs log the whole file on every `O_DSYNC` write until a full
`fsync` clears it, which the sync attribute does with the fill's first write. A filesystem on the volume flushes
constantly; a raw-block writer that never flushes would see it. Hosts' flushes remain a blocking `fsync()` on the reactor.

### The user's choice: `NVWEAVER_SPDK_DURABILITY` (2026-11-13)

Decision 58: the SPDK image carries upstream's `nvmf_tgt` and the patched `nvmf_tgt-fua`, and the setting picks one. Run in
a fresh lab VM, with images built from this change:

- `e2e.sh`: kernel target 351 passed, SPDK 376, and SPDK with `SPDK_DURABILITY=fua` 376, none failed; nothing left behind.
  Its new checks: the `durability` capability, the sync attribute set or cleared on an existing volume file when the mode
  changes across a restart (both ways), an unknown mode refused, the setting refused on the kernel target, and the
  patched binary and its patch in the image. The two binaries differ (`cmp`).
- The power-cut test (`POWER_ONLY="spdk reflink 8"`, `SERVICE_ENV="-e NVWEAVER_SPDK_DURABILITY=fua"`): 8 of 8 clean, 192
  checks. That run couldn't show which mode the service was in, so `vm-e2e.sh` now checks the capability against
  `SERVICE_ENV` at the start of each round; a 3-round rerun passed it every round (80 checks).

Not yet run in `fua` mode: the power-cut test on a block device (in `TODO.md`).

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
| `dmsetup suspend` on thin LVs, then `lvcreate -s` of each, to pause a consistency group | `lvcreate` failed ("decrement suspended device counter below zero"), LVM resumed one LV itself, and the thin pool was left with an unexpected transaction ID, so later snapshots failed | Never suspend LVM's devices behind its back. Pause at the NVMe target instead: decision 56 |
| ANA state `change` on SPDK | Refused: SPDK 26.09 accepts only `optimized`, `non_optimized` and `inaccessible` | Use `inaccessible` on both targets |

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
- **A Synology's iSCSI far ahead of nvweaver on reads:** looked like the protocol. Its reads came from DSM's RAM, as the
  box's disk counters showed; nvweaver's `O_DIRECT` reads reached the disks. Compare what reaches the disks, not only fio.
- **A Virtual DSM faster than the box it runs on, for single writes:** the host was caching the guest's writes.
- **A Kubernetes node Ready, and its pods Running, just after the cluster was powered on:** the API served the state from
  before the shutdown for about a minute; the node then went Ready=False. Wait for containers started since the boot.

## Escaped a scoped run

Bugs that a full run on `main` or a release found, which a scoped run should have caught. Two between releases, or any one involving lost data or a security hole, means reconsidering decision 39.

None so far.

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
- A unit test of reflink snapshots skipped itself on tmpfs and ext4, saying that the end-to-end suites covered it. They never
  checked that a snapshot is read-only, so nothing did. `e2e.sh` now checks.
- The crash test in `e2e.sh` passed in about one run in five without any change in progress when the service was killed, so
  recovery was never exercised. It now requires one.

## Test pitfalls

- **A pod on a class with `Immediate` binding gets a passing `FailedScheduling`,** "unbound immediate PersistentVolumeClaims",
  until its volume is made. `bench-classes.sh` took it for a failure; it now ignores that one message. A real failure still
  shows as `ProvisioningFailed` on the claim.
- **`pkill -f` matches the command that runs it,** when that's a shell over SSH whose text contains the pattern: it killed the
  session. Stop background processes by the PID they recorded.
- **Stopping k3s on a node leaves its containers running.** A test that then deletes the node leaves any leader that was
  there renewing its lease, so its replacement never takes over. Drain the node first.
- **A VolumeGroupSnapshot that nothing answers:** external-snapshotter 8.7.0's manifest runs snapshot-controller 8.6.0, which
  has group snapshots off, though the README says they're on by default. The object then has no status, no events, and
  nothing in any log, and the driver is never called. `k3s-csi.sh deploy` turns them on.

- **A write to a host's NVMe device that can't finish ignores `timeout -s KILL`.** It's uninterruptible until the host gives up on
  the controller, 600 seconds by default. Run it in the background and check whether it finished, then disconnect.
- **`echo n > file` empties the file before writing,** so a reader at that moment gets nothing. Write elsewhere and `mv` it.
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
- **The k3s lab's agents run `k3s.service`, not `k3s-agent`,** as `k3s-lab.sh` writes its own unit. Stopping `k3s-agent` does
  nothing, quietly, and a deleted node registers again at once.
- **DSM's SSH has no SFTP,** which `scp` uses now, so it fails with "Connection closed". Pipe files with
  `ssh host 'cat > FILE' < FILE`, or use `scp -O`.
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
- **In a shell, `$?` straight after `$(...)` is that command's status.** `echo "$(date) $?"` logged `date`'s, so a writer reported no errors for what had failed. Save `rc=$?` first. It's the iSCSI probe's, but could be anyone's.
- **`pvs -a` doesn't list everything LVM can read:** it leaves out a disk with no PV on it, whatever else is on it, though the first experiment suggested otherwise. Use `lvmdiskscan`.
- **`ausearch` reads stdin when it isn't a terminal,** so in a script piped to `bash -s`, it swallows the rest of the script. Use `</dev/null`.
- **One machine:** an initiator can't see the same namespace twice. Two host NQNs on one
  kernel also share a multipath device, so test multi-host by disconnecting one and reading
  through the other.
