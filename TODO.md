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
- [ ] Install from a release tarball on a clean machine. The suites install from source
- [ ] Publish cross-platform releases for x86-64 and arm64. This is a goal, but not started: x86-64
      comes first. Keep new work portable until then
  - [ ] Go binary and kernel image, tested on Arm before publishing
  - [ ] SPDK image: choose an Arm baseline for `SPDK_ARCH`
  - [ ] VM lab, or direct runner tests if Arm runners have no KVM
- [ ] Pin base images in the Containerfiles by digest, with automated updates
- [x] Add a `LICENSE` (MIT)
- [x] Include third-party license notices in the images (`THIRD_PARTY_NOTICES.md`)

## 2. Storage layer, spec first

The API only exports devices it is given today. This adds a layer that creates and
destroys storage, separate from the kernel and SPDK target backends.

Storage brings the biggest API changes, so design them in the spec first, and build the client
and CLI once, against the result.

- [x] Write an OpenAPI spec for the API as it stands today (`api/openapi.yaml`)
- [ ] Design pools, volumes, snapshots and clones in the spec, as in `docs/decisions.md` 19 to 24
- [ ] Write `docs/pools.md`: how to create a suitable pool of each type
- [x] Check automatically that the spec and the implementation stay in sync. Routes and field names
      are checked, not types or status codes
- [ ] Add an endpoint listing available block devices, for choosing what goes in a pool
- [ ] Benchmark the current API early, to give the design real numbers. See phase 5
- [ ] Move UUID and NGUID from the namespace to the volume, with a migration for saved state
- [ ] Replace the full reconcile with an incremental one, batched per subsystem, so that the target
      of 5,000 volumes across 100 node subsystems is reachable
- [ ] Add an intent log for pool operations, and internal names for volumes
- [ ] Fix SPDK's slow list: each namespace's status check fetches its entire configuration
- [ ] Check `fs.aio-max-nr` on SPDK at startup, and give a clear error. Each volume needs 128 per core
- [ ] Handle long-running operations such as full clones, without blocking the API behind its
      single lock
- [ ] Add support for an LVM backend, using thin provisioning only. Adopt existing pools only
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
- [ ] Add reflink file pools, on XFS or btrfs, which need no LVM. Ext4 doesn't support reflink
- [ ] Leave room in the API for group snapshots, without building them yet
- [ ] Later: creating pools from raw disks, behind an explicit confirmation

## 3. API foundations

- [ ] Add TLS to the API, needed before the web UI or CSI driver use it over a network
- [ ] Create a shared Go API client from the spec, for the CLI and CSI driver to use
- [ ] Add an endpoint showing connected hosts

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
  - [ ] Controller, and a privileged node plugin (`nvme connect`, format, mount)
  - [ ] Make volume creation idempotent by name in the API, because CSI retries calls
  - [ ] Use the snapshot and cloning features
  - [ ] Map volumes into per-node subsystems, and shared volumes into a shared one
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
- [ ] Test on a host that really has no `nvmet-tcp`, which has only been simulated
