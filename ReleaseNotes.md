# Release notes

The major differences between versions. For every change, see the commit history.

## v0.2.0 (not yet released)

### Added

- **Storage pools.** Adopt a directory on XFS or btrfs as a reflink pool, without nvweaver ever initializing a disk. See
  [docs/pools.md](docs/pools.md). LVM thin pools are planned.
- **Volumes.** Create, grow and delete volumes in a pool, and map them into subsystems. A volume keeps its UUID, NGUID and
  data when it moves to another subsystem. Lists come a page at a time.
- **Snapshots and clones.** Instant, read-only snapshots, which work while a host is writing, and can outlive their volume.
  A clone is a new volume with its own identity, made from a snapshot or a volume. Snapshots are crash-consistent.
- **Sharing a volume between hosts,** for Kubernetes RWX block volumes and KubeVirt live migration, using a subsystem that
  allows several hosts.
- **Recovery from a crash.** If the service dies while changing a pool, the next start finishes or undoes that change, so
  storage isn't left orphaned.
- **A transport on ports.** Ports report `transport`, and `capabilities.transports` lists what's usable. Only `tcp` is
  available, and anything else is refused. This leaves room for NVMe/RDMA later.

### Planned for this release

- **arm64 images and tarball,** alongside x86-64. So far, arm64 builds from source. Its tests pass on Arm CI runners and
  emulated Cortex-A72, and SPDK works on a Raspberry Pi CM4. Raspberry Pi OS's kernel has no NVMe/TCP target, so only SPDK
  can run there.

### Fixed

- **The Quadlet in the release tarball now names the published image.** In v0.1.0, it needed the image tagged by hand.
- **The SPDK Quadlet no longer reports `failed` after a slow shutdown.** Podman killed it after 10 seconds, and it now
  allows 25.
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
