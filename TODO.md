# Project TODO

The goal is to test locally what an enterprise NVMe/TCP array would do, without the
investment in one. So the Kubernetes CSI driver must handle production-like scale, not
just a handful of lab volumes.

The phases below were written in dependency order, and are mostly done. What's next is decided with the user, and listed
here first. Each step points at its items further down.

## Next

Agreed with the user on 2026-11-10. Done since the last list: scale (5,000 volumes on 100 nodes, and 1,000 connections), and
the third outside review's findings. In this order now:

1. **Changed block tracking:** done, for the storage, API, CLI and CSI driver, with the chart's sidecar. See phase 7. Still to
   do: the manifests, and checking whether Velero or Kasten use it yet
2. **Cleaning up after a node leaves:** done. Hosts have owners and labels, the controller labels each node's host with its
   cluster and node, and forgets a node whose CSINode has gone once nothing is exported to it. The web interface shows hosts'
   cluster, node and owner, and the overview lists problems. Decision 54. Still to do: give `nvw`'s existing hosts to its token
3. **Consistency groups:** built on 2026-11-10, decision 56: snapshot groups in the API, CLI and web interface, which pause
   hosts with ANA on both targets, and the CSI driver's VolumeGroupSnapshot, which refuses a group that would pause a node
   without native multipath. Tested on both targets and pool types, and in the k3s lab on 1.37 and 1.31. Still
   to do: the KubeVirt guest-agent freeze test on `nvw`, deleting groups in the web interface, and the CSI driver on
   OpenShift and Arm
4. **The CSI driver's gaps against arrays' drivers,** agreed on 2026-11-10, fitted in around 3: topology and capacity
   reporting, volume health, and SELinux mount labelling. See phase 7
5. **iSCSI as a second protocol,** then every feature that works on NVMe checked on iSCSI too: the API, the CLI, the web
   interface, the CSI driver, snapshots, clones, CBT and consistency groups. See "Later" for what the probe found

Smaller, to fit in: the CSI driver on Arm, on a single-node OpenShift 5.0 cluster on arm64 that the user has added, and then
native Arm VMs there for the end-to-end suites (see "Testing without hardware"); a test that a new version reads the previous release's state; the driver refusing to run when two nodes
share a host NQN; a guest writing while it live-migrates; publishing the Helm chart.

## 1. Build and release

- [ ] Keep the Go version in one file that the Containerfiles (by a build argument) and CI read, so that a bump is one line
      (decision 52). Until then it's in the three Containerfiles, `ci.yml`'s four `setup-go` steps, and the test images
- [ ] Bump Go to 1.26.11 once `setup-go` lists it, everywhere at once, and check for a new patch release before each release
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
- [ ] Pin base images in the Containerfiles by digest, with automated updates, the CSI driver's included
- [ ] Find out why the workflow's token was refused writing to `nvweaver-buildcache` from about 21:50 UTC on 2026-11-09, with the
      repository holding Admin access to it: `denied: permission_denied: write_package`. Writing it is best-effort now, so `main`'s CI
      only warns, and releases don't write it
- [ ] Test that a new version reads the previous release's saved state, before each release. Nothing does yet
- [x] Add a `LICENSE` (MIT)
- [x] Include third-party license notices in the images (`THIRD_PARTY_NOTICES.md`)
- [ ] Add container image scanning prior to image publication

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
- [x] Group snapshots: `/v1/snapshot-groups`, consistent across volumes, as hosts are paused with ANA while they're taken.
      Decision 56
- [x] Add `transport` to ports, accepting only `tcp` for now, and `capabilities.transports`, before the CLI and CSI driver are written against the API. See "Transports" in `docs/storage-api.md`
- [ ] Later: creating pools from raw disks, behind an explicit confirmation

## Test suite

- [ ] `vm-e2e.sh`'s power-loss tests leave their background `lab.sh ssh` loops running on the build host after the VM is reset,
      on dead connections, until killed. Kill them when the test ends
