# Project TODO

The goal is to test locally what an enterprise NVMe/TCP array would do, without the
investment in one. So the Kubernetes CSI driver must handle production-like scale, not
just a handful of lab volumes.

Work through these in order: each phase depends on the ones before it.

## 1. Build and release

- [x] Create a GitHub workflow for building and releasing code (`docs/releasing.md`)
  - [x] Lint and unit tests on every push
  - [x] End-to-end suites in VMs on every push
  - [x] Publish binaries, the kernel and SPDK images, checksums and provenance
  - [x] Cache the SPDK build
- [x] Prove CI on GitHub
- [x] Prove the release workflow, with the `v0.1.0-rc1` and `v0.1.0-rc2` pre-releases
- [x] Prove CI on a pull request, in #2
- [ ] Prove CI on a pull request from a fork: it reads the SPDK cache without logging in
- [x] Make the first release: `v0.1.0`, on x86-64
- [x] Install from a release tarball on a clean machine, using `v0.1.0` on fresh VMs
- [x] Prove the Quadlet in the release tarball against a published image, using `v0.2.0-rc2` on x86-64 and a real CM4
- [x] Publish cross-platform releases for x86-64 and arm64, starting with v0.2.0. See decision 27
  - [x] Probe it: `e2e.sh` passes on both targets on a real Arm kernel. See `docs/testing.md`
  - [x] Choose the SPDK baseline by architecture: `Containerfile.spdk` already does
  - [x] Add an Arm job to CI that runs the unit tests and `e2e.sh` on both targets
  - [x] For v0.2.0: publish multi-arch images and an arm64 tarball, built natively on Arm runners. Proved with
        `v0.2.0-rc2`. Moving `latest` first runs on GitHub at `v0.2.0`
  - [x] Pull a published arm64 image and run it on real Arm, using `v0.2.0-rc2` on a CM4
  - [x] For v0.2.0: say in the README what's supported on Arm. See decision 27
  - [x] Check that `armv8-a+crc` runs on older Arm CPUs: Cortex-A72, emulated and on a real CM4
  - [x] Give the SPDK Quadlet a longer Podman stop timeout than its 10-second default, below `TimeoutStopSec`. A slow
        shutdown was killed and reported as failed. Reproduced and fixed on a CM4: see `docs/testing.md`
  - [ ] `test/systemd-e2e.sh` has a fixed `-l 30`, which doesn't scale with `TEST_TIMEOUT_SCALE`
  - [ ] In `e2e.sh`, stop waiting for a reconnect once the host has removed its controller
  - [ ] Arm runners have no KVM, so CI runs `e2e.sh` directly. Decide whether Arm needs the VM suites
- [x] For v0.3.0: publish an SBOM with each release, so that consumers know what's in it. Generate SPDX JSON in the release workflow with a pinned tool such as Syft, from the
      tested images and tarballs, for both architectures. Attach them to the GitHub release, and attest them, as with provenance
  - [x] Check whether a scanner sees SPDK, DPDK and ISA-L inside `nvmf_tgt`. Syft 1.57 doesn't, so their exact versions are recorded at build time and added
  - [x] Have CI fail if the SBOM doesn't list what's in `THIRD_PARTY_NOTICES.md`, so that this can't quietly go stale
- [x] For v0.3.0: publish the `nvweaver` command line utility as its own artifact, as `nvweaver-cli-<version>-linux-<arch>`, for people who only want the client. Linux on amd64 and arm64,
      built natively, as the suites and the Arm runner already test it. Include them in the checksums, attestations and SBOM, and say in the README how to get them
- [ ] Pin base images in the Containerfiles by digest, with automated updates
- [x] Add a `LICENSE` (MIT)
- [x] Include third-party license notices in the images (`THIRD_PARTY_NOTICES.md`)

## 2. Storage layer, spec first

The API only exports devices it is given today. This adds a layer that creates and
destroys storage, separate from the kernel and SPDK target backends.

