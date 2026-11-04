# Storage API design

**Status: agreed, and not built yet.** It moves into `api/openapi.yaml` as it's built, where the sync tests
hold the code to it. The reasoning behind the model is in
[decisions.md](decisions.md), entries 19 to 24.

## The model

```
Pool ──> Volume (owns UUID, NGUID, internal name, state)
            │  └──> Snapshot ──> a clone is a new Volume made from one
            │
            └── mapped into ──> Subsystem (usually one per node)
                (a Namespace: the NSID and the mapping)
```

## Pools

```
GET    /v1/pools
POST   /v1/pools                adopt an existing pool
GET    /v1/pools/{pool}
PATCH  /v1/pools/{pool}         labels and warning level
DELETE /v1/pools/{pool}         forget it; refused while it has volumes or snapshots
```

Adopting an LVM thin pool, or a directory for reflink files:

```json
{"name": "fast", "type": "lvm-thin", "volume_group": "nvweaver", "thin_pool": "pool0",
 "labels": {"media": "nvme"}, "warning_percent": 80}

{"name": "files", "type": "reflink", "path": "/srv/nvweaver"}
```

A pool's state and capacity come back with it:

```json
{"name": "fast", "type": "lvm-thin", "volume_group": "nvweaver", "thin_pool": "pool0",
 "labels": {"media": "nvme"}, "state": "online", "warnings": [],
 "capacity": {"total_bytes": 966367641600, "used_bytes": 12884901888, "provisioned_bytes": 2199023255552,
              "used_percent": 1.3, "metadata_used_percent": 3.5},
 "volumes": 120, "snapshots": 8}
```

- **Adopting changes nothing on disk.** It checks that the pool exists and is usable, and refuses a
  directory on overlayfs or without working reflink, or an LVM pool that this service can't manage.
- **`warnings` report problems early:** the warning level, a low LVM metadata area, and files or
  volumes with our naming prefix that nothing refers to.
- **`state`** is `online`, or `missing` if it can't be found, say because a disk is absent at boot.
- **Forgetting a pool** deletes nothing, and is refused until its volumes and snapshots are gone.

## Volumes

```
GET    /v1/volumes              optional ?pool=
POST   /v1/volumes              create, or clone with "source"
GET    /v1/volumes/{name}
PATCH  /v1/volumes/{name}       grow it, or change labels
DELETE /v1/volumes/{name}       refused while it is mapped
```

```json
{"name": "pvc-1234", "pool": "fast", "size_bytes": 10737418240, "labels": {"cluster": "lab1"}}

{"name": "pvc-5678", "pool": "fast", "source": {"snapshot": "snap-1"}}
{"name": "pvc-9abc", "pool": "fast", "source": {"volume": "pvc-1234"}}
```

```json
{"name": "pvc-1234", "pool": "fast", "size_bytes": 10737418240, "state": "online",
 "uuid": "...", "nguid": "...", "internal_name": "nvw-4f3a9c1e8d2b7a60",
 "device_path": "/dev/nvweaver/nvw-4f3a9c1e8d2b7a60", "source": null, "labels": {"cluster": "lab1"},
 "mapped_to": {"subsystem": "nqn.2026-11.lab:node1", "nsid": 3}, "created_at": "2026-11-04T20:00:00Z"}
```

- **Create is idempotent by name.** The same request again returns the existing volume with `200`, and
  a new one returns `201`. The same name with different settings is `409`, because CSI retries.
- **A clone gets a new UUID and NGUID,** and defaults to the source's size. It may be larger.
- **`internal_name`** is what's on disk, made from random characters, so user-chosen names never need
  to be safe for LVM or filesystems. The prefix also tells us what belongs to us in a shared volume group.
- **Deleting** works with snapshots present, which keep their data, and is refused while mapped.

## Snapshots

```
GET    /v1/snapshots            optional ?volume=
POST   /v1/snapshots
GET    /v1/snapshots/{name}
DELETE /v1/snapshots/{name}
```

```json
{"name": "snap-1", "volume": "pvc-1234"}
```

```json
{"name": "snap-1", "volume": "pvc-1234", "pool": "fast", "size_bytes": 10737418240, "state": "online",
 "internal_name": "nvw-82c1d7e4f09a3b56", "created_at": "2026-11-04T20:05:00Z"}
```

- **Snapshots are top-level, not under their volume.** This is a correction to the earlier proposal:
  a snapshot can outlive its volume, and then a URL under that volume would no longer work.
- **Creating is idempotent by name,** as with volumes.
- **Read-only.** To use one, clone it to a new volume. Group snapshots could later be added as their own
  resource, such as `/v1/snapshot-groups`, without changing any of this.

## Mapping a volume into a subsystem

The namespace API gains a third kind of backing, alongside `device_path` and `file`:

```
POST /v1/subsystems/{nqn}/namespaces     {"volume": "pvc-1234"}
```

- **Identity comes from the volume:** `uuid` and `nguid` can't be given here. So moving a volume to another
  node, by deleting the namespace and creating it elsewhere, keeps what hosts recognise it by.
- **One subsystem at a time.** Mapping a volume that is already mapped elsewhere is `409`. Sharing doesn't
  need more: see below.
