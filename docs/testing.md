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

The fakes encode what we believe the real systems do, so only the end-to-end suites can
prove behaviour. Anything new that touches a target needs end-to-end coverage.

## Latest results

Fedora 44 VMs on KVM, kernel 7.2.15, SELinux enforcing, 4 vCPUs and 4 GB each, rootful Podman
5.8, SPDK v26.09. Images were built with rootless Podman on the VM host.

| Suite | Path | Result |
|---|---|---|
| Unit tests | | Pass |
| `e2e.sh`, kernel target | Loopback in one VM | 105 of 105 |
| `e2e.sh`, SPDK target | Loopback in one VM | 116 of 116 |
| `systemd-e2e.sh` | Loopback in one VM | 67 of 67 |
| `vm/vm-e2e.sh` | Between two VMs | 129 of 129 |

Earlier versions of the first three also passed on Docker, with 101, 107 and 67 checks. Docker
hasn't been tested since Podman became the default.

## Not tested

Deferred until hardware is available, as tracked in `TODO.md`:

- Physical NVMe drives, including passthrough. So far these are emulated by QEMU
- A physical network. So far: loopback, and QEMU's link between VMs
- Install and reboot on physical hardware, whose device timing at boot may differ from a VM's
- A host that really has no `nvmet-tcp`. This was simulated by withholding kernel modules

Not tested for other reasons:

- Initiators other than Linux, or any formal NVMe conformance suite
- `compose.yaml`: Compose wasn't available
- Docker since Podman became the default. It's a fallback: see `TODO.md`
- SPDK on a real older CPU. Nehalem was under QEMU, and only as far as a running reactor
- Non-x86-64 builds
- Scale. Nothing has been measured beyond a handful of objects

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

## Test pitfalls

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
- **`rsync --exclude nvweaverd`** also skips `cmd/nvweaverd`, and everything still looks fine at
  first. Anchor it, as in `--exclude /nvweaverd`.
- **`podman --device` with a `/dev/disk/by-id` path:** the symlink doesn't exist in the container.
  Give a destination, as in `--device /dev/disk/by-id/x:/dev/nvweaver0`.
- **SELinux checks:** the native unit runs as `unconfined_service_t`, so "no denials" proves little
  there. The Quadlet's `container_t` is the meaningful test.
- **QEMU:** with extra disks, the root disk needs `bootindex`. An SSH agent with many keys can use
  up a VM's login attempts, hence `IdentitiesOnly`. `if=virtio` drives take IDs like `virtio0`.
- **One machine:** an initiator can't see the same namespace twice. Two host NQNs on one
  kernel also share a multipath device, so test multi-host by disconnecting one and reading
  through the other.