Storage brings the biggest API changes, so design them in the spec first, and build the client
and CLI once, against the result.

- [x] Write an OpenAPI spec for the API as it stands today (`api/openapi.yaml`)
- [x] Agree the storage API: `docs/storage-api.md`
- [x] Move the storage API into the spec, as it's built
- [x] Write `docs/pools.md`: how to create a suitable pool of each type
- [x] Check automatically that the spec and the implementation stay in sync. Routes and field names
      are checked, not types or status codes
- [x] Benchmark the current API early, to give the design real numbers: see `docs/testing.md`
- [x] Replace the full reconcile with an incremental one, so that the target of 5,000 volumes across 100 node subsystems is
      reachable. Not batched, as measurements didn't justify it: see decision 29
- [ ] Try sending SPDK's RPCs at startup without waiting for each answer. Starting takes about 26 ms per volume, and 310 s with a
      subsystem for each of 5,000 volumes
- [ ] Find out why every SPDK RPC takes about 8 ms, and how long `nvmf_tgt` takes to exit at 5,000 volumes if it isn't killed
- [ ] Test the systemd timeouts at 5,000 volumes, under systemd
- [x] Add an intent log for pool operations, and internal names for volumes
- [x] Make removing a host force its connections off on the kernel target, and test the effect on other hosts: see decision 30
- [x] Fix SPDK's slow list, which fetched its entire configuration for each namespace
- [ ] Report to SPDK that AIO bdevs ignore FUA while reporting a write cache. See decision 28. An upstream fix would let us drop
      the sync attribute, and also cover block devices
- [x] Check `fs.aio-max-nr` on SPDK at startup, and give a clear error. Each volume needs 128 per core
- [x] Handle long-running operations such as full clones, without blocking the API behind its
      single lock
- [x] Add support for an LVM backend, using thin provisioning only. Adopt existing pools only. In a container, use
      the host's own `lvm`, and test that together with a shared `/dev`
  - [x] Enable snapshots and cloning of NVMe devices
  - [x] Give every clone a new UUID and NGUID, or hosts reject it as a duplicate
  - [x] Activate thin snapshots explicitly, because they skip activation by default. Clones are active, and snapshots stay
        inactive until they're cloned
  - [x] Report thin pool usage through the API: a full pool causes I/O errors for hosts
  - [x] Test what happens when a thin pool really fills up, for both hosts and the API: see `docs/testing.md`
  - [x] Decide how this runs in containers: through the host's `lvm`, with `--pid=host`. See `docs/storage-api.md`
  - [x] Document that snapshots are crash-consistent only
- [x] Add reflink file pools, on XFS or btrfs, which need no LVM. Ext4 doesn't support reflink
- [x] Add the volume model: volumes own their identity, and map into subsystems
- [x] Add snapshots and clones, on reflink. Checked as real files on XFS, including under load: see `docs/testing.md`
- [ ] Leave room in the API for group snapshots, without building them yet
- [x] Add `transport` to ports, accepting only `tcp` for now, and `capabilities.transports`, before the CLI and CSI driver are written against the API. See "Transports" in `docs/storage-api.md`
- [ ] Later: creating pools from raw disks, behind an explicit confirmation

## Test suite

- [ ] Add path filters to pull requests, using the table in decision 39, keeping the full run on `main` and releases
- [ ] Audit the suites, once the web interface is done. Time each section, then remove candidates only if deliberately introduced bugs are still caught. `e2e.sh` and `vm-e2e.sh` overlap, but may fail for different reasons, and `e2e.sh` is all that runs on Arm
- [ ] Check "Escaped a scoped run" in `docs/testing.md` at each release, and revisit decision 39 as it says

## 3. API foundations

- [x] Deal with LVM that clients create inside their namespaces, before the CSI driver, as Kubernetes nodes may well use LVM. Only the machine's own LVM
      configuration can prevent it, so nvweaver reports it under `warnings`, and documents the fix. See decision 32 and "Clients' LVM" in `docs/pools.md`
