# nvweaver

<p align="center">
  <img src="images/nvweaver-logo-light-italic.png" alt="nvweaver logo">
</p>

An NVMe over TCP target for test and automation labs, run from a container and
configured through a REST API.

A small control plane (one static Go binary) turns API calls into target
configuration and saves it, so it survives restarts. It is not in the data path.
Two targets can carry the I/O:

- **The Linux kernel's `nvmet`**, the default and preferred choice.
- **[SPDK](https://spdk.io)'s userspace target**, a fallback for hosts whose kernel
  has no `nvmet-tcp`, or where a container can't be privileged. Non-NVMe disks and
  files work on it too: storage stays on its normal kernel driver.

The API, saved configuration and test suite are shared, so a volume can move
between targets and hosts see the same UUID, NGUID, block size and data.

## Requirements

| | Kernel target | SPDK target |
|---|---|---|
| Image | `Containerfile`, 25 MB | `Containerfile.spdk`, 134 MB (includes both targets) |
| Host kernel | `nvmet-tcp` module (`modinfo nvmet-tcp`), which is separate from the `nvme-tcp` initiator | No NVMe modules |
| Container | Privileged, host network | Can run with `--cap-drop=ALL` |
| CPU | x86-64 or arm64 | x86-64-v2 or newer (about 2009 onward), or arm64 with CRC instructions, tested on Cortex-A72 (Raspberry Pi 4) |
| Memory | About 8 MB plus kernel use | About 240 MB (`NVWEAVER_SPDK_MEM_MB`, default 1024, is its limit) |

## Quick start

```sh
sudo podman build -t nvweaver:dev -f Containerfile .
sudo podman run -d --name nvweaver --privileged --network host \
  -v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-data:/var/lib/nvweaver \
  nvweaver:dev
API=http://127.0.0.1:8420
SUB=nqn.2026-11.lab.example:vol1
HOST=nqn.2026-11.lab.example:client1

# A listener, an initiator, and a subsystem joining them.
curl -X POST $API/v1/ports -d '{"id":1,"address":"192.0.2.10","service":4420}'
curl -X POST $API/v1/hosts -d "{\"nqn\":\"$HOST\"}"
curl -X POST $API/v1/subsystems \
  -d "{\"nqn\":\"$SUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"

# Export a device, or a new sparse file (use one or the other).
curl -X POST $API/v1/subsystems/$SUB/namespaces -d '{"device_path":"/dev/sdc"}'
curl -X POST $API/v1/subsystems/$SUB/namespaces \
  -d '{"file":{"name":"vol1.img","size_bytes":10737418240}}'
```

The commands above run as root because the kernel target needs it. `compose.yaml` does the
same thing for Compose users, and hasn't been tested. Docker is a fallback: the same
commands work with `docker` in place of `sudo podman`.

On a host without `nvmet-tcp`, use the SPDK image, with no privileges and only the
storage you pass in. Omit `--device` to use only service-created files:

```sh
podman build -t nvweaver:spdk -f Containerfile.spdk .
podman run -d --name nvweaver --network host --cap-drop=ALL \
  --device /dev/sdc -v nvweaver-data:/var/lib/nvweaver \
  nvweaver:spdk
```

Then run the same API calls. Run the build once and keep the image: it compiles SPDK.

On the initiator:

```sh
nvme discover -t tcp -a 192.0.2.10 -s 4420 --hostnqn=$HOST
nvme connect  -t tcp -a 192.0.2.10 -s 4420 -n $SUB --hostnqn=$HOST
```

Open the port in the target's firewall. The kernel serves discovery on every
port; add one on 8009 if your initiators expect the standard discovery port.

## What can back a namespace

| Backing | How | Notes |
|---|---|---|
| NVMe namespace | `device_path: /dev/nvme0n1` | Exported as a block device. |
| SATA, SAS, virtio, loop, and so on | `device_path: /dev/sdc` | Any block device the host has. |
| File on a volume you mounted | `device_path: /data/x.img` | Path as seen inside the container. |
| Sparse file managed by the service | `file: {name, size_bytes}` | Can be grown live. Mount a volume at the data directory, as the examples do. |
| Whole NVMe controller | `passthru_device: /dev/nvme0` on the subsystem | Hosts see the real controller. The subsystem can't have other namespaces. |

The kernel target refuses files on overlayfs, which includes a container's own
filesystem: hosts would see a disk of size zero with no device node.

On SPDK, passthrough is not available, and everything else in the table goes
through Linux AIO, so an NVMe drive gets no more speed than the kernel target gives
it. SPDK's own NVMe driver is deliberately not used, because it would take the drive
away from the kernel.

Prefer `/dev/disk/by-id/...` paths, because `/dev/sdX` names can change across
reboots. A block device that is mounted, used as swap, or held by LVM/MD is
refused, as is exporting the same device twice.

## API

[`api/openapi.yaml`](api/openapi.yaml) is the full specification. JSON over HTTP. Errors are `{"error":"..."}` with status 400, 401, 404, 409
(conflict, including an address already in use) or 422 (invalid). A failed change
is rolled back, so it never leaves partial configuration.

| Resource | Methods |
|---|---|
| `/healthz` | `GET` (needs no token) |
| `/v1/system` | `GET` (includes the active `backend` and its `capabilities`) |
| `/v1/ports`, `/v1/ports/{id}` | `GET` `POST`, `GET` `PATCH` `DELETE` |
| `/v1/hosts`, `/v1/hosts/{nqn}` | `GET` `POST`, `GET` `PATCH` `DELETE` |
| `/v1/subsystems`, `/v1/subsystems/{nqn}` | `GET` `POST`, `GET` `PATCH` `DELETE` |
| `/v1/subsystems/{nqn}/ports/{id}` | `PUT` `DELETE` (idempotent) |
| `/v1/subsystems/{nqn}/hosts/{nqn}` | `PUT` `DELETE` (idempotent) |
| `/v1/subsystems/{nqn}/namespaces`, `.../{nsid}` | `GET` `POST`, `GET` `PATCH` `DELETE` |

Behaviour worth knowing:

- `PATCH` on a port drops its connections briefly, because the kernel only allows
  port changes while no subsystem is attached.
- A subsystem's serial and model can't be changed after creation, because the
  kernel freezes them once a host has identified the controller.
- Serial, UUID and NGUID are generated once and saved, so hosts see the same
  identity after a restart.
- `DELETE .../namespaces/{nsid}?delete_file=true` also deletes a managed file.
  Without it, the file is kept.
- DH-HMAC-CHAP: set `dhchap_key` (and optionally `dhchap_ctrl_key`, `dhchap_hash`,
  `dhchap_dhgroup`) on a host. Secrets are never returned by the API. They are
  stored in plain text in the mode 0600 state file.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NVWEAVER_LISTEN` | `127.0.0.1:8420` | API address. |
| `NVWEAVER_API_TOKEN` | none | Bearer token. Required unless the API is on loopback. |
| `NVWEAVER_DATA_DIR` | `/var/lib/nvweaver` | Saved state and managed files. |
| `NVWEAVER_KEEP_ON_EXIT` | `false` | Leave the target serving when the container stops. Kernel target only. |
| `NVWEAVER_BACKEND` | `auto` | `auto`, `nvmet` or `spdk`. `auto` uses the kernel if it can, otherwise SPDK if the image has it. |
| `NVWEAVER_SPDK_CORES` | one CPU | SPDK CPU list or mask, such as `[2,3]`. |
| `NVWEAVER_SPDK_MEM_MB` | `1024` | Memory SPDK may reserve for buffers. 256 is too little. |
| `NVWEAVER_SPDK_HUGEPAGES` | `false` | Use hugepages already reserved on the host. |
| `NVWEAVER_SPDK_POLLING` | `false` | Poll continuously: lower latency, but each SPDK core runs at 100% even when idle. |

By default, stopping the container removes the configuration from the kernel, and
starting it restores it; connected hosts reconnect by themselves. With
`NVWEAVER_KEEP_ON_EXIT=true` the kernel keeps serving with no container running,
and the next instance adopts it without disturbing hosts, which allows upgrades
with no interruption. Objects created outside this service are left alone.

## Installing from a release

[Releases](https://github.com/xphyr/nvweaver/releases) include a tarball with the binary and units,
which needs nothing built. Check it, then install it:

```sh
sha256sum -c SHA256SUMS
# Use arm64 in place of amd64 on Arm.
tar xzf nvweaver-*-linux-amd64.tar.gz
cd nvweaver-*-linux-amd64

sudo install -m 0755 nvweaverd /usr/local/bin/nvweaverd
sudo install -m 0644 nvweaver.service /etc/systemd/system/
sudo install -d -m 0755 /etc/nvweaver
sudo install -m 0600 nvweaver.env /etc/nvweaver/
sudo systemctl daemon-reload
sudo systemctl enable --now nvweaver
```

Use `install` rather than `mv`. On a system with SELinux enforcing, a file moved from your home
directory keeps its label, and the service fails to start with status `203/EXEC`.

For SPDK on a host without `nvmet-tcp`, install the Quadlet instead, which pulls the matching image:

```sh
sudo install -d -m 0755 /etc/nvweaver /etc/containers/systemd
sudo install -m 0600 nvweaver.env /etc/nvweaver/
sudo install -m 0644 nvweaver-spdk.container /etc/containers/systemd/
sudo systemctl daemon-reload
sudo systemctl start nvweaver-spdk
```

The images are `ghcr.io/xphyr/nvweaver:<version>` and `:<version>-spdk`, or `latest` and `latest-spdk`, for both x86-64 and
arm64. Podman pulls the right one for the machine.

## Running under systemd

To build from source for the kernel target, run the binary directly: the container adds nothing
there. This needs Go to build, and enables nothing until you ask:

```sh
make
sudo make install
sudo systemctl daemon-reload
sudo systemctl enable --now nvweaver
```

Settings go in `/etc/nvweaver/nvweaver.env`, using the variables under
[Configuration](#configuration). Configuration and managed files are in
`/var/lib/nvweaver`. `systemctl start` returns once the target and API are ready, so
units ordered after this one can rely on it. If startup fails, for example because
an address isn't up yet, systemd retries every 5 seconds for up to 5 minutes.

For the SPDK target, run the container using Podman Quadlet, which needs no build tools
beyond Podman, and no capabilities:

```sh
sudo podman build -t nvweaver:spdk -f Containerfile.spdk .
sudo make install-quadlet
sudo systemctl daemon-reload
sudo systemctl start nvweaver-spdk
```

Add an `AddDevice=` line to `/etc/containers/systemd/nvweaver-spdk.container` for each
disk it may export, giving it a name inside the container, such as
`AddDevice=/dev/disk/by-id/your-disk:/dev/nvweaver0`. Use that name in the API. It starts at boot once installed, because Quadlet units can't be
enabled or disabled; remove the file to stop that.

Worth knowing:

- **The network must really be up.** Check that your distribution's network-wait
  service is enabled (such as `NetworkManager-wait-for-online`), or use the retries.
- **Backing storage must be there too.** For files on another mount, add
  `RequiresMountsFor=/that/mount` with `systemctl edit nvweaver`.
- **The unit is sandboxed.** It can't see `/home`, so backing files can't be there, and
  `/usr`, `/boot` and `/etc` are read-only. Relax a setting with `systemctl edit nvweaver`
  if you need to. Don't add `ProtectKernelTunables`, `ProtectKernelModules`,
  `PrivateDevices` or `ProtectClock`, which stop it working.
- **`NVWEAVER_KEEP_ON_EXIT=true` also applies to `systemctl stop`:** the target keeps
  serving hosts. To stop it, remove the setting, restart, then stop.
- **`make uninstall`** keeps `/etc/nvweaver` and `/var/lib/nvweaver`. Stop and disable
  the service first.

## Differences on SPDK

Requests the SPDK target can't honour are refused with a 422, and `/v1/system`
reports them under `capabilities`:

- **No wildcard addresses.** Use a specific IP. SPDK would accept `0.0.0.0`, then
  reject every host.
- **Passthrough, `inline_data_size` and `buffered_io`** are not available.
- **`NVWEAVER_KEEP_ON_EXIT`** is refused at startup: I/O stops with the container.
  Hosts reconnect once it is back.
- **`dhchap_hash` and `dhchap_dhgroup` are accepted but not enforced per host.** The
  target and host negotiate from everything both support. Keys are enforced.
- **A crash ends the container**, so set a restart policy. The next start restores
  the configuration.

With a saved configuration that SPDK can't run, startup fails and names the setting.

Without host networking, publish the ports and create the port using the container's
own IP. Discovery then reports that IP, which hosts outside can't reach, so have them
connect directly to the published address.

## Security

This is for isolated lab networks.

- Anyone who can reach the API with its token can export any unused host block
  device. The API has no TLS, so the token travels in clear text if the API is not
  on loopback.
- NVMe/TCP traffic is unencrypted. TLS for NVMe/TCP is not implemented here.
- Host NQNs are claims, not proof of identity. Use DH-HMAC-CHAP if that matters.

## Testing

```sh
go test ./...      # needs no privileges; uses a fake configfs
./test/e2e.sh      # real target and Linux initiator over loopback
./test/bench.sh    # NVMe/TCP overhead, measured against null_blk
./test/systemd-e2e.sh  # the installed service under real systemd, in a container
./test/vm/vm-e2e.sh    # separate initiator, emulated disks, passthrough, real install and reboot
```

Set `BACKEND=spdk` to run `e2e.sh` and `bench.sh` against SPDK. `systemd-e2e.sh` installs
nothing on the machine running it: `make install` runs inside a disposable container.
`vm-e2e.sh` needs QEMU with KVM and no root, and manages its own VMs: see `test/vm/lab.sh`.

The last two need privileged containers. They temporarily load kernel modules and
create NVMe devices on the host, then remove them. Set `ENGINE=podman` to use Podman.

## Known limits

- NVMe/TCP only.
- **On the kernel target, removing a host from a subsystem briefly stalls other hosts on that subsystem.** The kernel can't drop one
  host's connection, so all of that subsystem's are dropped, and the others reconnect. They saw no errors, but their I/O stopped for
  10 s, which is the host's default reconnect delay. Hosts on other subsystems aren't affected, and neither is anyone on SPDK. A removed host
  can't read or write from then on in both cases, but on the kernel target it also loses its controller, and has to connect again if it's allowed back.
- **On SPDK, the kernel's `fs.aio-max-nr` limits volumes.** Each volume uses 128 per SPDK core, so the
  default of 65,536 allows 512 volumes on one core. Beyond that, adding a namespace is refused, and if the saved
  configuration needs more, the service won't start. Raise it on the host with `sysctl -w fs.aio-max-nr=1048576`: a
  container with no capabilities can't do this itself.
- **Tested to 5,000 volumes,** either across 100 subsystems or with one subsystem each. See [the numbers](docs/testing.md).
- **SPDK allows up to 8,192 subsystems,** and each uses about 0.37 MB of memory. Beyond the limit, creating one is refused. Starting SPDK
  also takes longer than the kernel target: about 150 s with 5,000 volumes, or 310 s with a subsystem for each.
- **On the kernel target, discovery fails for a host allowed on more than about 4,095 subsystems on one port.** This is the kernel's limit,
  and nothing about it is reported to nvweaver. Connecting by NQN still works. SPDK isn't affected.
- **Under load, the API can answer `503` with `Retry-After`.** Nothing has changed when it does, so send the request again.
- **SPDK ignores FUA, so its power-cut safety varies by what backs a namespace.** Files are set to write synchronously, which made
  a power cut safe in testing, but small writes are several times slower. On a filesystem that can't do this, adding the
  namespace is refused. Block devices, including LVM volumes, can't be set this way: writes reach the device, but a drive
  with a volatile cache might lose them in a power cut, which we haven't been able to test. For the strongest guarantee, use
  the kernel target.
- **arm64 has less testing than x86-64.** `e2e.sh` runs on real Arm CI runners on both targets, but the suites with a separate
  initiator and with disks run only by hand, under emulation. Its performance and scale aren't measured, and the 16K-page kernel of
  a Raspberry Pi 5 is untested. Raspberry Pi OS's kernel has no NVMe/TCP target, so only SPDK can run there.
- No formal NVMe conformance suite has been run. Protocol behaviour is the target's;
  the tests here exercise both with the Linux initiator only.
- **Not yet tested on physical NVMe hardware or over a physical network.** Tests use
  loopback and VMs with QEMU's emulated NVMe, SATA and virtio disks, including passthrough.
  See [TODO.md](TODO.md) for what is deferred until hardware is available.
- No ANA groups, NVMe/TCP TLS, per-controller statistics, or API for referrals.

## More documentation

- [TODO.md](TODO.md): the roadmap, and what is deferred until hardware is available
- [docs/architecture.md](docs/architecture.md): how the pieces fit together
- [docs/decisions.md](docs/decisions.md): design decisions, and what was rejected
- [docs/testing.md](docs/testing.md): coverage, latest results and known pitfalls
- [docs/releasing.md](docs/releasing.md): CI, and how releases are made and verified
- [docs/pools.md](docs/pools.md): creating LVM thin and reflink pools, in preparation for that feature

## License

[MIT](LICENSE). Bundled components have their own licenses: see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
