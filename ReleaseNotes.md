# Release notes

The major differences between versions. For every change, see the commit history.

## Not yet released

### Added

- **SPDK can honour FUA, with nvweaver's patch, if you choose:** `NVWEAVER_SPDK_DURABILITY=fua`. Upstream SPDK ignores FUA, so
  by default every file is made to write synchronously, which is slow on real disks. The patch makes only the writes that
  hosts need durable synchronous: as safe in a power-cut test, and other writes 1.6 to 3.4 times faster on an all-flash
  Synology. It isn't upstream SPDK, so it's off by default; the SPDK image carries both builds. Changing it is a restart.
  `capabilities.durability` in `GET /v1/system` reports it. See decision 58.

## v0.5.0

Snapshot groups, changed block tracking for backups, a Synology setup, and a CSI driver that cleans up after nodes that leave,
and runs on Kubernetes 1.31 and later, and on Arm.

### Upgrading

- **Hosts made by an earlier CSI driver have no owner,** so the driver can't label them, or delete them when their node
  leaves, and its log says so. Give each to its cluster's scoped token, as an admin:
  `nvweaver host update <node's host NQN> --owner <cluster>`. See `csi/README.md`.
- **Group snapshots need this nvweaver as well as this driver.** The driver refuses them, saying to upgrade, against an older
  one.
- **On SPDK, hosts will see ANA reporting change** when it restarts on this version: see "Changed".

### Added

- **nvweaver on a Synology, or any host with no NVMe target:** `dist/synology/` has a Compose file for Container Manager,
  and a guide. It runs SPDK, unprivileged, with one capability, serving reflink pools on a btrfs volume. DSM's kernel has
  no NVMe target, so this is the way there. See decision 57.
- **The CSI driver supports Kubernetes 1.31 and later,** such as OpenShift 4.18. Below 1.34 it needs older releases of the
  provisioner and resizer sidecars, which the Helm chart picks itself. See decision 51 and `csi/README.md`.
- **Changed block tracking,** for backups: `GET /v1/snapshots/{snapshot}/changes?base=` reports what changed between two
  snapshots of a volume, and `/allocated` where a snapshot holds data, from the storage's own mappings, on both pool types
  and both targets. Also `nvweaver snapshot changes` and `snapshot allocated`. On LVM it needs `thin_delta` and `thin_dump`,
  from the package that already provides `thin_check`. See decision 50.
- **The CSI driver is tested on Arm,** on single-node OpenShift 5.0 on arm64, and `csi/deploy/openshift/` has MachineConfigs to
  load `nvme-tcp` at boot on nodes that don't.
- **The CSI driver serves changed block tracking to backup tools,** through Kubernetes' external-snapshot-metadata sidecar,
  from Kubernetes 1.33. It's off unless the Helm chart's `snapshotMetadata.enabled` is set. See `csi/README.md`.

- **Snapshot groups:** several volumes snapshotted as of one instant, consistent with each other, with
  `POST /v1/snapshot-groups`, `nvweaver snapshot-group`, and in the web interface, which can delete them. nvweaver pauses hosts' I/O to the volumes with NVMe ANA while it
  takes them, for a few ms per volume, on both targets and both pool types. **Hosts need native NVMe multipath**, Linux's
  default, to wait out the pause. The CSI driver serves them as `VolumeGroupSnapshot`, off unless asked for, and refuses a
  group that would pause a node without it. The web interface shows each snapshot's group. See decision 56.
- **The CSI driver forgets nodes that leave the cluster:** once nothing is exported to a departed node, its subsystem and
  host in nvweaver are deleted. Hosts have owners now, as volumes do, so a cluster's scoped token may change and delete its own;
  hosts made before have none, and need an admin to give them to the cluster's token: see `csi/README.md`. See decision 54.
- **Hosts have labels,** and the CSI driver labels each node's host with its cluster and Kubernetes node. The web interface's
  host list shows them, and the owner. `nvweaver host create` and `host update` take `--label` and `--owner`.
