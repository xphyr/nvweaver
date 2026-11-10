# Architecture

## Overview

```
 REST clients
      |
      v
 internal/api      HTTP handlers, validation, in-use checks
      |
      v
 api.Manager       one change at a time; desired state; apply, then save; roll back on failure
      |
      v
 target.Backend    interface: Check, Reconcile, Revalidate, LiveEnabled, Available,
      |            Capabilities, Close, Done
      +--------------------------+
      v                          v
 target.Target               spdk.Backend
 (kernel nvmet)              (SPDK nvmf_tgt)
      |                          |
      v                          v
 configfs writes             JSON-RPC over a Unix socket
      |                          |
      v                          v
 kernel target threads       nvmf_tgt child process, with AIO bdevs
```

The control plane is `cmd/nvweaverd`. All NVMe/TCP traffic goes through the target
below it, never through this process.

## Packages

| Path | Purpose |
|---|---|
| `cmd/nvweaver` | The command line utility, which only uses `internal/client` |
| `internal/auth` | Signing in: the users file, bcrypt, sessions in memory and limiting guesses; scoped tokens and their file (decision 46) |
| `internal/webui` | The browser interface: static files embedded in the binary, which only use the public API |
| `internal/client` | Go client for the API, for the command line utility and CSI driver |
| `csi/` | The CSI driver, a module of its own (decision 44): controller (`driver/controller.go`), node plugin (`driver/node.go`), manifests (`deploy/`), Helm chart (`deploy/helm/`) |
| `cmd/nvweaverd` | Startup: settings, backend selection, API server, shutdown |
| `internal/api` | HTTP handlers (`server.go`), state and safety checks (`manager.go`), checking whether this machine's LVM can read exports (`lvmwatch.go`), metrics (`metrics.go`) |
| `internal/target` | State model and validation (`model.go`), `Backend` interface (`backend.go`), kernel backend (`reconcile.go`) |
| `internal/target/targettest` | Fake configfs that enforces the kernel's ordering rules |
| `internal/spdk` | SPDK backend: reconcile (`backend.go`), process (`process.go`), RPC (`rpc.go`) |
| `internal/storage` | Pool drivers, which allocate volumes as paths for the backends to export |
| `internal/sdnotify` | systemd readiness notification |
| `dist/systemd` | Unit template, environment file, Quadlet |
| `test` | End-to-end suites and benchmark |

## How a change is applied

`Manager.commit` handles every API change the same way, holding one lock:

1. Copy the current desired state, and apply the request to the copy.
2. Validate it (`State.Validate`), then ask the backend (`Backend.Check`).
3. `Backend.Reconcile(current, next)` moves the target to the new state.
4. Save the state to `state.json`, atomically with fsync. Commit it in memory.
5. If step 3 or 4 fails, reconcile back to the old state and return the error.

A change to a pool can't be saved together with `state.json`, so it's recorded first, in `intents/` under the data
directory. See `internal/api/intent.go`. On startup, each recorded name is settled by one rule: if the saved state refers
to it, the storage is wanted, and otherwise it's removed. An interrupted grow is finished, as undoing it could lose data.

So a request either happens completely or not at all. On startup, the saved state is
applied the same way, with an empty state as the starting point.

Reads don't take that lock. The state is replaced whole and never modified, so a read loads the latest one, and sees a change
once it has been applied and saved.

Slow storage work runs before step 1, without the lock. The request reserves the objects it depends on (`internal/api/busy.go`), so nothing can
change or remove them meanwhile. Whatever clashes gets `503` with `Retry-After`, as does a request when too much is pending. `Manager.mutate` is
`commit` with that check, and work that's already done uses `commit` directly, so that it can't be refused at the last moment.

## The two reconcilers

**Kernel (`target.Target`)** diffs `prev` against `next`, and only removes what is in
`prev`, so objects configured outside this service are left alone. It reads attributes
back and writes only what differs, which is what lets a restarted service adopt a
running target without disturbing hosts.

**SPDK (`spdk.Backend`)** ignores `prev` and makes SPDK's live configuration match
`next`. This service owns the whole process, so nothing else needs protecting, and
rollback is just another reconcile. Bdev and key names are hashes of what they describe,
so a change produces a new name and the old object is cleaned up as a leftover.

Both attach subsystems to ports last, so a host can never discover a partly configured
subsystem.

## State

`state.json`, mode 0600, in `NVWEAVER_DATA_DIR`. It holds the desired configuration,
including DH-CHAP secrets in plain text, and it is the same for both backends, which is
how a volume can move between them. Serial, UUID and NGUID are generated once and saved
there, because hosts recognise a volume by them.

Managed file volumes are in `volumes/` under the same directory.

## Pools and volumes

Pools, volumes and snapshots sit above the target backends, which don't know about them. Mapping a volume copies its
path, UUID and NGUID onto a namespace, which a backend treats like any other. So a pool type only has to produce a path.

LVM runs on the host, through `nsenter` from a container started with `--pid=host`, which it detects by comparing its mount
namespace with PID 1's. Otherwise it runs directly, as under systemd.

A snapshot and a clone are separate driver calls, because they can differ by pool type: a thin LVM snapshot stays inactive
until it's needed. On reflink both are instant copies of a file, and independent of their source once made.

Changed block tracking is a storage call too, and so works the same on both targets: a snapshot's allocated ranges, and
what changed between two snapshots of a volume, come from the pool's own mappings, `thin_dump` and `thin_delta` on a
reserved metadata snapshot for LVM, and FIEMAP for reflink. They run outside the lock, one at a time per LVM pool, as a
pool holds one metadata snapshot at a time. See decision 50.

## Backend lifetime

| | Kernel | SPDK |
|---|---|---|
| Data path | In the kernel, outlives this process | In a child process, stops with it |
| Default on stop | Removes configuration | Removes configuration, stops `nvmf_tgt` |
| `KEEP_ON_EXIT` | Keeps serving; next start adopts it | Refused at startup |
| Target dies | Not applicable | Service exits, so a restart policy can recover |

`NVWEAVER_BACKEND=auto` tries the kernel, and falls back to SPDK only if `nvmf_tgt` is on
the `PATH`.

## Known scale limits

Each change still copies the state and rewrites the state file, so the kernel target's time for a change grows slowly: 8 ms at
1,000 volumes and 20 ms at 5,000. On SPDK it's nearly flat, because each RPC takes about 8 ms, and a change makes the same few of them at any size.
On LVM, measured through the CSI driver, a change took about 50 ms at 5,000 volumes, most of it the save, and LVM's own
commands are slower still: each `lvcreate` waits for the volume group's lock, at about 0.4 s apiece with 5,000 LVs. Exporting a
volume compares it only with what was exported by path (decision 49), so it doesn't grow with the number of volumes.
Changes are applied one at a time, which limits them to about 50 a second on the kernel target at 5,000 volumes, and about 17 on SPDK. See
`docs/testing.md` for the numbers and decision 29 for why they aren't batched.
