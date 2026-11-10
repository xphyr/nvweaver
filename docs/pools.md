# Creating storage pools

A pool is where nvweaver allocates volumes from. nvweaver adopts a pool that you've already
created, and never initializes a disk itself, because that destroys what is on it. This page
explains how to create a pool that will work well.

> **Status:** nvweaver can adopt both types. Unless a section says otherwise, the commands were run on Fedora 44 VMs with
> LVM 2.03 and emulated disks, not physical drives.

| | LVM thin | Reflink files |
|---|---|---|
| **Volumes are** | Block devices | Files on a filesystem |
| **Needs** | A disk or partition you can dedicate, and root | A directory on XFS or btrfs |
| **Works with** | Both targets. In a container, it must be privileged, with `--pid=host` | Both targets, including SPDK with no capabilities |
| **Snapshot** | Thin snapshot | Reflink copy |
| **Choose it for** | Behaviour closest to an array | Simplicity, and no extra privileges |

Snapshots and clones are independent on both: deleting a volume doesn't affect its snapshots, or the
other way around.

## LVM thin

**These commands erase the disk you give them.** Check that you have the right one first:

```sh
lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINTS,SERIAL
```

Use a `/dev/disk/by-id` path, because names like `/dev/sdc` can change across reboots.

### Create it

```sh
DISK=/dev/disk/by-id/your-disk

sudo pvcreate --metadatasize 16m "$DISK"
sudo vgcreate nvweaver "$DISK"
sudo lvcreate --type thin-pool -L 900G --poolmetadatasize 1G -n pool0 nvweaver
```

Then check it:

```sh
sudo vgs -o vg_name,vg_size,vg_free,vg_mda_size,vg_mda_free nvweaver
sudo lvs -o lv_name,lv_attr,lv_size,data_percent,metadata_percent nvweaver/pool0
```

### Don't skip these settings

| Setting | Why |
|---|---|
| `--metadatasize 16m` | **Must be set when the disk is initialized, and is hard to change later.** The default area ran out of room at about 1,760 volumes in our testing, as each uses about 288 bytes. With 16 MB, 5,000 volumes left over 7 MB free. |
| `--poolmetadatasize 1G` | Holds the thin pool's mappings, and grows as data is written and snapshots are taken. With 5,000 empty volumes, 1 GB was only 3.5% used, so allow for real data on top of that. |

**Leave room when sizing the pool.** LVM also keeps a spare metadata volume of the same size, so the
pool needs its data size, plus twice the metadata size, plus the metadata area. On an 8 GB disk, a
6 GB pool with 1 GB of metadata didn't fit, failing with `Insufficient free space`.

### Keep the metadata archive small

By default, LVM saves a copy of its metadata in `/etc/lvm/archive` on every change. That reached
375 MB in 2,028 files after 1,000 volumes. With archiving off, it stayed at 12 KB.

nvweaver turns this off for its own commands (`--config backup/archive=0`). It keeps LVM's single backup
of the current metadata, in `/etc/lvm/backup`. Check `/etc/lvm/archive` occasionally if you also run LVM commands yourself, and see
`archive` in `lvm.conf` for how much it keeps.

### Don't let a thin pool fill up

A thin pool can promise more than it has. LVM warns about this and allows it, which is much of the
point of thin provisioning. But if the pool really does fill up, **hosts get I/O errors**. Watch both
numbers, and act well before either reaches 100%:

```sh
sudo lvs -o lv_name,data_percent,metadata_percent nvweaver/pool0
```

To grow the pool, add a disk or use free space in the volume group:

```sh
sudo lvextend -L +100G nvweaver/pool0
sudo lvextend --poolmetadatasize +1G nvweaver/pool0
```

LVM can also extend a pool automatically, with `thin_pool_autoextend_threshold` in `lvm.conf`, which
relies on `dmeventd`. We haven't tested the automatic extension or these `lvextend` commands.

### Adopt it

