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
- **"The same store" means what a path reaches, not its name:** a file by filesystem and inode, so a hard link or a bind
  mount counts, and a block device by its number, where a disk and any of its partitions overlap. Two partitions of one
  disk don't. This applies to mapping a volume too. The `O_EXCL` open stays, for what's mounted or held at that moment,
  as it's released before `nvmet` opens the device.
- **Stacked devices, from the second review:** `O_EXCL` already refuses a disk or partition that device-mapper holds,
  and a disk one of whose partitions is held. What it can't see is the other order: a logical volume exported over a disk
  that's exported already, as the target opens what it exports without claiming it. So a new block device's "slaves" in
  sysfs are walked down to the disks, and each, with the disk it's a partition of, is compared with what's exported. Only
  the new path is walked, so that a request reads sysfs for one device rather than for each of 5,000 volumes. Two thin
  volumes in one pool are built on the same devices and don't overlap, nor does a volume on one partition with another
  partition of the disk. A stack more than 32 devices deep is refused rather than passed unchecked.
- **A block device that sysfs can't describe is refused,** with an error saying sysfs is needed to tell a disk from its
  partitions, as passthrough already is. Skipping the check would pass a disk and its partition. A namespace already
  exported that sysfs can't describe doesn't block other exports.
- **Passthrough gets the same care, for every namespace of the controller,** found through sysfs. With native multipath,
  as on fabrics controllers and shared namespaces, the controller's directory lists only hidden path devices, such as
  `nvme2c2n1`, which have no device node. The namespace, `nvme2n1`, is in the NVMe subsystem's directory. Path devices
  with no namespace found is refused, rather than taken as nothing to check, which a simpler lookup would do.

## 10. The API is on loopback unless it has a token

- **Chosen:** default to `127.0.0.1:8420`, and refuse to start off loopback with no token.
- **Why:** the API can export any unused block device on the host.
- **TLS:** added later, in decision 31. Without it, a token crosses the network in clear text.

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
  - Incremental reconcile, and returning "busy" under load, as PowerMax does, rather than a full reconcile on every
    change. Batching was planned too, and turned out not to be needed. See decision 29.
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
- **The tags cover both architectures.** `0.2.0` and `0.2.0-spdk` are manifest lists over tags that end `-amd64` and `-arm64`,
  so that nobody has to choose. Attestations are on the lists, whose digests pin each image.
- **CI runs the suites directly on Arm,** without VMs, as its runners have no KVM. Emulated VMs took over two hours, which
  is too slow for every change, so `TEST_TIMEOUT_SCALE=10` and `LAB_ARCH=aarch64` remain a manual check.

## 28. On SPDK, files are made durable with the sync attribute

- **The problem:** SPDK's AIO bdevs tell hosts there's a write cache, but never honour FUA: nothing in its write path
  reads the bit. XFS commits its log with a FUA write and relies on it. In a file, the new blocks stay in the target's
  page cache and filesystem journal until the next flush, so **a power cut corrupted hosts' filesystems**, including files
  whose `fsync` had finished, in 5 of 8 rounds on a reflink pool. The kernel target honours FUA, and passed.
- **Chosen:** set the file's sync attribute (`chattr +S`) before SPDK opens it, so that the kernel treats each write as
  `O_DSYNC`. 8 of 8 rounds were then clean. A filesystem that can't do this is refused, rather than silently unsafe.
- **The cost:** on the same live volume in VMs, 4k random writes fell from about 5,200 to 1,200 IOPS at queue depth 16, and
  from about 1,570 to 810 at depth 1. 1M sequential writes fell from about 80 to 64 MB/s. Reads didn't suffer. Flushes
  are cheap on these disks, so a real disk may lose more.
- **Rejected:** patching SPDK to honour FUA, which is the proper fix but needs carrying a patch, for now. It's worth reporting
  upstream. Reporting no write cache to hosts, as they'd then send no flushes at all. An option to turn this off, until
  someone needs it: a test lab can tolerate slow writes better than a corrupted filesystem that looks like a client's bug.
- **Still open: block devices on SPDK, including LVM volumes.** They have no such attribute, and FUA is ignored there too. Those
  writes do reach the device, and passed here, but QEMU's disks don't lose unflushed writes. A real drive with a volatile
  cache might lose them on a power cut. This is untested, and documented in the README.

## 29. Changes are applied one at a time, and storage work runs outside the lock

- **Chosen:** reads load the current state, which is replaced whole and never modified, so they never wait. A change takes one lock
  for its reconcile and save. Slow storage work runs before that, without the lock.
- **Reservations, not queues:** while working on storage, a request holds its object exclusively, and what it depends on shared, such as a
  clone's source. Anything that clashes gets `503` with `Retry-After` straight away, and nothing waits for a reservation. So does a pool with
  32 operations running, or a queue of 256 changes. A client that retries, as CSI does, needs nothing more, and a retry of a clone
  still running is told to wait, rather than being called a conflict.
- **Rejected: batching changes into one reconcile and save.** It was the plan, in decision 24. The numbers don't justify it: at 5,000 volumes a change
  takes 20 ms on the kernel target and 60 ms on SPDK, and SPDK's cost is RPCs, one each per volume, which a batch can't share. It would also make
  failures hard to report fairly, as one bad namespace would fail its neighbours' requests, and some requests can't simply be run again.
  Reconsider if a real client needs more than about 50 changes a second.
- **Rejected, for now: a journal instead of rewriting the state file.** A whole change takes 20 ms at 5,000 volumes on the kernel target, save
  included, once the file is compact, so there's little left to gain.