- [x] Add TLS to the API, needed before the web UI or CSI driver use it over a network. See decision 31
- [ ] By 1.0: make serving the API in clear text off loopback an explicit choice by the admin deploying it, rather than a warning. See decision 31. `NVWEAVER_ALLOW_INSECURE_LOGIN` already works this way for passwords, so it may become one setting
- [x] Check at startup that the tools in "Tools" in the README are there. See decision 34
- [x] Create a shared Go API client, for the CLI and CSI driver to use. See decision 35
- [x] Add an endpoint showing connected hosts, including each connection's transport. See decision 41
- [ ] Say which host is connected on kernels built with `CONFIG_NVME_TARGET_DEBUGFS`, from `/sys/kernel/debug/nvmet`. Fedora's isn't, so it can't be tried in the VMs without building a kernel
- [x] Check `info.version` in the API spec in the release workflow: `v0.2.0` shipped saying `0.1.0`
- [ ] Add an endpoint listing available block devices. Pools are adopted for now, so nothing needs it yet

## 4. Command line utility

- [x] Create a command line utility for CRUD actions, using the shared client

## 5. Scale

Production-like scale is a requirement, so measure this before building on it.

- [x] Benchmark the API with thousands of subsystems, namespaces, hosts and volumes
- [x] Replace the full reconcile and single lock. The whole-state save stays, as it no longer limits scale: see decision 29
- [ ] Add pagination to list endpoints: subsystems, hosts and ports still come as one list
- [ ] Add server-side search to the volume and snapshot lists. The web interface's filter only sees what's loaded so far, and its picker for adding a volume reads every volume, which won't do at 5,000
- [ ] Measure target limits with many connections: each connection opens roughly one TCP
      connection per CPU, so per-volume subsystems add up quickly

## 6. Web UI

**Next, by choice,** ahead of what's left of phases 3 and 5, so that there's something to see.

- [x] Read-only screens, embedded in the binary, tested in a real browser. See decision 37
- [x] Sign in with a username and password, from a users file in bcrypt. See decision 40
- [x] Try signing in over real TLS, in a browser. It has a real certificate that the browser is told to accept, as trusting a CA isn't what's being tested
- [x] Create, grow, clone and delete volumes, take, clone and delete snapshots, and export and un-export volumes from their own pages and from subsystems
- [x] Show tables a page at a time, 10 rows by default
- [x] Create and delete subsystems, hosts and ports, including who's allowed and which ports, and namespaces on raw devices and files
- [x] Adopt and forget pools
- [x] Set DH-HMAC-CHAP keys on hosts. It doesn't set their hash or DH group, or labels, a namespace's UUID, or enabling and disabling a namespace, which are in the CLI
- [x] Show which hosts are connected on a subsystem's page, and before un-exporting or deleting it
- [x] Run a browser against both real targets in VMs: `test/webui-real-e2e.sh`

## Review findings, before the CSI driver

From an outside review, checked against the code after v0.3.0-rc1. All are real, and none are regressions. The details and suggested tests are in `CLAUDE.review.md`, which isn't in the repository. In this order, each with a test that fails first:

