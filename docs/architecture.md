# Architecture

## Overview

```
 REST clients
      |
      v
 internal/api      HTTP handlers, validation, in-use checks
      |
      v
 api.Manager       one lock; desired state; apply, then save; roll back on failure
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
| `cmd/nvweaverd` | Startup: settings, backend selection, API server, shutdown |
| `internal/api` | HTTP handlers (`server.go`), state and safety checks (`manager.go`) |
| `internal/target` | State model and validation (`model.go`), `Backend` interface (`backend.go`), kernel backend (`reconcile.go`) |
| `internal/target/targettest` | Fake configfs that enforces the kernel's ordering rules |
| `internal/spdk` | SPDK backend: reconcile (`backend.go`), process (`process.go`), RPC (`rpc.go`) |
| `internal/storage` | Pool drivers, which allocate volumes as paths for the backends to export |
| `internal/sdnotify` | systemd readiness notification |
| `dist/systemd` | Unit template, environment file, Quadlet |
| `test` | End-to-end suites and benchmark |

## How a change is applied

`Manager.mutate` handles every API change the same way:

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

A snapshot and a clone are separate driver calls, because they can differ by pool type: a thin LVM snapshot stays inactive
until it's needed. On reflink both are instant copies of a file, and independent of their source once made.

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

Every change holds one lock, copies the whole state, runs a full reconcile and rewrites
the whole state file. That is expected to limit production-like scale, and hasn't been
measured. See phase 5 in `TODO.md`.