- **Stopping SPDK no longer takes it apart first:** its configuration goes with its process, and doing so took about 100 s at 5,000 volumes.
- **SPDK is limited to 8,192 subsystems,** rather than its default of 1,024. It's fixed at startup, and beyond it a request is refused with a clear
  error. The kernel target reached 5,000 with no limit in the way, and the difference is documented in the README.

## 30. Removing a host drops the subsystem's connections on the kernel target

- **The problem:** the kernel only checks `allowed_hosts` when a host connects. A removed host stayed connected, and kept writing with no errors, so
  removal couldn't fence it, and differed from SPDK.
- **Chosen:** when a host loses access, detach the subsystem from its ports and attach it again, after removing the host's link. Any connected
  host on that subsystem is dropped, and the removed one can't return. Tested in `vm-e2e`: its write never reaches the disk.
- **The cost:** another host on that subsystem stalled for 10.3 s, its default reconnect delay, with no errors. A host on another subsystem on the same port
  saw no effect. The usual case of one host per subsystem disturbs nobody, but a shared subsystem does stall, as after a live migration.
- **Rejected: nvmet's debugfs,** which is thought to allow dropping one controller. That's unconfirmed, as Fedora's kernel is built without
  `CONFIG_NVME_TARGET_DEBUGFS`, so it can't be tried here. It's worth revisiting if the distributions turn it on, since it could avoid the stall.
- **Rejected, for now: an option to keep connections.** Nothing needs it yet. The CSI driver might, for live migration: see `TODO.md`.
- **Changing a host's `dhchap_key` or `dhchap_ctrl_key` drops its connections too, decided by the user.** Both targets only check keys at login,
  so a host stayed connected on its old key indefinitely. On the kernel target this is the same detach and attach, after the new key is written,
  with the same stall for other hosts on the subsystem. On SPDK, `nvmf_subsystem_set_keys` left the session up, as `e2e.sh` showed on 26.09, so
  the host is removed and added again with its keys, which drops only that host. **So update a host's key on the host first:** a host without the
  new key can't reconnect, and its I/O fails once it gives up.

## 31. The API's TLS uses files it's given, and reads them again when they change

- **Chosen:** `NVWEAVER_TLS_CERT` and `NVWEAVER_TLS_KEY`, as PEM files, both or neither. With them, plain HTTP is refused. TLS 1.2 is the minimum.
- **Reloading:** each handshake checks whether the files have changed. Restarting to renew a certificate would stop SPDK's data path,
  and a timer or signal is one more thing to set up. A check is one `stat` per file, and handshakes are rare.
- **A bad change keeps the current certificate,** and logs an error once. A renewal replaces two files one after the other, so a pair that
  doesn't match is normal for a moment. At startup, the same mistake is fatal, before the target starts.
- **Rejected: creating a self-signed certificate.** Clients could only skip verifying it, which leaves the token open to anyone in the path.
- **Rejected, for now: client certificates.** The bearer token already authenticates clients. Reconsider if something needs clients to have
  separate identities.
- **A token without TLS off loopback is only logged as a warning, for now,** as refusing it would break existing v0.2.0 setups on an isolated lab
  network. **By 1.0, the admin deploying it decides:** clear text off loopback will be an explicit choice, rather than something that happens by leaving
  TLS unset. See `TODO.md`.

## 32. nvweaver reports when this machine's LVM can read exports, rather than preventing it

- **The problem, tested:** with no devices file, this machine activates a client's LVM on an exported disk at boot, and a client's volume group named like a pool's takes that pool
  offline. Debian and Ubuntu ship that way, as does Fedora until its first `pvcreate` or `vgcreate`. Thin volumes are safe unless `scan_lvs=1`. A devices file that exists protects both.
- **Chosen:** run `lvmdiskscan` in the background, compare it by device number with what's exported, and report under `warnings` in `/v1/system`. It gives LVM's own answer, after its
  devices file, filters and `scan_lvs`, so this doesn't have to work those out. That was checked against each of them, and it took about 0.05 s with 300 thin volumes.
- **Why ask LVM, rather than read its configuration:** `use_devicesfile=1` means nothing if the file doesn't exist, as on a Fedora machine that has never used LVM, which reading the setting would have got wrong.
- **Rejected: `pvs -a`,** which was tried first. It leaves out what has no PV on it, so it only warned after a client had already made LVM.
- **Rejected: flags on nvweaver's own LVM commands,** such as `--devices`. It's the host's own activation at boot that takes the disk, which they can't stop.
- **Rejected: changing the host's LVM configuration.** Only the admin knows which disks are the machine's own, and a wrong devices file can stop it booting. nvweaver adopts what's there, and
  doesn't initialize.
- **Rejected: refusing to export a disk that LVM can read.** That's the default on Debian and Ubuntu, and only matters if a client uses LVM.
- **In the background,** because it has to scan, and `/v1/system` should stay fast. So a warning can be up to 30 seconds late after a change.

## 33. Storage stays independent of the protocol that exports it

- **Chosen:** pools, volumes and snapshots know nothing of how they're exported. Anything specific to a protocol goes on the mapping, not on them. A volume is storage, and exporting it is a separate step.
- **Why:** it keeps a second protocol, such as iSCSI, from rewriting the storage API that the CLI and CSI driver are built on. A probe, in `docs/testing.md`, backs up the estimate that iSCSI would mostly add resources beside `/v1/subsystems`, as it needed nothing different from pools, volumes or snapshots.
- **What already touches NVMe:** `mapped_to` on a volume names a subsystem and namespace, so it would need extending, which can be done without breaking clients. `uuid` and `nguid` stay as a volume's identity. On LIO, a WWN can come from `nguid`, as tested. On SPDK, it can't: identity comes from a hash of the bdev's name.
- **Consequence:** if a volume or pool seems to need another NVMe-specific field, put it on the mapping instead.
- **Rejected: `transport: iscsi` on ports.** `transport` is for ways of carrying NVMe, as in decision 26, and iSCSI is a different protocol.
- **Not chosen yet: building iSCSI.** It's in the "Later" section of `TODO.md`, after the CLI and CSI driver.