- [ ] The same disk can be exported under two paths: compare device and inode, `Rdev`, and a whole disk with its partitions, when a volume is mapped too
- [ ] Passthrough doesn't check that a controller is free. Under native multipath, sysfs may list `nvme0c0n1`, not the `nvme0n1` in use, so a simple lookup could pass a busy controller: test this on emulated NVMe
- [ ] A volume's grow can drop its intent if saving fails, or never rescan if the rescan fails
- [ ] A host change on SPDK fetches every subsystem, even adding an unrelated host: about 40 s under the API lock at 5,000 subsystems
- [ ] Managed `file` namespaces aren't in the intent log. Not decided: what happens to the file when a namespace moves to another `device_path`. An automatic delete would lose data silently, so refusing that change is the recommendation
- [ ] Volume and snapshot `state` is always `online`: report `missing`, in the spec first, and show it in the interface
- [ ] After one timeout, every SPDK RPC fails until a restart, as `json.Decoder` keeps its read error. Dial again. `bdevExists` should return errors, but `ENODEV` still has to mean that it isn't there
- [ ] Changing a host's DH-CHAP key leaves its session connected on the kernel target. Not decided: dropping it stalls other hosts on that subsystem for about 10 s, and a host without the new key can't reconnect. Dropping it is the recommendation, with the order documented
- [ ] A port update drops foreign subsystems: link them back afterwards, including if writing an attribute fails
- [ ] Errors from writing to configfs quote the value, including DH-CHAP secrets
- [ ] Smaller: compare labels on an idempotent create, compare resolved paths for reflink pools, name 503 and 507 in the README's error list, and tick "Make volume creation idempotent by name" if that's still open

## 7. Kubernetes CSI driver

- [ ] Create a Kubernetes CSI driver that works with the API to handle dynamic
      provisioning
  - [ ] Controller, and a privileged node plugin (`nvme connect`, format, mount), which takes the transport
        from the port rather than assuming TCP
  - [ ] Make volume creation idempotent by name in the API, because CSI retries calls
  - [ ] Use the snapshot and cloning features
  - [ ] Map volumes into per-node subsystems, and each shared volume into its own subsystem
  - [ ] Test RWX block volumes and KubeVirt live migration with the real thing. On the kernel target, removing the source host
        stalls the destination for about 10 s: see decision 30. A shorter reconnect delay may help, and hasn't been measured
  - [ ] Scope names per cluster, so that several clusters can share one target
  - [ ] Detect nodes sharing a `hostnqn`, common on cloned images, which breaks access
        control
  - [ ] Limit queue counts per connection, based on the phase 5 measurements
  - [ ] Pass the `csi-sanity` conformance suite
  - [ ] Test at production-like volume and node counts
  - [ ] Document that there is a single target, with no failover
  - [ ] Test a clone of a volume with LVM inside it, on a node that also has the original. Its PV UUID and VG name match its source's, so the node's LVM should
        see duplicates. Nothing has tested this yet. Real arrays have the same problem, and the target can't fix it without changing the client's data. Find
        out what happens to pods on both volumes, then either document it or have the node plugin handle it, as with `vgimportclone`
## Later (low priority)

- [ ] Publish the `nvweaver` command line utility for macOS, on arm64 and amd64, and Windows on amd64. Only the client: the service stays Linux only. Name them as the Linux ones are, and include them in
      the checksums, attestations and SBOM
  - [ ] Add a smoke test on hosted macOS and Windows runners, running the real binary against a fake API, as nothing has run there yet. Don't publish those builds without it. All these targets
        cross-compile, but `go test ./cmd/nvweaver` won't there, as its tests use the Linux-only `internal/target`
  - [ ] Check what differs there: file paths for `--token-file` and `--ca`, the system's certificates, and signals on Windows
- [ ] iSCSI as a second protocol, once the CLI and CSI driver have settled the API. It would be new resources, such as `/v1/iscsi/targets`, not a
      `transport` on ports. Pools, volumes and snapshots should carry over: see decision 33
  - [x] Probe whether it's really feasible, driving LIO and SPDK by hand in VMs: it is, on both. See "iSCSI: is it feasible?" in `docs/testing.md`
  - [ ] Decide how to handle a volume's identity differing between LIO and SPDK, which can't be made to match
  - [ ] Have `mapped_to` cover both protocols, so that a volume is only ever exported once: nothing below stops two
  - [ ] Try what the probe didn't: CHAP, multipath, growing on SPDK, a direct login by a stranger on SPDK, `spdk_tgt` in place of `iscsi_tgt`, Arm, and more power-cut rounds
  - [ ] Weigh the cost of four combinations of target and protocol in the test suite