- **Deleting the namespace** unmaps the volume and leaves it, and its data, in place.

## Sharing a volume between nodes

Kubernetes RWX block volumes and KubeVirt live migration need one volume on several nodes at once. A
volume stays in one subsystem, and that subsystem allows several hosts. No new API is needed, because hosts
can already be added and removed while a subsystem is in use:

```
POST /v1/subsystems                                {"nqn": "nqn...:pvc-1234", "allowed_hosts": ["node1"], "ports": [1]}
POST /v1/subsystems/nqn...:pvc-1234/namespaces     {"volume": "pvc-1234"}
PUT  /v1/subsystems/nqn...:pvc-1234/hosts/node2    the destination of a migration, with node1 still running
```

- **Each shared volume gets its own subsystem,** so a host only ever sees the shared volumes it was given.
  Trident uses one shared subsystem for many volumes instead, which needs fewer connections, but every
  host in it sees every volume in it.
- **Raw block only.** A normal filesystem can't safely be mounted on two nodes at once. That's what CSI's
  file-based RWX would need, and it isn't in scope.
- **Nothing coordinates writers.** The hosts have to, as QEMU does during a migration.
- **Tested on both backends:** adding a host during another's I/O, without disturbing it, and each host reading
  what the other wrote. Not yet tested with KubeVirt itself, or with NVMe reservations.

### Removing a host differs between backends

| | Kernel target | SPDK |
|---|---|---|
| New connections from a removed host | Refused | Refused |
| **A connection it already has** | **Stays, and can still write** | Dropped, and I/O stops |

So on the kernel target, removing a host doesn't cut off a connection it already has. A CSI driver disconnects
the host itself, so this matters most for a host that's misbehaving or unreachable. To force it, the plan needs one
of these, neither tested yet: detach the subsystem from its port and reattach it, which drops every host and
lets only allowed ones return, or remove the namespace, which also affects the other hosts.

## Things that stay as they are

- **`device_path` and `file` namespaces** don't change, with the identity on the namespace. `file` could
  become a default pool before 1.0.
- **Existing configuration** keeps working, so no saved state needs migrating for those.
- **`GET /v1/system`** adds the pool types this service can use in `capabilities`.

## Behaviour under load

- **Slow storage work doesn't hold the global lock.** A request reserves its name under the lock, does the
  `lvcreate` or copy, then records the result, with an intent log so that a crash can be cleaned up.
- **Incremental reconcile:** a change only touches its own subsystem, and changes to one subsystem can be
  batched.
- **`503` with `Retry-After`** when too much is pending, rather than queuing without limit.
- **First version is synchronous.** A correction to the earlier proposal: thin snapshots and clones took
  under half a second at 5,000 volumes in testing, and reflink copies milliseconds, so nothing needs `202`
  and polling yet. The `state` field leaves room for `creating` if a slower operation arrives.

## Errors

| Status | When |
|---|---|
| `409` | Name exists with different settings; volume mapped or still in use; pool still has volumes |
| `422` | Invalid request; source in a different pool; shrinking a volume; pool type not usable here |
| `503` | Too much pending, with `Retry-After` |
| `507` | The pool doesn't have the room. Only for real exhaustion, as thin pools may overcommit |

## Not in the first version

- Creating pools from raw disks
- Restoring a volume from a snapshot in place: clone it instead
- Clones across pools
- Mapping a snapshot directly
- Limits on overcommitment, or per-pool volume sizes
- Importing existing LVs or files as volumes
- Group snapshots

## Decisions

1. **Names:** volume and snapshot names are unique across the service.
2. **Default pool:** `pool` can be left out when exactly one pool exists. Otherwise it's required.
3. **Pagination:** on volumes and snapshots from the start.
4. **Removing a host forces its connections off,** on both backends. On the kernel target, this probably means
   reattaching the subsystem to its ports, which interrupts other hosts briefly, so the method needs testing.

5. **LVM in a container goes through the host's own `lvm`,** using `nsenter` and `--pid=host`, so that there is one
   code path and LVM's version and configuration are always the host's. The service then needs `lvm2` on the host, and
   the privileges it already has. Containers with no capabilities can't manage LVM pools, and use reflink pools.

### How that was decided

Pools are created on the host, but the service has to create volumes in them. Tested in a VM, with the host's udev
running and a Debian container:

| Approach | Result |
|---|---|
| A. LVM in the image, privileged, `/dev` shared | Fails: `/dev/mapper/nvweaver-pool0_tmeta: open failed` |
| B. As A, plus host IPC for udev sync | Fails the same way |
| C. As B, plus `/etc/lvm` and the lock directories shared | Fails the same way |
| D. Privileged, `--pid=host`, running the host's `lvm` through `nsenter` | **Works:** create, snapshot, activate, delete |
| E. No capabilities | Fails: it can't reach device-mapper |
| F. An LV made on the host, passed to an unprivileged SPDK container with `--device` | Works today, without snapshots |

A to C might work with more LVM settings changed, but they'd also have two LVM versions and two sets of configuration
acting on one volume group. A host agent was also considered, as the only way to give LVM to a container with no
capabilities, but nothing needs that combination.

The combination of D with `/dev` shared, as the service runs, hasn't been tested yet.