## 34. Missing tools stop the service only if the chosen target can't run

- **Chosen:** what the chosen target can't run without stops it at startup, as it already did: `nvmet-tcp`, or `nvmf_tgt`. Anything else is only reported under `warnings`, and only if something saved needs it. So far that means `lvm` and `thin_check`, with an LVM pool saved.
- **Why not stop for those too:** a pool's tools going missing shouldn't take down everything else this machine exports. Someone using reflink pools doesn't need LVM at all. And without `thin_check`, tested, LVM still activates thin pools, only skipping its check of their metadata.
- **It looks where the tool would run,** on the host through `nsenter` in a container, as checking inside the container would give the wrong answer. It asks LVM where `thin_check` should be, as that's configurable.
- **Not checked: `modprobe` and `nsenter`.** A missing `modprobe` only matters if the module isn't loaded, and its error already says what to do. `nsenter` is in both images.

## 35. The client is written by hand, and tested against the spec and the real handlers

- **Chosen:** `internal/client`, using the standard library, with its types written out. The command line utility uses only this, as the CSI driver will, so neither can do anything the API can't.
- **Why not generate it:** a generator would be this project's first dependency, even if only at build time, and the API is small. What generating would have guaranteed, tests check instead: every schema's fields against the
  client's types, in both directions, and every operation in the spec reached successfully against the real handlers.
- **It retries `503`,** which the API only sends having changed nothing, for up to a minute, as `Retry-After` says. It doesn't retry a network error, as that doesn't say whether anything happened. Each caller would otherwise write this itself.
- **It doesn't follow redirects,** as the API never sends one, so something else is answering.
- **It's under `internal/`,** as nothing outside this repository uses it. The module path isn't importable from elsewhere anyway. Reconsider if someone needs it.
- **The utility takes secrets only from files or the environment,** as other users of a machine can see command lines. That applies to the token and DH-HMAC-CHAP keys, and a `--token` flag is refused.
- **Rejected, for now: a confirmation prompt on delete.** It's for automation first, and the API refuses to delete what's in use.
- **Rejected, for now: an option to skip verifying certificates.** `--ca` covers a lab's own certificate, and it's easy to add if it's really needed.

## 36. The API refuses web pages, and with no token, any name but loopback

- **The problem, and it was real:** with no token, which is allowed on loopback, any web page open in a browser on the machine could send requests to the API, and could export any unused disk. A request needs no permission from the server if it doesn't
  ask to read the answer, and the API accepted a JSON body with any `Content-Type`. Tests showed that such a request got 201 and took effect. DNS rebinding also let a site read answers, by pointing its own name at 127.0.0.1. All of this was in v0.1.0 and v0.2.0.
- **Chosen:** Go's `http.CrossOriginProtection` on everything, which refuses unsafe requests from other origins using `Sec-Fetch-Site`, or `Origin` on older browsers. With no token, a `Host` that isn't loopback is also refused, as a site can't change that header.
- **Programs aren't affected,** as they send neither header. With a token, any name works, as the API can be on any address, and a page doesn't have the token.
- **Rejected: requiring `Content-Type: application/json`.** It would stop the writes but not rebinding, and would break plain `curl -d`, as the README's own examples use.
- **It's on by default, and only tests can turn the `Host` check off.**

## 37. The web interface is static files that use the public API

- **Chosen:** plain HTML, CSS and JavaScript in `internal/webui`, embedded with `go:embed`, and served at `/ui/`. It has no build step and no dependencies, and it reaches the service only through the public API, so it can do nothing that the API can't.
- **Why no framework:** a framework brings a build toolchain and many dependencies to audit, for a small set of tables. Reconsider if it grows into something that needs one.
- **A strict Content-Security-Policy:** only this service's own scripts, styles, images and API. Data reaches the page through `textContent` only, and a test fails if the script uses `innerHTML` or similar. A label with markup in it is tested in a real browser.
- **It's tested in a real browser,** with Playwright in a container, against `test/webui-demo`, which is the real API on a fake target. That caught what unit tests can't: a page script with a syntax error, and a stray word under every table. Screenshots caught what the checks then missed, so look at them.
- **The files need no token,** as they're the same for everyone. The API decides what a browser may do.
- **Tables show 10 rows at a time, chosen by the user,** adjustable to 250, remembered in `localStorage`, which only ever holds that number. Volumes and snapshots come from the API 500 at a time, so a filter only searches what's loaded so far, and says so.
- **The icons are copies of those in `images/`,** as `go:embed` can't reach outside its package. A test fails if they differ.

## 38. A volume's usage has its own request

- **Chosen:** `GET /v1/volumes/{volume}/usage`, rather than a field on a volume. It asks the storage, which runs `lvs` on LVM. Listing or reading a volume stays a read of memory, as programs like the CSI driver do that a lot, and don't need this.
- **It includes what's shared with snapshots and clones,** as that's what both kinds of storage report, and working out what's exclusive would take far more. So a pool's volumes can add up to more than the pool has used, which the interface says.
- **It's only shown for one volume at a time** in the interface for now, for the same reason. Reconsider a column in the list if it's wanted, using one `lvs` for a whole page.

## 39. Run what a change can affect, and everything before it ships

