# Decisions

What was chosen, what was rejected and why. Add new entries at the end, and if a decision
is reversed, add a new entry rather than rewriting the old one.

Measurements come from a loaded 8-vCPU VM over loopback: useful for comparing options
there, not as absolute numbers.

## 1. Kernel `nvmet` is the primary target

- **Chosen:** configure the kernel target through configfs.
- **Why:** protocol compliance comes from the Linux target rather than a new
  implementation, I/O never passes through this service, and it uses no polling core.
- **Rejected:**
  - A custom protocol implementation: far more work, and compliance would all be on us.
  - SPDK as the only target: larger, uses more memory, and I/O stops with the container.

## 2. SPDK is a fallback, and `auto` prefers the kernel

- **Chosen:** SPDK runs only where the kernel target can't, or when asked for.
- **Why:** SPDK showed higher IOPS in testing, but that isn't a reason to prefer it. Kernel
  CPU use wasn't measured, it uses more memory (about 240 MB, against 8 MB for the service),
  has worse queue-depth-1 latency in interrupt mode, and can't keep serving while the
  container is down.
- **Trade-off:** SPDK is a second protocol implementation, so behaviour can differ subtly.
  Both run the same test suite to catch that.

## 3. SPDK uses AIO for all storage

- **Chosen:** `bdev_aio` for every backing device and file.
- **Why:** it works on any block device or file, with storage staying on its normal kernel
  driver, and needs no special permissions.
- **Rejected:**
  - `io_uring`: Docker's default seccomp profile is understood to block it, which would hurt
    portability. This wasn't tested: it was dropped before an `io_uring` build ever ran, and
    its performance here is unknown.
  - SPDK's userspace NVMe driver: fastest, but it takes the drive away from the kernel
    through `vfio-pci`, which is risky on a shared host and only helps NVMe drives.

## 4. SPDK defaults to interrupt mode, without hugepages

- **Chosen:** `--interrupt-mode` and `--no-huge`, on one CPU, by default.
- **Why:** it idled at about 1% of a core, against 100% when polling, and needs no host
  preparation.
- **Measured (1 core):**

  | | Interrupt | Polling |
  |---|---|---|
  | 4k random read, QD32 x 4 | 41k IOPS | 48k IOPS |
  | 1M sequential read | 955 MiB/s | 994 MiB/s |
  | 4k mean latency, QD1 | 412 us | 218 us |
  | Idle CPU | About 1% | 100% |

- **Available:** `NVWEAVER_SPDK_POLLING` and `NVWEAVER_SPDK_HUGEPAGES` turn on the others.

## 5. SPDK is built for x86-64-v2

- **Chosen:** `--target-arch=x86-64-v2` rather than SPDK's default of `native`.
- **Why:** a native build crashes on older CPUs, and portability is the reason for having
  this backend. Under QEMU it ran on Nehalem, and crashed with an illegal instruction on
  Conroe, which confirms both the floor and that the test can detect a violation.

## 6. Refuse what a backend can't do

- **Chosen:** `Backend.Check` rejects it with a 422, and `/v1/system` reports capabilities.
- **Why:** a setting that is silently ignored is worse than an error.
- **Exception:** `dhchap_hash` and `dhchap_dhgroup` are accepted on SPDK without being
  enforced per host, since it negotiates from what both sides support, and keys are still
  enforced. The README documents this.

## 7. Wildcard addresses are refused on SPDK

- **Chosen:** refuse `0.0.0.0` and `::` on SPDK. The kernel accepts them.
- **Why:** SPDK accepts a wildcard listener, then rejects every host with an error blaming
  the host, because the address they connected to doesn't match the listener.
- **Rejected:** expanding a wildcard to each local address, which goes stale as they change.

## 8. Files on overlayfs are refused on the kernel target

- **Chosen:** `Target.Check` refuses them, by filesystem magic number. SPDK allows them.
- **Why:** `nvmet` enables the namespace without error, but hosts see a hidden disk of size
  zero with no device node. A container's own filesystem is overlayfs, so running without a
  volume would hit this, with the API reporting success. SPDK was tested and works there.

## 9. Disks in use are refused

- **Chosen:** open block devices with `O_EXCL` before exporting them, and refuse exporting
  the same backing store twice. There is deliberately no override.
- **Why:** `nvmet` opens devices non-exclusively, and would export a mounted root disk for
  hosts to overwrite.

## 10. The API is on loopback unless it has a token

- **Chosen:** default to `127.0.0.1:8420`, and refuse to start off loopback with no token.
- **Why:** the API can export any unused block device on the host.
- **Known gap:** there is no TLS yet, so a token crosses the network in clear text. See
  `TODO.md`, phase 3.

## 11. Stopping removes the target by default

