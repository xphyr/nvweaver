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
  | `cmd/nvweaver`, `internal/client` | Unit tests and `e2e.sh` |
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