- **Chosen:** day to day, and later on pull requests, run the suites that a change can affect. Merges to `main` and releases always run everything, so that a wrong guess is caught before it ships. If in doubt, run more.

  | What changed | What's enough |
  |---|---|
  | `internal/webui`, or the demo | Unit tests and the browser test. `webui-real-e2e.sh` too if what the interface sends changed |
  | `cmd/nvweaver`, `internal/client` | Unit tests and `e2e.sh`, and `k3s-csi.sh` for the client |
  | `csi/` | Its unit tests and `k3s-csi.sh`, which `run-all.sh` doesn't run yet |
  | `internal/auth`, `internal/api` | Unit tests, both browser tests and `e2e.sh`, and `vm-e2e.sh` if storage handling changed |
  | `internal/storage`, `internal/target`, `internal/spdk` | Everything, as that's where data can be lost |
  | Docs only | Lint |

- **Why:** a full run takes about 25 minutes, and most of that tests storage that a Web UI change can't reach.
- **The risk:** what depends on what isn't always in the path. A volume's usage looked like an interface feature, yet it added `lvm.go` code that only `vm-e2e.sh` tests for real.
- **This is on probation, as the user asked.** A bug that a full run finds, which a scoped run should have caught, goes in "Escaped a scoped run" in `docs/testing.md`. **Two between releases, or any one that involves lost data or a security hole, means reconsidering this,** by widening the table or going back to full runs. Check the list at each release.

## 40. People sign in from a users file, in bcrypt, and bcrypt is the one dependency

- **Chosen by the user:** a file an admin keeps, at `NVWEAVER_USERS_FILE`, of `name:hash` lines. It fits how this is deployed, with no database or setup screen to secure, and can be managed like any other configuration.
- **bcrypt only,** using `golang.org/x/crypto`, which the Go project maintains, and which is pure Go. `htpasswd -B` and `nvweaver passwd` both make it. The `$6$` and `$y$` formats in `/etc/shadow` have no maintained Go library to trust, and Argon2's memory use makes floods of logins harder to handle.
- **It's vendored,** so that it can be read in this repository, and building needs no network for it. It's 56 KB, and this is the only dependency: don't add others without asking.
- **Passwords are refused over plain HTTP off loopback, by default,** chosen by the user, as they're often reused, unlike a token. `NVWEAVER_ALLOW_INSECURE_LOGIN` overrides it. It looks at the address that the connection really arrived on, as a `Host` header says whatever the sender likes.
- **Sessions are random IDs, in an `HttpOnly`, `SameSite=Strict` cookie, held in memory,** so a restart signs people out, and doesn't affect hosts. `http.CrossOriginProtection` covers CSRF.
- **Guessing is limited per address, not per username,** so someone guessing can't lock out an admin. An unknown username is checked against a decoy of the same cost, so that timing doesn't reveal who exists.
- **Programs keep using the token,** and the client has no sign-in. Rejected: a CLI login, as it has nowhere better to keep a credential than the token's file.
- **Rejected, for now: roles.** Everyone can do everything. Reconsider when there's more than a small team of admins.


## 41. Say who's connected as far as the target can really tell

- **The problem:** an admin un-exporting a volume wants to know who that will hurt. SPDK can say, with `nvmf_subsystem_get_qpairs`. The kernel only says in debugfs, and only when built with `CONFIG_NVME_TARGET_DEBUGFS`, which Fedora's kernel isn't.
- **Chosen:** `GET /v1/connections` lists what's known, with `identifies_connected_hosts` under `capabilities`. On the kernel's target it reads `/proc/net/tcp` and `tcp6`, as sockets are always there: each host address on each port, with how many queues, and no NQN or subsystem. The interface says that they may be using another subsystem on the port.
- **Nobody listed does mean nobody on either target,** so that part can be relied on before un-exporting. Discovery connections count on the kernel's target, where a socket looks the same, but not on SPDK's.
- **Rejected: the kernel's log,** which does name hosts. It's overwritten, doesn't say when hosts leave, and reading it would mean guessing.
- **Rejected, for now: debugfs where it exists.** It couldn't be tested here, and an untested path that only some kernels take isn't worth having yet. It's in `TODO.md`.
- **Cost:** one RPC for each subsystem on SPDK, so the interface only ever asks about one. Listing everyone hasn't been timed at scale.

## 42. A namespace's own file is in the intent log, and keeps its namespace

- **The problem, found by an outside review:** a namespace's `file`, which this service creates, was made and deleted
  outside the intent log that pools use. A crash after making it, before saving, left a file under a name that could
  never be used again. A crash after deleting the namespace, before removing the file, left it for good.
- **Chosen:** the same rule as pools, with its own intents: a file is wanted if a namespace refers to it, and removed
  otherwise, at the next start. Only files with an intent are touched, as one kept by deleting its namespace without
  `delete_file` is meant to stay. A name that's taken is refused before the intent is written, so that recovery can't
  remove a file that was there first.
- **Changing `device_path` on such a namespace is refused, decided by the user.** The review proposed deleting the old
  file once the change was saved. That destroys data on a `PATCH`, with no warning, and an orphan is the alternative.
  Deleting the namespace, with `delete_file=true` if wanted, says what happens to the file.

## 43. Listing every connection is refused on a target that would ask each subsystem in turn

- **The problem:** from the second review. `GET /v1/connections` with no `subsystem` costs one `nvmf_subsystem_get_qpairs`
  per subsystem on SPDK, about 8 ms each, on the one RPC connection that changes also wait on: about 40 s at the 5,000
  subsystems of decision 24. The web interface only ever asks about one, but the command line and other clients didn't.
- **Chosen:** it's refused with `422` and an error saying to pass `subsystem`, before the target is asked, on a target
  whose `lists_all_connections` under `capabilities` is false, which is SPDK. The kernel's target reads every
  connection from `/proc/net/tcp` at once, so it still lists them all. `nvweaver connection list` with no `--subsystem`
  passes on the server's error, rather than checking `/v1/system` first, so that the rule is only in one place.
