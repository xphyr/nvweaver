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
- [ ] Prove CI on a pull request, including one from a fork: it reads the SPDK cache without logging in
- [x] Make the first release: `v0.1.0`, on x86-64
- [x] Install from a release tarball on a clean machine, using `v0.1.0` on fresh VMs
- [ ] Prove the Quadlet in the release tarball against a published image, at the next release. The
      `v0.1.0` tarball's Quadlet needs the image tagged by hand, or its `Image=` line changed
- [ ] Publish cross-platform releases for x86-64 and arm64, starting with v0.2.0. See decision 27
  - [x] Probe it: `e2e.sh` passes on both targets on a real Arm kernel. See `docs/testing.md`
  - [x] Choose the SPDK baseline by architecture: `Containerfile.spdk` already does
  - [x] Add an Arm job to CI that runs the unit tests and `e2e.sh` on both targets. It hasn't run yet: it runs on pull
        requests and on `main`
  - [ ] For v0.2.0: publish multi-arch images and an arm64 tarball, built natively on Arm runners
  - [ ] For v0.2.0: say in the README what's supported on Arm. See decision 27
  - [x] Check that `armv8-a+crc` runs on older Arm CPUs: Cortex-A72, emulated and on a real CM4
  - [x] Give the SPDK Quadlet a longer Podman stop timeout than its 10-second default, below `TimeoutStopSec`. A slow
        shutdown was killed and reported as failed. Reproduced and fixed on a CM4: see `docs/testing.md`
  - [ ] `test/systemd-e2e.sh` has a fixed `-l 30`, which doesn't scale with `TEST_TIMEOUT_SCALE`
  - [ ] In `e2e.sh`, stop waiting for a reconnect once the host has removed its controller
  - [ ] Arm runners have no KVM, so CI runs `e2e.sh` directly. Decide whether Arm needs the VM suites
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
- [ ] Move the storage API into the spec, as it's built. Done for pools and volumes
- [x] Write `docs/pools.md`: how to create a suitable pool of each type
- [x] Check automatically that the spec and the implementation stay in sync. Routes and field names
      are checked, not types or status codes
- [x] Benchmark the current API early, to give the design real numbers: see `docs/testing.md`
- [ ] Replace the full reconcile with an incremental one, batched per subsystem, so that the target
      of 5,000 volumes across 100 node subsystems is reachable
- [ ] Add an intent log for pool operations, and internal names for volumes
- [ ] Make removing a host force its connections off on the kernel target, and test the effect on other hosts
- [x] Fix SPDK's slow list, which fetched its entire configuration for each namespace
- [ ] Check `fs.aio-max-nr` on SPDK at startup, and give a clear error. Each volume needs 128 per core
- [ ] Handle long-running operations such as full clones, without blocking the API behind its
      single lock
- [ ] Add support for an LVM backend, using thin provisioning only. Adopt existing pools only. In a container, use
      the host's own `lvm`, and test that together with a shared `/dev`
  - [ ] Enable snapshots and cloning of NVMe devices
  - [ ] Give every clone a new UUID and NGUID, or hosts reject it as a duplicate
  - [ ] Activate thin snapshots explicitly, because they skip activation by default
  - [ ] Report thin pool usage through the API: a full pool causes I/O errors for hosts
  - [ ] Filter LVM devices on the target, so it ignores LVM that clients create inside
        their namespaces
  - [ ] Decide how this runs in containers (udev sync, locking, `/etc/lvm`). Native
        systemd is the better fit, and it needs privileges that the no-capabilities SPDK
        setup doesn't have
  - [ ] Document that snapshots are crash-consistent only
- [x] Add reflink file pools, on XFS or btrfs, which need no LVM. Ext4 doesn't support reflink
- [x] Add the volume model: volumes own their identity, and map into subsystems
- [ ] Add snapshots and clones
- [ ] Leave room in the API for group snapshots, without building them yet
- [ ] Add `transport` to ports, accepting only `tcp` for now, and `capabilities.transports`, before the CLI and
      CSI driver are written against the API. See "Transports" in `docs/storage-api.md`
- [ ] Later: creating pools from raw disks, behind an explicit confirmation

## 3. API foundations

- [ ] Add TLS to the API, needed before the web UI or CSI driver use it over a network
- [ ] Create a shared Go API client from the spec, for the CLI and CSI driver to use
- [ ] Add an endpoint showing connected hosts, including each connection's transport
- [ ] Add an endpoint listing available block devices. Pools are adopted for now, so nothing needs it yet

## 4. Command line utility

- [ ] Create a command line utility for CRUD actions, using the shared client

## 5. Scale

Production-like scale is a requirement, so measure this before building on it.

- [ ] Benchmark the API with thousands of subsystems, namespaces, hosts and volumes
- [ ] Replace the full reconcile, single lock and whole-state save on every change, if
      the benchmark shows they limit scale (likely)
- [ ] Add pagination to list endpoints
- [ ] Measure target limits with many connections: each connection opens roughly one TCP
      connection per CPU, so per-volume subsystems add up quickly

## 6. Web UI

- [ ] Create a web UI for manually handling CRUD tasks, embedded in the binary
  - [ ] The web UI should allow for token auth or simple username/password authentication
  - [ ] Hash passwords, and protect sessions against CSRF
  - [ ] Cover the volumes, snapshots and clones from phase 2

## 7. Kubernetes CSI driver

- [ ] Create a Kubernetes CSI driver that works with the API to handle dynamic
      provisioning
  - [ ] Controller, and a privileged node plugin (`nvme connect`, format, mount), which takes the transport
        from the port rather than assuming TCP
  - [ ] Make volume creation idempotent by name in the API, because CSI retries calls
  - [ ] Use the snapshot and cloning features
  - [ ] Map volumes into per-node subsystems, and each shared volume into its own subsystem
  - [ ] Test RWX block volumes and KubeVirt live migration with the real thing
  - [ ] Scope names per cluster, so that several clusters can share one target
  - [ ] Detect nodes sharing a `hostnqn`, common on cloned images, which breaks access
        control
  - [ ] Limit queue counts per connection, based on the phase 5 measurements
  - [ ] Pass the `csi-sanity` conformance suite
  - [ ] Test at production-like volume and node counts
  - [ ] Document that there is a single target, with no failover

## Later (low priority)

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

### Deferred until hardware is available

- [ ] Test block device and passthrough namespaces on a physical NVMe drive
- [ ] Test with a separate physical initiator over a real network. A network test from another
      machine to a target on the lab host needs no new hardware, but is also not done yet
- [ ] Measure real performance on both backends. Current numbers are from a loaded VM over
      loopback
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
