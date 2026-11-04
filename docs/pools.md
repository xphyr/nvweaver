# Creating storage pools

A pool is where nvweaver allocates volumes from. nvweaver adopts a pool that you've already
created, and never initializes a disk itself, because that destroys what is on it. This page
explains how to create a pool that will work well.

> **Status:** nvweaver can adopt reflink pools. LVM thin pools are being built, and the steps below are
> the preparation they will need. Unless a section says otherwise, the commands were run on Fedora 44
> VMs with LVM 2.03 and emulated disks, not physical drives.

| | LVM thin | Reflink files |
|---|---|---|
| **Volumes are** | Block devices | Files on a filesystem |
| **Needs** | A disk or partition you can dedicate, and root | A directory on XFS or btrfs |
| **Works with** | The kernel target; SPDK if the service can run LVM | Both targets, including SPDK with no capabilities |
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

nvweaver will turn this off for its own commands (`--config backup/archive=0`), and that's what we
tested. Check `/etc/lvm/archive` occasionally if you also run LVM commands yourself, and see
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

### Things to know

- **Thin snapshots aren't active by default.** They have the `k` flag and no device node until
  activated with `lvchange -ay -K`. nvweaver will handle this.
- **Boot takes longer with many volumes.** Activating 5,000 volumes took 55 seconds, and deactivating
  them 28, in a VM. Creating a volume went from 0.11 seconds at 500 volumes to 0.45 at 5,000.
- **Avoid other LVM on the same host seeing clients' data.** If a client puts LVM on a volume, the
  target host could see it. Fedora's LVM only uses disks in its devices file
  (`/etc/lvm/devices/system.devices`), which should prevent that. On a system without one, set
  `global_filter` in `lvm.conf`. We haven't tested either case.
- **Containers:** LVM is awkward inside a container, because of udev and locking. Run the service
  directly under systemd when using this pool type.

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
- **With SPDK and no capabilities,** mount the directory into the container as a volume. Each volume
  counts towards `fs.aio-max-nr`, as the [README](../README.md#known-limits) explains.