- **A capability of its own, not `identifies_connected_hosts`,** which the review proposed. That one says whether hosts
  can be named, and would turn true on the kernel's target if debugfs is ever read, as `TODO.md` has it, which would
  then refuse a list that costs nothing there.
- **Decision 41 stands:** nobody listed still means nobody connected, for one subsystem and for the kernel's full list.
- **Rejected: sending the requests without waiting for each answer.** It's in `TODO.md` for startup, and would shorten
  this, but not by enough at 5,000 subsystems to make an unbounded list safe to ask for.

## 44. The CSI driver is a module of its own, with gRPC and the CSI spec vendored

- **The problem:** a CSI driver speaks gRPC, with the CSI spec's protobuf types. Decision 40 keeps the service to the
  standard library and bcrypt, and writing gRPC by hand isn't realistic.
- **Chosen, by the user:** `csi/` is its own Go module, `nvweaver/csi`, with `google.golang.org/grpc`,
  `google.golang.org/protobuf` and `github.com/container-storage-interface/spec` vendored in `csi/vendor`. The service
  and the command line utility gain nothing. The driver uses `internal/client`, through `replace nvweaver => ../`, so it
  can do nothing the API can't, as decision 35 meant. Go's `internal` rule goes by import path, which allows this.
- **The client is vendored too,** as a copy in `csi/vendor/nvweaver/internal/client`. `make lint` fails if that copy
  differs from `internal/client`, and says to run `go mod vendor` in `csi/`.
- **Rejected: one module for both,** which would put gRPC in the service's module, and in its SBOM, for code it never
  runs. **Rejected: a Go workspace,** whose shared vendor directory would do the same.

## 45. How the CSI driver maps Kubernetes onto nvweaver

- **Names are scoped by cluster:** `--cluster` starts every volume, snapshot and subsystem name, so that clusters can
  share one nvweaver. Kubernetes's names, such as `pvc-<uuid>`, fit nvweaver's; any that don't are hashed, the same
  way every time, so a retry finds the same volume. The volume's ID in Kubernetes is its nvweaver name.
- **The node ID is the node's NVMe host NQN,** from `/etc/nvme/hostnqn`, as that's what nvweaver allows in. Nodes
  that share one, as cloned images can, would get each other's volumes: detecting that is in `TODO.md`.
- **Single-node volumes go into one subsystem per node,** named from a hash of the host NQN, as decision 24 planned.
  Moving a pod to another node takes the namespace out of one and puts it in the other: a volume is only ever exported
  once. The node keeps its connection to its subsystem when its last volume goes, ready for the next.
- **Volumes several nodes use at once get a subsystem of their own,** which allows each node as it's published to, and
  is deleted when the last leaves. A node disconnects from it when it unstages the volume, as the kernel would otherwise
  keep trying to reconnect to a subsystem that's gone, for its ten minute loss timeout. Only raw block volumes may be
  written from several nodes: a filesystem written that way would be corrupted, so a ReadWriteMany filesystem is refused. Anything sharing a raw device must use direct
  I/O, as each node's page cache keeps what it read first.
- **The driver's name is `nvweaver.xphyr.github.io`,** from the project's public GitHub namespace, which a CSI name
  must look like a domain name to use.
- **The node plugin connects with `nvme connect`,** finds the volume's device by its NGUID, or UUID, within its
  subsystem, and makes ext4 or XFS on a blank volume. It never formats a device with something on it.
- **Nodes never give up on nvweaver by default:** they connect with `--ctrl-loss-tmo=-1`, so an outage pauses I/O until
  nvweaver is back. The kernel's default of 10 minutes would then drop the controller, and mounted volumes would fail for
  good. That's the right trade-off with a single target and no failover. `--ctrl-loss-tmo` on the node plugin changes it.
- **Nodes reconnect every 2 seconds, not the kernel's 10:** `--reconnect-delay=2`. On the kernel target, taking a node off a
  shared volume's subsystem, as at the end of a live migration, drops the other nodes' connections too (decision 30), and their
  I/O waits for the next reconnect. Measured on OpenShift, the stall was the delay plus about 0.1 s: 10.2 s, 2.2 s and 1.1 s at
  10, 2 and 1, with no failed write. 1 would halve it again, but in a real outage every node would then retry every subsystem
  every second.
- **A node reports 1024 volumes at most,** the namespaces its subsystem can hold on either target. `--max-volumes-per-node`
  lowers it.
- **A node ID that isn't an NQN is refused as not found,** as no node of ours can have one.
- **`PUBLISH_READONLY` isn't claimed:** nvweaver can't export a namespace read-only to one host. Read-only volumes are
  still read-only, enforced where the node mounts them.
- **A SINGLE_NODE_SINGLE_WRITER volume is published at one target path only;** a second is refused.
- **Listing returns this cluster's names only,** paged by name, with a token that must be one of them.
- **Not yet:** topology, capacity reporting, DH-HMAC-CHAP for nodes, and scale testing. They're in `TODO.md`.

## 46. Scoped tokens own what they create

- **The problem:** every token and everyone signed in could do everything. Several Kubernetes clusters sharing one
  nvweaver, each with its CSI driver, could delete each other's volumes, by mistake or otherwise.
- **Chosen, with the user:** scoped tokens, listed in a file, `NVWEAVER_TOKENS_FILE`, as `name:sha256:<hex>` lines.
  What a scoped token creates is owned by it: volumes, snapshots and subsystems carry its name as `owner`, set by the
  server, which the token can't choose. It may change or delete only what it owns, and use only what it owns: it can't
  snapshot, clone or export another's volume, or add a namespace to another's subsystem.
