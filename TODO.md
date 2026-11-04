# Project TODO

The goal is to test locally what an enterprise NVMe/TCP array would do, without the
investment in one. So the Kubernetes CSI driver must handle production-like scale, not
just a handful of lab volumes.

Work through these in order: each phase depends on the ones before it.

## 1. Build and release

- [ ] Create a GitHub workflow for building and releasing code
  - [ ] Unit tests on every push
  - [ ] Publish binaries, and the kernel and SPDK images
  - [ ] Cache the SPDK build, which takes several minutes
  - [ ] Find out whether hosted runners have `nvmet-tcp` and `nvme-tcp`, or whether the
        end-to-end suites need a self-hosted runner
- [x] Add a `LICENSE` (MIT)
- [ ] Include SPDK and DPDK license notices in the published SPDK image

## 2. API foundations

- [ ] Write an OpenAPI spec
- [ ] Add TLS to the API, needed before the web UI or CSI driver use it over a network
- [ ] Create a shared Go API client, for the CLI and CSI driver to use
- [ ] Add an endpoint listing available block devices
- [ ] Add an endpoint showing connected hosts

## 3. Command line utility

- [ ] Create a command line utility for CRUD actions, using the shared client

## 4. Storage layer

The API only exports devices it is given today. This adds a layer that creates and
destroys storage, separate from the kernel and SPDK target backends.

- [ ] Add support for an LVM backend, using thin provisioning only
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
- [ ] Consider reflink clones for file volumes on XFS or btrfs, which would need no LVM

## 5. Scale

Production-like scale is a requirement, so measure this before building on it.

- [ ] Benchmark the API with thousands of subsystems, namespaces and hosts
- [ ] Replace the full reconcile, single lock and whole-state save on every change, if
      the benchmark shows they limit scale (likely)
- [ ] Add pagination to list endpoints
- [ ] Measure target limits with many connections: each connection opens roughly one TCP
      connection per CPU, so per-volume subsystems add up quickly

## 6. Web UI

- [ ] Create a web UI for manually handling CRUD tasks, embedded in the binary
  - [ ] The web UI should allow for token auth or simple username/password authentication
  - [ ] Hash passwords, and protect sessions against CSRF
  - [ ] Cover the volumes, snapshots and clones from phase 4

## 7. Kubernetes CSI driver

- [ ] Create a Kubernetes CSI driver that works with the API to handle dynamic
      provisioning
  - [ ] Controller, and a privileged node plugin (`nvme connect`, format, mount)
  - [ ] Make volume creation idempotent by name in the API, because CSI retries calls
  - [ ] Use the snapshot and cloning features
  - [ ] Detect nodes sharing a `hostnqn`, common on cloned images, which breaks access
        control
  - [ ] Limit queue counts per connection, based on the phase 5 measurements
  - [ ] Pass the `csi-sanity` conformance suite
  - [ ] Test at production-like volume and node counts
  - [ ] Document that there is a single target, with no failover

## Enterprise features to consider

To decide later: these matter if tests should match enterprise array behaviour closely.

- [ ] ANA groups, and multipath failover tests
- [ ] NVMe/TCP TLS
- [ ] Other initiators than Linux, such as VMware and Windows

## Testing without hardware

No physical NVMe hardware is available right now, so development continues on emulation.
The tests so far use one machine over loopback, with files, loop devices, `null_blk` and
network-attached NVMe namespaces.

- [ ] Add a QEMU environment with emulated NVMe drives and separate target and initiator
      VMs. This allows passthrough tests against a PCIe controller and tests over a real
      network path. It emulates the hardware, so it does not replace the tests below

### Deferred until hardware is available

- [ ] Test block device and passthrough namespaces on a physical NVMe drive
- [ ] Test with a separate physical initiator over a real network
- [ ] Measure real performance on both backends. Current numbers are from a loaded VM over
      loopback
- [ ] Install on a real host and reboot, to prove the boot ordering. So far systemd has
      only been tested in a container
- [ ] Test on a host that really has no `nvmet-tcp`, which has only been simulated
