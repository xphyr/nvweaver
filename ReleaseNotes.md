# Release notes

The major differences between versions. For every change, see the commit history.

## Unreleased

### Added

- **A Kubernetes CSI driver,** in `csi/`: dynamic provisioning of filesystem and raw block volumes from nvweaver's pools, over NVMe/TCP,
  with growing, snapshots and clones, and raw block volumes shared by several nodes. It's new, tested so far in a k3s lab of three VMs,
  and its image isn't published yet: build it with `make images`. See "Kubernetes CSI driver" in the README.
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