- **Ownership, not name prefixes,** which were the first plan. A prefix is a naming convention that a mistake breaks; an
  owner is who made it. It also gives the interface and the CLI an Owner column, which shows each cluster's volumes,
  snapshots and subsystems directly. Labels were considered, but anyone who can change a volume can change its labels.
- **Reads are for everyone,** chosen by the user, so the driver and CLI work unchanged, and clusters can see each
  other's names. Filtering every list, page and count by owner is more work for little gain.
- **Admin-only, refused with `403` before anything is looked at:** pools, ports, changing or deleting hosts, setting
  DH-HMAC-CHAP keys, passthrough, letting any host connect, changing a namespace, and namespaces on devices or files.
  A scoped token may create hosts without keys, as nodes need them, and allow and deny hosts on its own subsystems.
  Every change that isn't listed as open to scoped tokens is admin-only, so a new route is refused to them until it's
  thought about.
- **Admins** are the token in `NVWEAVER_API_TOKEN` and everyone in the users file, as before. They may set an owner when
  creating something, and give a volume or subsystem to another token, or to no one, with `owner` on `PATCH`. What has
  no owner, including everything made before this, is for admins only. **So a cluster moving from the admin token to a
  scoped one needs its existing volumes and subsystems given to that token first,** with `--owner`.
- **The file holds SHA-256 hashes, not bcrypt,** as tokens are long and random: there's nothing to guess that a slow
  hash would protect. It's refused if others can read it, as the users file is, and read again when it changes, so a
  token can be added or revoked with no restart. `nvweaver token new NAME` makes a token and its line.
- **A tokens file doesn't count as protecting the API** off loopback: with only scoped tokens, nothing could manage it.
  An admin token or users file is still needed there.
- **Rejected: tokens made through the API,** chosen by the user: the API could then mint credentials, and the state file
  would hold them. **Rejected: hosts owned by a token.** Hosts are a node's identity, and a node could be shared; when
  nodes get DH-HMAC-CHAP keys, which only admins may set now, this needs another look.

## 47. Metrics are Prometheus text, written with the standard library

- **The problem:** the user wants usage watched, and alerts set, in a third-party tool, starting before v0.4.0. A thin pool
  that fills up gives every host using it I/O errors, and today only `warnings` in the API says so.
- **Chosen, by the user:** `GET /metrics`, in Prometheus's text format, written with the standard library: pools' size, use and
  metadata, object counts, warnings, pending intents, and API requests by route and status, with their latency. OpenTelemetry
  users scrape it with the OpenTelemetry Collector's `prometheus` receiver. Names and alert examples are in
  `docs/monitoring.md`.
- **Authenticated like the API,** as it names every pool, and a scoped token that owns nothing makes a read-only token for a
  monitoring system (decision 46). An unauthenticated endpoint would need its own listener, and its own decision.
- **Routes are labels by pattern,** such as `/v1/volumes/{volume}`, never by the name asked for, so a cluster creating
  thousands of volumes doesn't make thousands of series.
- **Pool figures are read at each scrape,** as `GET /v1/pools` does, rather than cached: at a 30-second interval that's one
  `lvs` per pool, and the numbers are never stale.
- **Rejected, for now: pushing OTLP.** With the OpenTelemetry Go SDK it's a large new dependency (decision 40); by hand, OTLP
  over HTTP is possible with the standard library, but more to get right than a first version needs. In `TODO.md`.
- **Not yet:** connections, which on SPDK cost an RPC per subsystem (decision 43), reconcile timings, and the CSI driver's
  own metrics beyond its sidecars'.

## 48. The CSI driver stages ReadOnlyMany read-only, and never hands a node a wildcard address

- **The problem, found by the third outside review:** a ReadOnlyMany filesystem was staged read-write on every node that used
  it, and only each pod's bind mount was read-only. Several nodes with the same ext4 or XFS mounted read-write corrupt it,
  through their journals and superblocks, even when no pod writes. Separately, a port listening on `0.0.0.0` or `::` put
  that address in the publish context, and a node told to connect there reached itself: the volume never appeared, and
  the failure looked like a missing device.
- **Chosen: ReadOnlyMany filesystems are staged with `ro`,** which replaces any `rw` or `ro` from the StorageClass. An
  empty device is still formatted first, by the first node to stage it, so a new volume can be used. A node that finds
  the staging path already mounted keeps that mount only if it's read-only; a read-write mount there is
  `FailedPrecondition`. Raw block volumes and ReadWriteOnce are unchanged.
- **Left: two nodes formatting a blank ReadOnlyMany volume at once.** Both can see an empty device and both run `mkfs`. A
  ReadOnlyMany volume is normally filled from a snapshot or a clone, which is never blank, so this needs a pod scheduled on
  two nodes at the same moment for a volume that has nothing to read anyway.
- **Also left: journal replay.** Mounting ext4 or XFS read-only still replays a dirty journal, which writes. The driver's
  own volumes are unmounted cleanly; one that wasn't would need `noload` or `norecovery`, which show an inconsistent
  filesystem, so the driver doesn't add them.
- **Chosen: a port whose address is empty, `0.0.0.0` or `::` is refused when publishing,** with `FailedPrecondition`
  naming the port, before any host, subsystem or namespace is made. The node checks again before connecting, for a
  publish context made before this. The target may still listen on a wildcard, for clients that connect to a real address
  themselves; such a port just can't be a StorageClass's `port`.

## 49. Mapping a volume compares it only with what was exported by path