- [ ] Add path filters to pull requests, using the table in decision 39, keeping the full run on `main` and releases
- [x] Audit the suites with deliberately introduced bugs, timing each section. Every suite caught something no other did, so none was removed. Results in `docs/testing.md`
- [ ] Decide whether `vm-e2e.sh` runs its storage-only LVM sections on one backend: about 3 minutes, as bugs in shared code were caught by both copies. Host-visible checks stay on both
- [ ] Find out what SPDK's discovery listeners are for: hosts discovered every subsystem without them on SPDK 26.09. Either test the case that needs them, perhaps a port with no subsystems or an older SPDK, or remove them
- [ ] Find out whether CI's unit tests skip the reflink ones, which `go test` without `-v` doesn't show
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
- [x] Measure target limits with many connections: 1,000 controllers with 4 I/O queues each, about 5,000 TCP connections, to one
      kernel target, cost about 2 MB each with the initiator on the same machine, and the target's connect stayed about 9 ms.
      See "Many connections to one target" in `docs/testing.md`. From many machines, and on SPDK, not yet
- [x] The CSI driver's controller at 5,000 LVM volumes on 100 simulated nodes, against a real nvweaver: `csi/test/scale`. Found
      two slow paths, both fixed: exporting a volume looked up every other volume (decision 49), and every publish made two
      creates that changed nothing. See "The CSI driver at scale" in `docs/testing.md`
- [ ] Each change rewrites the whole state with an fsync: about 50 ms at 5,000 volumes, so the API makes 15 to 20 changes a
      second at that size. Decision 29 kept it, and it's now the floor for publishing, unpublishing and node changes
- [ ] On LVM, every create, snapshot and delete waits for the volume group's lock, and each `lvcreate` takes about 0.4 s of
      it at 5,000 LVs: building 5,000 volumes takes 18 minutes, at 2.5 a second by the end. That's LVM's, but document it, and
      see whether `lvcreate` can skip anything nvweaver doesn't need

## 6. Web UI

Built ahead of what was left of phases 3 and 5, by choice, so that there was something to see.

- [x] **LVM snapshots showed as `missing` when they were only inactive,** as their state came from whether the device node
      existed, and the driver's snapshots are kept inactive (activation-skip). Found by the user browsing a lab server. LVM is
      now asked, once per pool for a listing, at most every 30 seconds
- [x] Show active problems on the overview: pool warnings, and volumes and snapshots that aren't online
- [x] Hosts show the cluster, Kubernetes node and owner the CSI driver gave them, as the user couldn't tell which was which
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

## Review findings

Two outside reviews, after v0.3.0: every finding was real, and all are fixed, each with a test that failed first. See "The
outside review's findings" and "The second pass" in `docs/testing.md`, and decisions 9, 30, 42 and 43. The reviewer's notes
are in `CLAUDE.review.md`, which isn't in the repository.

## 7. Kubernetes CSI driver

- [x] A disposable Kubernetes lab: `test/vm/k3s-lab.sh`, k3s on three VMs and nvweaver on a fourth. See `docs/testing.md`
- [x] Test on OpenShift too, on a three-node virtual cluster with nested VMs for KubeVirt: RHCOS, SCCs and MachineConfig. Three
      virtual nodes don't prove production-like node counts or performance, which stay open below
  - [x] An SCC for the privileged node plugin, and the manifests' paths checked on RHCOS: the privileged SCC, as `csi/README.md` says
  - [x] `nvme-tcp` loaded and a unique `hostnqn` on each node, by MachineConfig if RHCOS doesn't already. `nvme-tcp` was loaded already. RHCOS doesn't: every node
        had the same host NQN and host ID. `csi/deploy/openshift/` has a MachineConfig for each pool, applied to `nvw`'s master pool
  - [x] A CDI StorageProfile, `csi/deploy/openshift/storageprofile.yaml`: a VM and a live migration passed with it
  - [x] A DataImportCron: Fedora's golden image as a snapshot, and VMs cloned from it, once nvweaver's class is KubeVirt's default
  - [ ] A guest writing while it migrates. The stall itself was measured with pods: see the reconnect delay below
