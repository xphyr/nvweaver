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
- **Known gap:** there is no TLS yet, so a token crosses the network in clear text. See
  `TODO.md`, phase 2.

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

## 17. Roadmap order

- **Chosen:** the order in `TODO.md`.
- **Why:**
  - TLS comes before the web UI and CSI driver, which reach the API over a network.
  - The storage layer comes before the web UI, so the UI isn't reworked around volumes.
  - Scale work comes before the CSI driver, because production-like scale is a requirement.