- [ ] Test the suites against Docker. Podman is the supported engine and Docker a fallback,
      and nothing has been run against Docker since Podman became the default
- [ ] Support NVMe/RDMA, after the CSI driver. Both targets support it underneath. See decision 26
  - [ ] Test first whether SPDK's RDMA transport works with `--no-huge` and interrupt mode, as that decides how much
        else changes
  - [ ] Kernel target: set `addr_trtype` from the port, and load `nvmet-rdma`
  - [ ] SPDK: build with RDMA instead of `--without-rdma`, and create the transport and listeners from the port
  - [ ] Containers: `/dev/infiniband` and a higher memory-lock limit
  - [ ] Run the suites over Soft-RoCE (`rdma_rxe`) in the VMs, which proves the protocol but not the performance
  - [ ] Real RDMA NICs, added to the deferred hardware tests once they're available

## Enterprise features to consider

To decide later: these matter if tests should match enterprise array behaviour closely.

- [ ] ANA groups, and multipath failover tests
- [ ] NVMe/TCP TLS
- [ ] Other initiators than Linux, such as VMware and Windows

## Testing without hardware

No physical NVMe hardware is available right now, so development continues on emulation.
The tests so far use one machine over loopback, with files, loop devices, `null_blk` and
network-attached NVMe namespaces.

- [x] Run all suites on rootful Podman
- [x] Add a QEMU environment with emulated NVMe drives and separate target and initiator
      VMs (`test/vm/lab.sh`). It emulates the hardware, so it does not replace the tests below
- [x] Use the VMs to test passthrough against a PCIe-style controller (`test/vm/vm-e2e.sh`)
- [x] Use the VMs to test with a separate initiator
- [x] Use the VMs to test the systemd setup for real: `make install` as root, enable, reboot
      the VM, SELinux enforcing, and the Quadlet on the VM's own Podman
- [ ] Run benchmarks between the VMs and over the 1 Gb network, on both backends
- [ ] Long term: run the suites on a Debian VM too. All VM testing so far is on Fedora, and Debian differs where it matters, such as
      LVM having no devices file by default

### Deferred until hardware is available

- [ ] Test block device and passthrough namespaces on a physical NVMe drive
- [ ] Test with a separate physical initiator over a real network. A network test from another
      machine to a target on the lab host needs no new hardware, but is also not done yet
- [ ] Measure real performance on both backends. Current numbers are from a loaded VM over
      loopback
- [ ] Test a real power cut on SPDK with a block device, on a drive with a volatile write cache. SPDK ignores FUA, which
      QEMU's disks can't show the effect of. See decision 28
- [ ] Install on a real host and reboot, to prove the boot ordering. So far systemd has
      only been tested in a container
- [x] Test on a host that really has no `nvmet-tcp`: a Raspberry Pi CM4, where SPDK took over. See `docs/testing.md`
- [ ] Finish testing on real Raspberry Pis, on a 64-bit OS:
  - [x] CM4 (Cortex-A72, no AES or SHA) with SPDK, over a real network: 43 of 43. See `docs/testing.md`
  - [ ] Add that network test to the repository: it needs an `e2e.sh` that can use a remote initiator
  - [x] Run the SPDK Quadlet on the CM4
  - [ ] Run the plain systemd unit on the CM4
  - [ ] Pi 5 (Cortex-A76): Raspberry Pi OS defaults to a 16K-page kernel, while our VMs use 4K, which
        may matter to SPDK and DPDK
  - [ ] The kernel target on a Pi: Raspberry Pi OS's kernel has `NVME_TCP` and `NVME_TARGET` off, so it needs a different
        kernel or a module build. Only worth doing if the kernel target should be supported on Pis
  - [ ] Real storage: USB disks on a Pi 4, and PCIe NVMe on a CM4 carrier board or a Pi 5