- **The problem, found at scale:** with 5,000 LVM volumes, exporting one took about 0.2 s of the API's lock, so the CSI
  driver published only 3 to 5 volumes a second. `checkUnshared` resolved and looked up every volume, snapshot and
  namespace on each export: a symlink walk, a `stat` and two sysfs lookups apiece, 300,000 syscalls in 10 seconds. On
  sparse files, where the earlier scale work measured, each is one cheap `stat`, so it hadn't shown.
- **Chosen:** when the path being exported is a volume's, compare it only with namespaces exported by path (a device or
  a file, which an admin named) and with passthrough controllers' namespaces. Each volume and snapshot is a device or file
  its pool made for it alone, by an internal name, and a namespace of a volume is that volume's own path; none can reach
  another. Exporting by path still compares with everything, by what it reaches, as before.
- **Kept:** a volume's file hard-linked, or its device's disk or partition, exported by path, is still a conflict, in
  either order.

## 50. Changed blocks come from the storage's own mappings, checked against the data first

- **The problem:** CSI's `SnapshotMetadata` service lets backup tools read only what changed between two snapshots, or what a
  snapshot holds. A missed block makes every incremental backup built on it silently wrong, so the method has to be proven
  before anything is built on it.
- **Chosen: on LVM thin, `thin_delta` and `thin_dump`,** reading a metadata snapshot reserved on the live pool
  (`reserve_metadata_snap`), as the pool can't be taken offline. Only one can be reserved at a time, so one request at a time
  per pool. Ranges come in the pool's chunk size. They're in `device-mapper-persistent-data`, which hosts already need for
  `thin_check`, so nothing new is required.
- **Chosen: on reflink, FIEMAP** of the two snapshot files: a different physical extent, or written on one side only, is a
  change. Unwritten extents count as holes, as they read as zeros. Allocated ranges are the written extents.
- **Checked against the data:** `test/vm/cbt-check.sh` compares every byte of the snapshots with what's reported, on LVM at
  four chunk sizes and on XFS, with negative controls so that it can't pass by mistake. Nothing was missed.
- **Accepted: over-reporting.** LVM reports whole chunks, so scattered small writes on a pool with large chunks report nearly
  everything. That's correct, only slower to back up; `docs/pools.md` should say so.
- **Rejected: tracking writes in nvweaver,** as it isn't in the data path, and **comparing data,** which reads both snapshots
  in full, the cost CBT exists to avoid.
- **The API:** `GET /v1/snapshots/{snapshot}/allocated`, and `/changes?base=` for two snapshots of the same volume, in either
  order. Both are read-only, so open to scoped tokens, as all reads are (decision 46). Ranges are bytes, in whole blocks of
  `block_size`, merged where they touch, cut at the snapshot's size, and paged by `offset`, as CSI's `starting_offset` is.
  Each page asks the storage again: simple, and a backup reads the ranges once.
- **Refused: snapshots of different volumes.** A pool's mappings could compare any two devices, but only snapshots of one
  volume share a history, and a delta between unrelated volumes would be nearly everything.
- **A metadata snapshot left reserved,** by a service that stopped in the middle, is released and taken again, as nothing
  else should hold one on nvweaver's pools. Another tool that does would have it taken away.
- **Where FIEMAP can't say where data is,** for delayed allocation, inline or encoded data, including btrfs's compressed
  extents, that range counts as changed: never under-reporting matters more than reporting little.
- **The CSI driver serves CSI's `SnapshotMetadata` service,** only with `--snapshot-metadata`, and advertises it only then. It
  streams nvweaver's pages as variable-length ranges, with CSI's codes: `NotFound`, `InvalidArgument` for snapshots of
  different volumes, and `OutOfRange` past the end. The external-snapshot-metadata sidecar fronts it for backup tools, with
  TLS and Kubernetes tokens, and needs 1.33, so the Helm chart adds it only with `snapshotMetadata.enabled`, and refuses
  below 1.33 or without its CRD (decision 51). Not in the manifests yet.
- **The chart makes the sidecar's certificate and CA,** valid for 10 years and kept on upgrades, as clients hold the CA from
  the `SnapshotMetadataService`; or takes an existing secret and its CA. The audience defaults to the driver's name.

## 51. The CSI driver supports Kubernetes 1.31 and later

- **Asked for by the user:** 1.31, which OpenShift 4.18 is based on, as the oldest.
- **The driver itself doesn't limit it.** It vendors CSI spec v1.13, but the spec only adds between minor versions, and
  Kubernetes never checks which one a driver was built with: the kubelet and the sidecars call what they know.
- **The sidecars do.** `csi-provisioner` from v6 and `csi-resizer` from v2 need 1.34, for VolumeAttributesClass `v1`, which the
  driver doesn't implement. **Chosen: below 1.34, their last releases before that,** v5.3.0 and v1.14.0. The Helm chart picks
  them by the cluster's version (`imageBefore134`), and the manifests' instructions say which two lines to change. The other
  sidecars' minimums are 1.25 or lower.
- **Accepted: those two lines aren't updated any more,** so older clusters run sidecars from June 2025. Rejected: the older
  ones everywhere, which would give up their fixes on current clusters too.
- **Changed block tracking needs 1.33,** the `external-snapshot-metadata` sidecar's minimum, so it's optional: off unless asked
  for, and refused below 1.33.
- **Tested:** the whole CSI suite and `csi-sanity` on k3s 1.31.14 and 1.37.2, and OpenShift 4.22 (1.35). Not yet on
  OpenShift 4.18 itself, or 1.32 and 1.33.

## 52. Every Go build uses one exact Go release, bumped by hand