```sh
curl -X POST $API/v1/pools -d '{"name":"fast","type":"lvm-thin","volume_group":"nvweaver","thin_pool":"pool0"}'
curl -X POST $API/v1/volumes -d '{"name":"vol1","size_bytes":10737418240}'
curl -X POST $API/v1/subsystems/$SUB/namespaces -d '{"volume":"vol1"}'
```

Adopting checks that the thin pool exists, and changes nothing. Forgetting the pool doesn't either. Anything in the volume
group not named `nvw-...` is ignored, so it can be shared with other uses.

### Things to know

- **Thin snapshots aren't active by default.** They have the `k` flag and no device node until activated. nvweaver leaves
  them that way, which keeps boot faster, and makes clones active.
- **Sizes round up to whole extents,** 4 MiB by default. A volume reports its real size, and a retry with the size first
  asked for still matches.
- **Boot takes longer with many volumes.** Activating 5,000 volumes took 55 seconds, and deactivating
  them 28, in a VM. Creating a volume went from 0.11 seconds at 500 volumes to 0.45 at 5,000.
- **Stop this machine's LVM reading clients' data.** See "Clients' LVM", below.
- **The chunk size sets how finely changes are tracked.** A snapshot's changed blocks, for incremental backups, come in
  whole chunks: 1,000 scattered 512-byte writes reported 55 MB at 64 KiB chunks, and nearly the whole volume at 1 MiB. LVM
  picks the chunk size from the pool's size unless `-c` says; for backups, keep it at 64 to 256 KiB. See decision 50.
- **Containers:** the service uses the host's own `lvm`, so the host needs `lvm2`, and `thin_check`: see [Tools](../README.md#tools). In a container, run it with
  `--privileged --pid=host -v /dev:/dev`, and it goes through `nsenter`. Under the systemd unit, it just runs `lvm`. The SPDK
  Quadlet has no capabilities, so it can't use this pool type.

## Reflink files

Each volume is a file, and a snapshot or clone is an instant copy sharing its data until either side
is changed. In our testing, copying a 1 GB file took 14 milliseconds.

### Create it on XFS

**`mkfs` erases the disk you give it.**

```sh
DISK=/dev/disk/by-id/your-disk

sudo mkfs.xfs -m reflink=1 "$DISK"
sudo mkdir -p /srv/nvweaver
sudo mount "$DISK" /srv/nvweaver
```