- [x] Create a Kubernetes CSI driver that works with the API to handle dynamic
      provisioning: `csi/`, decisions 44 and 45, tested end to end in the k3s lab by `test/vm/k3s-csi.sh`
  - [x] Controller, and a privileged node plugin (`nvme connect`, format, mount), which takes the transport
        from the port rather than assuming TCP
  - [x] Make volume creation idempotent by name in the API, because CSI retries calls
  - [x] Use the snapshot and cloning features
  - [ ] Changed block tracking: CSI's `SnapshotMetadata` service (KEP-3314, beta in Kubernetes 1.36, CSI spec 1.10), for incremental
        backups of block volumes. After snapshots work in the driver. Storage does the work, so both targets get it
    - [x] First, on its own: prove the storage side never under-reports, as a missed block silently corrupts every incremental backup
          built on it. `test/vm/cbt-check.sh`: 72 checks and 66 negative controls passed, on LVM at four chunk sizes and on XFS. See
          "Changed block tracking" in `docs/testing.md` and decision 50
    - [x] LVM thin: `thin_delta` and `thin_dump` on a reserved metadata snapshot, one at a time per pool. Ranges come in the pool's
          chunk size. They're in `device-mapper-persistent-data`, which hosts already need for `thin_check`, and the images use the
          host's LVM tools, so nothing is added to them
    - [x] Reflink: `FIEMAP` on both snapshot files, where a different physical extent or a hole is a change
    - [x] API, spec first: paginated, read-only allocated and changed ranges on snapshots, with each pool's block size:
          `/v1/snapshots/{snapshot}/allocated` and `/changes`, and `nvweaver snapshot allocated` and `changes`. Checked end
          to end by `vm-e2e.sh` against where a host's XFS put its files, on both pool types and targets
    - [x] Support Kubernetes down to 1.31, for OpenShift 4.18: older provisioner and resizer below 1.34, decision 51
    - [x] Driver: optional, off unless asked for, and only from Kubernetes 1.33, the sidecar's minimum. The `SnapshotMetadata` service, the `external-snapshot-metadata` sidecar with its TLS and token audiences, and a
          `SnapshotMetadataService` object, by the Helm chart. Tested end to end in the k3s lab with the sidecar project's example
          client: `k3s-csi.sh cbt`
    - [x] The fourth outside review: snapshots held while they're read, thin IDs read once the metadata is frozen, and thin tools'
          output it doesn't understand, or for a device that isn't there, an error. Decision 55
    - [ ] Pages of `/allocated` and `/changes` tied to the snapshot's internal name, so that one deleted between pages and made
          again under the same name isn't stitched in. The driver's names are unique, so only API clients that reuse names see it
    - [ ] Changed block tracking with the manifests too, not only the Helm chart
    - [ ] Check whether Velero, Kasten or other backup tools use it yet, and try one
    - [ ] Test it on OpenShift, which needs `nvw` on 4.20 (Kubernetes 1.33) or later; it's on 4.22, so it can be tried now
  - [x] Map volumes into per-node subsystems, and each shared volume into its own subsystem
  - [x] Test RWX block volumes and KubeVirt live migration with the real thing: both passed on OpenShift. On the kernel target, removing the source host
        stalls the destination for about 10 s: see decision 30. Measured on OpenShift: the new node's I/O queued for 10 s, 10 s after
        the migration, with no error. A shorter reconnect delay may help, and hasn't been measured
  - [x] Decide the node's reconnect delay (`nvme connect --reconnect-delay`), which sets how long that stall lasts, and measure it:
        2 s, decision 45, measured at 10, 2 and 1
  - [ ] Moving a cluster to another port, such as a storage network: today its StorageClass is recreated and each node's subsystem
        cleared by hand, as nodes keep their live connection. The driver could offer each node's subsystem on the new port, and
        have nodes connect to it, as a second path
  - [x] Scope names per cluster, so that several clusters can share one target: `--cluster`, decision 45
  - [ ] Detect nodes sharing a `hostnqn`, common on cloned images, which breaks access
        control
  - [ ] Limit queue counts per connection, based on the phase 5 measurements. Probably not needed: 1,000 controllers of 5 queues
        each cost about 2 MB apiece, and a node has one per subsystem it uses
  - [x] Run `k3s-csi.sh` against the SPDK target: all 25 passed
  - [x] Survive outages: nodes retry for ever by default (`--ctrl-loss-tmo=-1`), decision 45. Tested with nvweaver stopped for
        90 s, and a node rebooting, with a pod writing
  - [ ] Run the outage test on SPDK, and for longer than the kernel's default 10 minutes, with the default once to show the difference
  - [x] Report `MaxVolumesPerNode`, from the 1024 namespaces a subsystem can hold on both targets, so the scheduler knows
  - [x] Pass the `csi-sanity` conformance suite: 75 of 75, v5.6.0, by `k3s-csi.sh sanity`
  - [x] Publish the driver's image in releases, for amd64 and arm64, with its third-party notices, and run `k3s-csi.sh` in CI or before each release:
        from v0.4.0, with its SBOM; `k3s-csi.sh` runs before each release, as `docs/releasing.md` says, as CI's runners are too small
  - [ ] Run `k3s-csi.sh` in CI, on a larger runner, or self-hosted
  - [x] A Helm chart, as another way to install: `csi/deploy/helm/nvweaver-csi`, linted and rendered by `make lint`, tested on OpenShift
  - [ ] Publish the chart with releases, such as to an OCI registry, and test it in the k3s lab too
  - [ ] DH-HMAC-CHAP for nodes: a key per node, from a Secret, set on its host
  - [x] Delete a node's subsystem and host when the node leaves the cluster, once nothing is exported to it: the controller's
        node janitor, every `--node-cleanup-interval`. Hosts have owners, so a scoped token may delete its own. Decision 54
  - [x] Group snapshots, VolumeGroupSnapshot: optional (`--group-snapshots`, the chart's `groupSnapshots.enabled`), each node
        saying in a Lease whether it can be paused, and the controller refusing a group that would pause one that can't.
        Tested in the k3s lab: `k3s-csi.sh groups`. Decision 56
  - [ ] Group snapshots on OpenShift, `nvw` and `snoarm`, and a KubeVirt VM's disks with its guest agent's freeze
  - [ ] Topology and `GetCapacity`, for clusters where not every node can reach nvweaver, such as one with a storage network,
        and so the scheduler sees a pool that's full. Arrays' drivers all report capacity
  - [x] ReadOnlyMany filesystems on SELinux nodes: found on OpenShift, where pods using them never started, as a read-only
        filesystem can't be relabelled. Mounted with one label now, decision 53
  - [ ] Volume health: `VOLUME_CONDITION` from the node and `GET_VOLUME` from the controller, so a lost path, a full thin pool or a
        missing LV shows on the claim in Kubernetes, not only in nvweaver's warnings
  - [ ] SELinux mount labelling: `seLinuxMount: true` on the CSIDriver, so a volume is mounted with its label rather than every
        file relabelled at each pod start, which is slow on big volumes such as VM disks. Check the node passes the `context`
        option through, and test on OpenShift. Mind Kubernetes 1.31, decision 51
  - [ ] Several nvweavers behind one driver, each StorageClass naming its API and credentials through CSI secrets, as arrays'
        drivers do for several arrays
  - [ ] Importing a volume nvweaver already has into Kubernetes, for migrations
  - [x] `ListVolumes` and `ListSnapshots`, which `csi-sanity` and some backup tools use
  - [ ] Test on OpenShift 4.18 itself, and on Kubernetes 1.32 and 1.33, which only the chart's rendering covers
  - [ ] Test at production-like volume and node counts. The controller passed at 5,000 volumes on 100 simulated nodes, against
        a real nvweaver (phase 5). Still to do: real nodes connecting at that count, and the sidecars' own limits
  - [ ] Document that there is a single target, with no failover
  - [ ] Document what can never change once volumes exist: `--cluster`, the NQN prefix and the driver's name, as changing them
        orphans every volume. Test upgrading the driver with volumes in use
  - [ ] Add `k3s-csi.sh` to `run-all.sh`, or run it before each release
  - [ ] Test a clone of a volume with LVM inside it, on a node that also has the original. Its PV UUID and VG name match its source's, so the node's LVM should
        see duplicates. Nothing has tested this yet. Real arrays have the same problem, and the target can't fix it without changing the client's data. Find
        out what happens to pods on both volumes, then either document it or have the node plugin handle it, as with `vgimportclone`
## Security

- [x] Scoped API tokens, so that clusters sharing one nvweaver can't delete each other's volumes: by ownership, not name prefixes,
      decision 46. Tested in the k3s lab, with the driver on a scoped token
- [ ] Roles for people in the web interface, which decision 40 left out: everyone signed in is still an admin
- [ ] A way to hand a cluster's existing volumes, snapshots and subsystems to its scoped token in one go, rather than one at a time
- [ ] Hosts owned by a token, when nodes get DH-HMAC-CHAP keys, which only admins may set now
- [ ] Document backing up nvweaver's data directory, and recovering from losing it: it's a single target, and the state is what
      says which volume is which

## Monitoring

- [x] A Prometheus endpoint, `/metrics`, and OpenTelemetry metrics, so that usage can be watched and alerts set up with a third-party
      tool. At least: each pool's size, use and data and metadata percentages, as a thin pool filling up gives hosts I/O errors; counts of
      volumes, snapshots, subsystems, namespaces and connections; API requests by route and status, and their latency; the
      reconcile and intent log; and `warnings`. Spec first, and authenticated like the rest of the API
  - [x] Decide how OpenTelemetry is sent: through the Collector's `prometheus` receiver, decision 47. `docs/monitoring.md`
  - [x] Example alert rules, such as a pool over 80% and a target that isn't ready: in `docs/monitoring.md`
  - [ ] Push OTLP from nvweaver itself, if scraping isn't enough: by hand with the standard library, or with the SDK, decision 40
  - [ ] More metrics: connections, which cost an RPC per subsystem on SPDK, reconcile timings, and the driver's own
  - [ ] The CSI driver's own metrics, from its sidecars, documented

## Later (low priority)

- [ ] Publish the `nvweaver` command line utility for macOS, on arm64 and amd64, and Windows on amd64. Only the client: the service stays Linux only. Name them as the Linux ones are, and include them in
      the checksums, attestations and SBOM
  - [ ] Add a smoke test on hosted macOS and Windows runners, running the real binary against a fake API, as nothing has run there yet. Don't publish those builds without it. All these targets
        cross-compile, but `go test ./cmd/nvweaver` won't there, as its tests use the Linux-only `internal/target`
  - [ ] Check what differs there: file paths for `--token-file` and `--ca`, the system's certificates, and signals on Windows
- [ ] iSCSI as a second protocol, next after consistency groups (see "Next"), once the CLI and CSI driver have settled the API. It would be new resources, such as `/v1/iscsi/targets`, not a
      `transport` on ports. Pools, volumes and snapshots should carry over: see decision 33
  - [x] Probe whether it's really feasible, driving LIO and SPDK by hand in VMs: it is, on both. See "iSCSI: is it feasible?" in `docs/testing.md`
  - [ ] Decide how to handle a volume's identity differing between LIO and SPDK, which can't be made to match
  - [ ] Pausing for snapshot groups with ALUA, LIO's equivalent of ANA: a LUN moved into a target port group in the
        "transitioning" state. Find out what Linux hosts need for that: `scsi_dh_alua` may requeue I/O without dm-multipath.
        Each node then reports for iSCSI too. Decision 56
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

- [x] The CSI driver on Arm: installed with the Helm chart on the user's single-node OpenShift 5.0 cluster on arm64, with a
      class of its own. 25 of 25 checks, CBT included, once decision 53 fixed ReadOnlyMany on SELinux nodes. See "The CSI driver
      on Arm" in `docs/testing.md`
- [ ] A KubeVirt VM on that cluster, on the nvweaver class
- [x] `nvw`'s class names no pool, so adopting a second pool on nvweaver-1 stopped it making volumes (found 2026-11-10).
      Recreated with `pool: csi`, through Helm
- [ ] The k3s lab doesn't relabel volumes for SELinux, which is how decision 53's bug got past it: run k3s with SELinux
      (`--selinux`) so the lab sees what OpenShift does
- [ ] Native Arm VMs on that cluster, on its DHCP VLAN, to run `e2e.sh` and `vm-e2e.sh` with nvweaver as the target on both
      backends, rather than emulated on x86 (`LAB_ARCH=aarch64`), which is too slow to run often

No physical NVMe hardware is available right now, so development continues on emulation: loopback on one machine, QEMU VMs
with emulated NVMe, SATA and virtio disks, a k3s lab, a Raspberry Pi CM4 over a real network, and a dedicated test server, a VM with four
virtual disks for hosting nvweaver.

- [x] Run all suites on rootful Podman
- [x] Add a QEMU environment with emulated NVMe drives and separate target and initiator
      VMs (`test/vm/lab.sh`). It emulates the hardware, so it does not replace the tests below
- [x] Use the VMs to test passthrough against a PCIe-style controller (`test/vm/vm-e2e.sh`)
- [x] Use the VMs to test with a separate initiator
- [x] Use the VMs to test the systemd setup for real: `make install` as root, enable, reboot
      the VM, SELinux enforcing, and the Quadlet on the VM's own Podman
- [ ] Run benchmarks between the VMs and over the 1 Gb network, on both backends. A dedicated test server and the k3s nodes make the network half possible now
- [ ] Long term: run the suites on a Debian VM too. All VM testing so far is on Fedora, and Debian differs where it matters, such as
      LVM having no devices file by default

### Deferred until hardware is available

- [ ] Test block device and passthrough namespaces on a physical NVMe drive
- [ ] Test with a separate physical initiator over a real network. A network test between machines is done, from the CM4: see below
- [ ] Measure real performance on both backends. Current numbers are from a loaded VM over
      loopback
- [ ] Test a real power cut on SPDK with a block device, on a drive with a volatile write cache. SPDK ignores FUA, which
      QEMU's disks can't show the effect of. See decision 28
- [ ] Install on a real host and reboot, to prove the boot ordering. So far that's been done in containers, and in VMs by
      `vm-e2e.sh`
- [x] Test on a host that really has no `nvmet-tcp`: a Raspberry Pi CM4, where SPDK took over. See `docs/testing.md`
- [ ] Finish testing on real Raspberry Pis, on a 64-bit OS:
  - [x] CM4 (Cortex-A72, no AES or SHA) with SPDK, over a real network: 43 of 43. See `docs/testing.md`
  - [ ] Add that network test to the repository: it needs an `e2e.sh` that can use a remote initiator. The dedicated test server and the CM4 can both serve as one
  - [x] Run the SPDK Quadlet on the CM4
  - [ ] Run the plain systemd unit on the CM4
  - [ ] Pi 5 (Cortex-A76): Raspberry Pi OS defaults to a 16K-page kernel, while our VMs use 4K, which
        may matter to SPDK and DPDK
  - [ ] The kernel target on a Pi: Raspberry Pi OS's kernel has `NVME_TCP` and `NVME_TARGET` off, so it needs a different
        kernel or a module build. Only worth doing if the kernel target should be supported on Pis
  - [ ] Real storage: USB disks on a Pi 4, and PCIe NVMe on a CM4 carrier board or a Pi 5