- **The problem:** images were built from `golang:1.26-alpine` and the tarball by CI's `setup-go` with `1.26.x`. When Go
  1.26.11 came out, the image had it the same day, while `setup-go`'s list of releases still stopped at 1.26.10, so a pull
  request's images and binaries were built with different Go, and the SBOM check refused them, as it's meant to (decision
  on releases in `docs/releasing.md`). Nothing in the pull request had changed.
- **Chosen, by the user: one exact release everywhere,** `1.26.10` for now: the three Containerfiles, CI's `setup-go`, and
  the test images and tools. A new Go release becomes a deliberate change, made once `setup-go` lists it, in all of them at
  once, and tested like any other.
- **Accepted:** a Go patch release, which can carry security fixes, isn't used until someone bumps it. Check for one at
  each release.
- **Rejected: following the newest automatically,** which is what broke, as the two sources update at different times.

## 53. Read-only filesystems get one SELinux label for the whole mount

- **The problem, found on OpenShift:** decision 48 mounts ReadOnlyMany filesystems read-only on every node. Where SELinux labels
  files, the container runtime relabels a volume for each pod's label when the pod starts, and a read-only filesystem
  refuses: `relabel failed ... lsetxattr`, and the pod never starts. The k3s lab didn't see it, as its runtime doesn't relabel.
- **Chosen:** where the filesystem holding the staging path has `seclabel`, which is how kubelet itself decides whether to
  relabel, a ReadOnlyMany filesystem is mounted with `context=system_u:object_r:container_file_t:s0`. Every file then has that
  label, which every container's level dominates, so any pod using the volume may read it; the mount has no `seclabel`, so
  kubelet relabels nothing. Without `seclabel`, nothing changes. A class that gives its own `context=` keeps it.
- **Accepted:** the label doesn't keep pods' categories apart on that volume. It's read-only, and only pods that use it have it
  mounted, as with any read-only shared filesystem, such as NFS's `nfs_t`.
- **Rejected for now: `seLinuxMount: true` on the CSIDriver,** which has kubelet mount with each pod's own label. It isn't in
  every Kubernetes the driver supports (decision 51), and is in `TODO.md` with the driver's other SELinux work.

## 54. Hosts are owned and labelled, and the driver forgets nodes that have left

- **The problem:** the driver keeps each node's subsystem and host for its next volume, so they stay after the node has left
  the cluster, and a scoped token couldn't delete a host, as hosts had no owner (decision 46). And the user, browsing nvweaver,
  couldn't tell which host was which node of which cluster: a host held only its NQN and keys.
- **Chosen: hosts have an `owner` and `labels`,** as subsystems and volumes do. A host a scoped token creates is its own, and
  it may change that host's labels and delete it; DH-HMAC-CHAP keys stay an admin's. An admin may set any host's owner, which
  is how hosts made before this are given to a cluster.
- **Chosen: the driver labels each node's host** with `csi-cluster` and `csi-node`, the Kubernetes node's name, which it finds
  in the node's CSINode: its driver entry's node ID is the host NQN.
- **Chosen: the controller forgets a node that has left,** checking every 5 minutes, or as `--node-cleanup-interval` says, 0
  to stop it. A node of this cluster's, from its subsystem's name, whose host NQN is in no CSINode for two checks in a row,
  and whose subsystem has nothing exported, loses its subsystem, then its host, if the driver owns it. A rebooting node keeps
  its CSINode, so it's never touched; a node removed with volumes still exported in it is logged and left, as Kubernetes
  unpublishes those, and the next check finds it empty.
- **Kept safe:** a check that can't read CSINodes, or finds none of the driver's, does nothing, as an empty answer is more
  likely a broken one than a cluster with no nodes. The driver reads CSINodes with the standard library, with its service
  account's token, so there's no Kubernetes client to add (decision 40).

## 55. Changed block tracking holds what it reads, and fails on what it doesn't understand

From the fourth outside review. Decision 50's choice to over-report is unchanged. These are about an answer that is too small,
which is worse: every incremental backup built on it is wrong, and nothing says so.

- **The problem:** on LVM, the thin IDs were read before the metadata snapshot was reserved, and the API held neither snapshot
  while the storage was read. Device-mapper reuses a deleted device's ID, so a snapshot deleted in between, with another
  taking its ID, gave the other's mappings, and an empty answer looked like "nothing changed". The XML parser also skipped
  any element it didn't know, so a new kind of difference would have been dropped quietly. And `thin_dump --dev-id` for a
  device that isn't there prints an empty superblock and exits 0, which read as "nothing allocated".
- **Chosen: the API holds the snapshots while reading,** with the shared reservation that `removeSnapshot`'s exclusive
  one clashes with, so a delete meanwhile is a 503, which clients retry. It's released before the answer is written. Each page
  is a separate request, so a delete between pages is allowed: the next page then finds no snapshot, a 404, not another's.
  **Left open:** a snapshot deleted between pages and made again under the same name would be read as the new one. The CSI
  driver's snapshot names are unique, so it can't happen there; an API client that reuses names could stitch two together.
  A page token naming the internal name would close it, and is in `TODO.md`.
- **Chosen: on LVM, each thin ID is read after the metadata snapshot is reserved.** LVs' names are random and never reused
  (`NewInternalName`), so a name that's there after reserving was there when the metadata was frozen, with the same ID. A
  second lookup before reserving, to compare, would add nothing.
- **Chosen: the parser accepts only what 1.3.4 prints for nvweaver's flags.** One `<diff>` with `same`, `different`,
  `left_only` and `right_only`, or one `<device>`, the one asked for, with `range_mapping` and `single_mapping`. Anything
  else is an error that names the element. `<def>` and `<ref>`, for shared mappings, appear only without `--dev-id`, which
  nvweaver always passes. That was checked on metadata with a shared subtree, made with `thin_restore`.