- **Chosen:** stopping removes configuration from the kernel, and `NVWEAVER_KEEP_ON_EXIT`
  opts out.
- **Why:** people expect stopping a service to stop it. Hosts reconnect by themselves after
  a restart, and the opt-out allows upgrades with no interruption.

## 12. A dead SPDK process ends the service

- **Chosen:** the service exits non-zero, and a restart policy restores everything.
- **Rejected:** restarting `nvmf_tgt` inside the service, which adds a supervisor that the
  container engine and systemd already provide.

## 13. systemd: native for the kernel, Quadlet for SPDK

- **Chosen:** a native unit for the kernel target, and a Quadlet running the image for SPDK.
- **Why:** a container adds nothing to the kernel target, since it would be privileged
  anyway. SPDK in a container avoids building it per distribution, and keeps the
  no-capabilities isolation.
- **Sandbox:** `ProtectSystem=full` rather than `strict`, because file namespaces can be on
  any writable filesystem. `ProtectKernelTunables`, `ProtectKernelModules`, `PrivateDevices`
  and `ProtectClock` are left out because they are expected to stop it working, going by what
  they restrict. That wasn't tested one by one. The settings that are on have been tested.

## 14. Standard library only

- **Chosen:** no Go dependencies, including for sd_notify and JSON-RPC.
- **Why:** a small static binary with no supply chain to track. Reconsider this if a later
  feature needs one, such as password hashing beyond what the standard library offers.

## 15. Testing without hardware

- **Chosen:** build on emulation for now, and track hardware tests as deferred in `TODO.md`.
- **Why:** no physical NVMe hardware is available right now.

## 16. Podman is the container engine

- **Chosen:** Podman by default, in scripts and documentation. Docker is a fallback, and
  testing against it is low priority.
- **Why:** the user's preference. Also, Quadlet is already how the SPDK image runs as a service.
- **Constraint:** the kernel target and the Linux initiator need real root, so a full
  end-to-end run needs rootful Podman, and rootless can't replace it.

## 17. Publish exactly what was tested

- **Chosen:** a release runs the whole CI pipeline, then publishes its images and files, rather
  than rebuilding them. Actions are pinned by commit, with attestations on what is published.
- **Why:** a rebuild would pull newer base images, and so wouldn't be what passed. Pinning matters
  because a workflow on this public repo can publish images.
- **Rejected:** self-hosted runners as the default. They need a trusted machine and are unsafe with
  pull requests on a public repo. Hosted runners have KVM, so the VM suites should run there.
- **Limit:** `linux/amd64` only for now. Cross-platform releases for x86-64 and arm64 are the goal,
  so avoid adding x86 assumptions, but nothing is published for Arm until it has been tested.
- **Fallback:** if hosted runners can't run the VM suites, a self-hosted runner on the lab host. On
  a public repo, it must not run pull requests from forks.

## 18. Roadmap order

- **Chosen:** the order in `TODO.md`.
- **Why:**
  - The storage layer comes first, designed in the OpenAPI spec before it's built, because it
    brings the biggest API changes. The client, CLI and web UI then get built once, against it.
  - TLS comes before the web UI and CSI driver, which reach the API over a network.
  - Scale work comes before the CSI driver, because production-like scale is a requirement.

## 19. Pools, volumes and snapshots are separate from exports

- **Chosen:** Pool -> Volume -> Snapshot, with a clone being a new Volume made from a snapshot or
  volume. A Volume exists whether or not it is exported, and owns the UUID, NGUID, internal name and
  state. A Namespace becomes a mapping of a Volume into a Subsystem, holding little more than the
  NSID. `device_path` namespaces stay, for exporting an existing disk directly.
- **Why:** a snapshot or clone must exist whether or not anything exports it. A volume has to keep
  its identity when it moves to another node, or hosts see a different disk.
- **Reviewed against:** NetApp Trident, Dell csi-powerstore and Dell csi-powermax, read for comparison
  only, with no code taken. All three separate storage from exposure. ONTAP's "namespace" is what we
  call a Volume. We don't copy multi-array backends, storage class engines, QoS or replication.
- **Pools can be nearly invisible:** PowerStore exposes none, so a default pool is reasonable.

## 20. Export per node, not per volume

- **Chosen:** CSI maps volumes into one subsystem per node, and raw volumes shared between nodes into
  a shared subsystem.
- **Why:** subsystems and connections then follow nodes rather than volumes. All three vendors scope
  visibility per host: Trident with per-node subsystems, PowerMax with per-node masking views,
  PowerStore with host mappings inside one subsystem. As far as we know the kernel target can't hide
  namespaces per host within a subsystem, so per-node subsystems are what works on both backends.
- **Limit:** 1,024 namespaces per subsystem, on our side and in Trident's driver.