- **Two drivers in one cluster,** for two nvweavers, such as a server and a Synology: the chart's `node.livenessPort`, and
  what else a second instance must change, in `csi/README.md`.
- **A KubeVirt VM's snapshot with its guest agent's freeze** is tested, and `csi/README.md` says what an SELinux guest's data
  disks need for it.
- **The web interface's overview lists problems:** pool warnings, and volumes and snapshots whose storage is missing.

### Fixed

- **LVM snapshots no longer show as `missing`** in the API and web interface when they're only inactive, as the CSI driver's
  are.
- **ReadOnlyMany filesystems work on SELinux nodes, such as OpenShift's.** Pods using one never started, as a read-only
  filesystem can't be relabelled for each pod; they're mounted with one label now. See decision 53.
- **The CSI driver mounts ReadOnlyMany filesystems read-only** on every node. Before, each node mounted them read-write,
  and only the pod's view was read-only, which can corrupt a filesystem mounted on several nodes. See decision 48.
- **The CSI driver refuses a port listening on `0.0.0.0` or `::`,** with an error naming the port, rather than telling nodes
  to connect there, which connected them to themselves.

### Changed

- **On SPDK, subsystems have ANA reporting on, with every namespace in ANA group 1.** That's how a snapshot group pauses
  hosts. SPDK's own default was a group per namespace. Hosts with native NVMe multipath see the change and carry on.
- **Scoped tokens may change the labels of hosts they own, and delete them,** as with volumes and subsystems. Keys stay an
  admin's.
- **Built with Go 1.26.11,** from 1.26.10.
- **Publishing CSI volumes is four to five times faster at scale.** At 5,000 LVM volumes, exporting one looked up every other
  volume, and the driver made two creates on every publish that changed nothing. 5,000 publishes took 20 minutes, and now
  take 4. See decision 49 and "The CSI driver at scale" in `docs/testing.md`.

## v0.4.0

The CSI driver, scoped tokens and metrics. The driver's image is published for the first time, as
`ghcr.io/xphyr/nvweaver-csi`, for amd64 and arm64.

### Added

- **Metrics, at `GET /metrics`,** in Prometheus's text format: how full each pool is, in data and LVM metadata, object counts,
  warnings, stuck changes, and API requests and their latency. OpenTelemetry users scrape it with the Collector. Metric names,
  scrape configs and example alerts are in `docs/monitoring.md`.
- **A Kubernetes CSI driver,** in `csi/`: dynamic provisioning of filesystem and raw block volumes from nvweaver's pools, over NVMe/TCP,
  with growing, snapshots and clones, and raw block volumes shared by several nodes. It's new, tested so far in a k3s lab of three VMs,
  and on a compact OpenShift 4.22 cluster, with KubeVirt live migration. Its image is `ghcr.io/xphyr/nvweaver-csi`. See `csi/README.md`.
- **A Helm chart for the CSI driver,** in `csi/deploy/helm/nvweaver-csi`, as another way to install it: the driver, its RBAC, a
  StorageClass and snapshot class, and on OpenShift its SCC, from values. The step-by-step manifests stay.
- **Scoped API tokens,** from a file in `NVWEAVER_TOKENS_FILE`, made with `nvweaver token new`. A scoped token may read everything,
  but only change the volumes, snapshots and subsystems it created, which show it as their owner in the API, the CLI and the web
  interface. One for each Kubernetes cluster keeps clusters sharing an nvweaver from touching each other's volumes.
- **A Kubernetes lab for testing it,** `test/vm/k3s-lab.sh`: k3s on three VMs and nvweaver on a fourth, with no root.

### Changed

- **On SPDK, `GET /v1/connections` needs `?subsystem=`,** and `nvweaver connection list` needs `--subsystem`. Listing every connection
  asked each subsystem in turn, which takes tens of seconds at thousands of subsystems and holds up changes. The kernel target still
  lists everyone. `lists_all_connections` under `capabilities` says which applies.