Add it to `/etc/fstab` so that it mounts at boot, and use `RequiresMountsFor=/srv/nvweaver` in the
service, as described in the [README](../README.md#running-under-systemd). Reflink is on by default
in current `mkfs.xfs`. Check it, including on an existing filesystem:

```sh
sudo xfs_info /srv/nvweaver | grep -o 'reflink=[01]'
```

### Or use btrfs

Any directory on btrfs works. There's nothing to set up, but see the note on `nodatacow` below.

### Check that it works

Whatever the filesystem, this must succeed:

```sh
cd /srv/nvweaver
sudo truncate -s 1M test.img
sudo cp --reflink=always test.img test.clone
sudo rm test.img test.clone
```

### What won't work

| Filesystem | Result |
|---|---|
| **ext4** | `cp` fails with `Operation not supported`: it has no reflink |
| **overlayfs**, including a container's own filesystem | Refused by the kernel target, because hosts see a zero-size disk. Mount a real filesystem instead |
| **btrfs with `nodatacow`** (`chattr +C`) | Expected to be a problem for reflink, and we haven't been able to confirm how. Avoid it on the pool directory |

### Adopt it

```sh
curl -X POST $API/v1/pools -d '{"name":"files","type":"reflink","path":"/srv/nvweaver"}'
curl -X POST $API/v1/volumes -d '{"name":"vol1","size_bytes":10737418240}'
curl -X POST $API/v1/subsystems/$SUB/namespaces -d '{"volume":"vol1"}'
```

Adopting tries a real reflink copy, and refuses the directory if that fails. If the service runs in a container,
mount the pool's directory into it, and use the path as it appears there.

### Things to know

- **The same space warning applies.** Sparse files and shared data mean the volumes can promise more
  than the filesystem has. Hosts get I/O errors when it fills, so watch `df`.
- **btrfs compression makes incremental backups larger.** Changed blocks come from where each file's data is on disk, and a
  compressed extent can't be compared byte by byte, so every one is reported as changed. That's never wrong, only more to
  read. XFS doesn't compress. See decision 50.
- **With SPDK and no capabilities,** mount the directory into the container as a volume. Each volume
  counts towards `fs.aio-max-nr`, as the [README](../README.md#known-limits) explains.

## Clients' LVM

This applies to any block device that's exported, whether or not you use LVM pools. If this machine's LVM can read an exported
device, and a client puts LVM on it, then this machine can treat that LVM as its own:

- **At boot, it activates the client's logical volumes,** which then hold the exported disk open.
- **A client's volume group with the same name as a pool's takes that pool offline.** Its state becomes `missing`, and it refuses changes.
  A client can do this by accident, as names like `vg0` are common.

In testing, hosts kept their connections and their data stayed intact, as nothing here wrote to the client's volumes. Files can't be read as PVs, so
reflink pools and file-backed namespaces aren't affected.

**nvweaver checks for this, and reports it under `warnings` in `GET /v1/system`,** and in its log. It runs `lvmdiskscan` in the background, at startup, every 5 minutes, and
within 30 seconds of a change, so a warning takes a moment to appear or clear. It doesn't change LVM's configuration: only you know which disks are this
machine's own, and getting that wrong can stop it booting. Where it can't reach the host's LVM, as in the SPDK Quadlet, it says that it couldn't check.

What protects a machine:

| Setup | Thin volumes | Exported disks |
|---|---|---|
| A devices file that exists, with `use_devicesfile=1` | Protected | Protected, unless you add them to it |
| `use_devicesfile=1` but no file: Fedora, until the first `pvcreate` or `vgcreate` | Protected by `scan_lvs=0`, the default | **Exposed** |
| `use_devicesfile=0`: the default in Debian 13, and Ubuntu 24.04. Ubuntu 22.04's LVM has no such setting | Protected by `scan_lvs=0`, the default | **Exposed** |
| No devices file in use, with `scan_lvs=1` | **Exposed** | **Exposed** |

So a Fedora machine that only exports disks, and has never used LVM, is exposed too. Those defaults are what each distribution's `lvm2` package ships. The behaviour was tested on Fedora's LVM, version 2.03.38, with the settings changed to match.

**Setting `use_devicesfile=0` removes the devices file.** Every LVM command then deletes it, even `pvs`, leaving a copy in `/etc/lvm/devices/backup`. Setting it back to 1 does nothing until there's a file again, and nvweaver's check will run a command that does this too, like anything else using LVM there.

To fix it, do one of these, **before clients have made any LVM if you can.** Keep the root disk and every other disk this machine's own LVM needs.

- **Use a devices file:** set `use_devicesfile = 1` in `/etc/lvm/lvm.conf`, then run `vgimportdevices -a`. That imports every volume group it can see, including a client's, so
  check that `/etc/lvm/devices/system.devices` lists only this machine's disks. With no volume groups, it does nothing. If this machine has no LVM of its own, `touch` that file instead:
  an empty one hides everything from LVM, as far as `lvmdiskscan` shows, though a reboot with one hasn't been tested.
- **Or filter out exported disks,** in `lvm.conf`, using kernel names and covering partitions, as a client can make those:
  `global_filter = [ "r|^/dev/vdb[0-9]*$|", "r|^/dev/sda[0-9]*$|" ]`. For names like `nvme0n1`, use `p?[0-9]*`.

If a pool has already gone offline, apply the fix and reboot, which also releases the client's logical volumes. That's tested with `global_filter`.
Adding a devices file was only tested before clients had made any LVM. Neither has been tested with this machine's root on LVM, where its initramfs may need updating.