## 21. Deleting a volume with snapshots simply works

- **Chosen:** on LVM thin and reflink, delete at once. Tests confirm that snapshots and clones survive
  losing their origin. Any future pool type where they don't is refused. Deleting a volume that is
  exported is always refused.
- **Rejected:** Trident's soft delete and PowerMax's deletion queue, which suit their arrays. PowerStore
  refuses. We add a `state` field, but no soft-delete state until something needs one.

## 22. Pools are adopted, not created

- **Chosen:** adopt an existing LVM volume group or thin pool, or an existing reflink-capable directory.
  `docs/pools.md` documents how to create them. The service doesn't initialize disks.
- **Why:** initializing a disk is destructive, and the settings matter at scale, as `docs/pools.md`
  explains. Creating pools could come later, behind an explicit confirmation.
- **Pool types:** LVM thin and reflink files, from the start. Reflink needs no LVM and no extra
  privileges, so it also suits SPDK with no capabilities. Classic LVM snapshots are rejected.

## 23. Storage changes record their intent first

- **Chosen:** an intent log for pool operations, and a separate internal name for each volume.
- **Why:** today we apply and then save, which is fine for configuration we can re-apply, but a crash
  between `lvcreate` and the save would orphan storage. Trident does the same with transactions.
- **Built as:** one small, fsynced file for each change in progress, so the cost doesn't grow with volumes. Recovery
  compares each name with the saved state, rather than replaying steps, so it works the same for every pool type.
- **Rejected:** rolling back a grow, as a host may already have used the space. It's finished instead, so a client whose
  request was interrupted may find that it took effect, as with any create.

## 24. The scale target is 5,000 volumes across 100 node subsystems

- **Chosen:** this as the first target, measured with `test/vm/scale-bench.sh`.
- **Design consequences:**
  - Incremental reconcile, batching per subsystem, and returning "busy" under load, as PowerMax does,
    rather than one global lock and a full reconcile on every change.
  - Slow operations don't block the API.
  - Create is idempotent by name, because CSI retries.
- **Left room for:** group snapshots, which all three vendors support. Not in the first version, and
  LVM may not offer an atomic multi-volume snapshot.

## 25. nvweaver runs on its own machine, with the privileges it needs

- **Chosen:** the expected deployment is a dedicated machine outside any Kubernetes cluster, serving it over the
  network. It's a storage system, so running privileged is fine.
- **Why it matters:** this removes constraints such as cluster pod security policies and needing to manage LVM with no
  capabilities. Running SPDK with no capabilities still works, and is tested, but isn't something to design around.
- **Consequence:** in a container, LVM runs as the host's own through `nsenter`. See `docs/storage-api.md`.
- **For the CSI driver:** its controller and node plugin run in the cluster and reach this machine over the network, so
  the API's TLS and token matter.


## 26. The transport belongs to the port, and NVMe/RDMA is deferred

- **Chosen:** ports get an optional `transport`, defaulting to `tcp`, and `capabilities.transports` says what can be
  used. A transport that can't be used is refused. Clients read the transport from the port. See `docs/storage-api.md`.
- **Why now:** adding the field later wouldn't break the API, but the CLI and CSI driver could be written assuming TCP.
  So it arrives before them, accepting only `tcp`.
- **Why not on the subsystem or volume:** a subsystem refers to ports already, so this lets one be reached over two
  transports, and storage needs no changes.
- **RDMA itself is deferred until after the CSI driver:** nothing on the roadmap depends on it, and the links here are
  1 Gb Ethernet with no RDMA NICs, so its benefits couldn't be measured. Both targets support it underneath. The
  main unknown is whether SPDK's RDMA transport works with the `--no-huge` and interrupt mode that we use. Not tested.

## 27. arm64 is released from v0.2.0

- **Chosen:** v0.2.0 publishes arm64 images and a tarball alongside x86-64, built natively on Arm runners, with an Arm job
  in CI so that it stays working.
- **Why now:** the suites pass under emulation on `-cpu max` and Cortex-A72, apart from a Quadlet timeout since fixed;
  `e2e.sh` passes on a real Arm runner; and SPDK passes over a physical network on a real CM4. What's left is mainly CI
  and release work.
- **What it promises:** arm64 with SPDK, and with the kernel target where the host's kernel has `nvmet-tcp`, as Fedora's
  has. Raspberry Pi OS's kernel doesn't, so it's SPDK only there.
- **Not promised:** a Pi 5 and its 16K-page kernel, which is untested; Arm performance or scale, which aren't measured.
- **CI runs the suites directly on Arm,** without VMs, as its runners have no KVM. Emulated VMs took over two hours, which
  is too slow for every change, so `TEST_TIMEOUT_SCALE=10` and `LAB_ARCH=aarch64` remain a manual check.