- **A logical volume can't be exported over a disk that's exported already,** nor a block device that sysfs can't describe, as the
  check that a disk and its partition aren't both exported needs sysfs.
- **The key dialog in the web interface says what saving a key does:** put the key on the host first, as its sessions are dropped.

- **Changing a host's DH-HMAC-CHAP key drops its connections,** so that it logs in again with the new key. Before, a connected host carried on
  with its old key indefinitely. Give the host its new key first. On the kernel target, other hosts on that subsystem stall for about 10 s.
- **The same storage can't be exported twice under another name:** a hard link, a bind mount, or a disk and one of its partitions. Passthrough
  is refused for a controller that's already passed through, or has a namespace that's exported, mounted or otherwise in use.
- **Changing `device_path` on a namespace whose file nvweaver created is refused,** as the file would be left behind. Delete the namespace
  instead, with `delete_file=true` to remove the file.
- **Creating a volume again with different labels is a `409`,** like any other different setting, rather than returning the existing volume.

### Fixed

- **A volume or snapshot whose storage can't be found is reported as `missing`,** rather than always `online`, and the interface shows it.
- **After one SPDK request timed out, every later one failed** until the service restarted.
- **A crash while creating or deleting a namespace's file left it behind,** under a name that couldn't be used again. A failed grow could
  also leave hosts seeing the old size. Both now finish at the next start, or when the request is sent again.
- **A failed write of a host's key returned the key in the error.**
- **Changing a port detached subsystems that nvweaver doesn't manage.**
- **A namespace's new file that couldn't be removed, after its create failed, was forgotten,** so the name stayed taken. It's now
  removed at the next start.

## v0.3.0

### Security

- **Web pages could use the API, in all earlier versions.** With no token, which is allowed on loopback, any page open in a browser on the same machine could send requests to the API, and export any unused disk, and DNS rebinding could read
  from it too. These are now refused. If you run an earlier version, set `NVWEAVER_API_TOKEN`, which prevents this, or don't browse from that machine.

### Added

- **`nvweaver`, a command line utility,** for everything the API does, from this machine or another. It's in the images and the tarball. See [Command line](README.md#command-line).
- **A web interface,** at `/ui/`, which shows pools, volumes, snapshots, subsystems, hosts, ports and warnings. It manages volumes, snapshots, subsystems, hosts and ports, and adopts pools. Sign in with a username and password, from a users file. See [Web interface](README.md#web-interface).
- **`nvweaver passwd`,** to make a line for that file.
- **`GET /v1/connections`, and `nvweaver connection list`,** for who's connected. The interface shows it on a subsystem's page, and before un-exporting or deleting. SPDK names each host. The kernel's target
  usually can't, so it lists the addresses connected to each port. `identifies_connected_hosts` under `capabilities` says which applies.
- **An SBOM for each release,** in SPDX JSON, for each image and package and both architectures, which includes SPDK, DPDK and ISA-L. See [docs/releasing.md](docs/releasing.md).
- **`nvweaver` on its own, as a release artifact,** as `nvweaver-cli-<version>-linux-<arch>`, for people who only want the client.
- **HTTPS for the API,** with `NVWEAVER_TLS_CERT` and `NVWEAVER_TLS_KEY`. Renewing a certificate needs no restart. Without them, serving off
  loopback now logs a warning that the token is in clear text.
- **`warnings` in `GET /v1/system`,** which reports if this machine's LVM can read exported devices. A client's LVM on one can get activated here at boot, and can
  take a pool offline. See ["Clients' LVM"](docs/pools.md#clients-lvm). They also report if an LVM pool's `lvm` or `thin_check` is missing.

## v0.2.0

### Added

- **Storage pools.** Adopt an existing LVM thin pool, or a directory on XFS or btrfs as a reflink pool, without nvweaver
  ever initializing a disk. See [docs/pools.md](docs/pools.md). In a container, LVM pools need `--pid=host`.
- **Volumes.** Create, grow and delete volumes in a pool, and map them into subsystems. A volume keeps its UUID, NGUID and
  data when it moves to another subsystem. Lists come a page at a time.
- **Snapshots and clones.** Instant, read-only snapshots, which work while a host is writing, and can outlive their volume.
  A clone is a new volume with its own identity, made from a snapshot or a volume. Snapshots are crash-consistent.
- **Sharing a volume between hosts,** for Kubernetes RWX block volumes and KubeVirt live migration, using a subsystem that
  allows several hosts.
- **Recovery from a crash.** If the service dies while changing a pool, the next start finishes or undoes that change, so
  storage isn't left orphaned.
- **arm64 images and tarball,** alongside x86-64. The image tags now cover both, and Podman pulls the right one. Its tests pass
  on Arm CI runners and emulated Cortex-A72, and SPDK works on a Raspberry Pi CM4. Raspberry Pi OS's kernel has no NVMe/TCP
  target, so only SPDK can run there. See [known limits](README.md#known-limits).
- **A transport on ports.** Ports report `transport`, and `capabilities.transports` lists what's usable. Only `tcp` is
  available, and anything else is refused. This leaves room for NVMe/RDMA later.

### Changed

- **Much faster at scale.** Adding a volume with 5,000 in place takes 20 ms instead of 264 ms on the kernel target, and 60 ms instead of 488 ms on
  SPDK. SPDK also now allows 8,192 subsystems, up from 1,024. See [known limits](README.md#known-limits).
- **The API stays responsive while storage is busy.** Reads never wait for a change, and slow storage work no longer holds up other requests.
- **The API can answer `503` with `Retry-After`** when too much is pending, or when an object is in the middle of another change. Clients should retry.
- **SPDK stops much faster with many volumes,** without taking the target apart first.
- **Removing a host now cuts it off straight away on the kernel target,** rather than when it next reconnects. Other hosts on that
  subsystem stall for about 10 s while they reconnect, with no errors. See [known limits](README.md#known-limits).
- **Too small an `fs.aio-max-nr` on SPDK gets a clear error** saying how to raise it. This also stops a saved configuration from starting.

### Fixed

- **The Quadlet in the release tarball now names the published image.** In v0.1.0, it needed the image tagged by hand.
- **SPDK could corrupt hosts' filesystems in a power cut when using files,** including in v0.1.0. It ignores FUA, which XFS relies
  on. Files are now set to write synchronously, which fixes that, but makes small writes to files on SPDK several times slower.
  The kernel target and LVM volumes aren't slowed. See [known limits](README.md#known-limits) for block devices on SPDK.
- **The SPDK Quadlet no longer reports `failed` after a slow shutdown.** Podman killed it after 10 seconds, and it now
  allows 25.
- **systemd no longer kills the service while it starts or stops with thousands of volumes.** Its time limits were too short: start is now
  900 s, and stopping the unit 300 s.
- **Listing namespaces on SPDK is much faster at scale,** at about 71 ms instead of 32 seconds with 5,000 volumes.

## v0.1.0

The first release, for x86-64.

- NVMe/TCP target with a REST API, using the kernel's `nvmet` or SPDK as a fallback, with `NVWEAVER_BACKEND=auto`
- Ports, hosts, subsystems and namespaces, backed by block devices or files, plus NVMe passthrough on the kernel target
- DH-HMAC-CHAP authentication for hosts
- Configuration that survives restarts, and optionally keeps serving hosts while the service restarts, on the kernel target
- Container images, a systemd unit and a Quadlet
- An OpenAPI description of the API

**Known issue:** the tarball's Quadlet needs the image tagged by hand, or its `Image=` line changed. This is fixed in v0.2.0.
