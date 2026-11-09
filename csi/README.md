# nvweaver CSI driver

A Kubernetes CSI driver that provisions volumes from an nvweaver service's pools and connects nodes to them over NVMe/TCP,
as an enterprise array's driver would. It talks to nvweaver only through its API.

**Status:** new, from v0.4.0. It passes `csi-sanity`, the CSI conformance suite, and an end-to-end test in a k3s lab of three VMs, on both
of nvweaver's targets (`test/vm/k3s-csi.sh`). On a compact OpenShift 4.22 cluster it passed the same checks as restricted pods,
and ran and live-migrated a KubeVirt VM. It hasn't been tested at scale or on Arm yet. See [What isn't done yet](#what-isnt-done-yet).

## What it does

- Creates, deletes and grows volumes, as filesystems (ext4 or XFS) or raw block devices.
- Snapshots volumes, and creates volumes from snapshots or other volumes.
- **Single-node volumes** (ReadWriteOnce, ReadWriteOncePod) go into one NVMe subsystem per node, which the node stays connected
  to. Moving a pod to another node moves the volume to that node's subsystem.
- **Shared volumes** (ReadWriteMany, ReadOnlyMany) get a subsystem of their own, which each node using it is allowed into.
  ReadWriteMany is for **raw block volumes only**: a filesystem written from two nodes would be corrupted. Whatever shares a raw
  device must use direct I/O, as each node's page cache keeps what it read first.
- **Nodes wait out an outage.** If nvweaver stops, I/O pauses until it's back, rather than failing. Nodes keep reconnecting for
  ever by default, as there's one target and no failover.

How it maps Kubernetes onto nvweaver is in decision 45 of [`docs/decisions.md`](../docs/decisions.md), and why it's a module of
its own in decision 44.

## Before you start

### On nvweaver

- **An adopted pool** to create volumes from, and **a port** on an address the nodes can reach, as for any client. See the main
  [README](../README.md) and [`docs/pools.md`](../docs/pools.md).
- **The API over HTTPS**, with `NVWEAVER_TLS_CERT` and `NVWEAVER_TLS_KEY`, as the driver reaches it over the network.
- **A scoped token for this cluster,** so that its driver can only change what it created. Name it as you'll name the cluster
  below:

  ```sh
  nvweaver token new prod --token-file prod.token | sudo tee -a /etc/nvweaver/tokens
  sudo chmod 600 /etc/nvweaver/tokens       # and set NVWEAVER_TOKENS_FILE=/etc/nvweaver/tokens
  ```

  The admin token works too, but then the driver could change anything, including other clusters' volumes. See "Scoped tokens"
  under Security in the main README, and decision 46.

### On every node

- **The kernel's `nvme-tcp` module,** loaded at boot. The node plugin's image brings `nvme-cli` and the filesystem tools.
- **A network route to nvweaver's port**, and to its API from wherever the controller runs.
- **A unique NVMe host NQN in `/etc/nvme/hostnqn`.** This matters: see the next section.

### Every node needs its own host NQN

The driver identifies each node by the NVMe host NQN in its `/etc/nvme/hostnqn`, as an array does. It's the node's ID in
Kubernetes, and what nvweaver allows into that node's subsystem. **If two nodes have the same one, nvweaver can't tell them
apart:** they share one subsystem, so every node is allowed to every one of their volumes, and nothing reports an error.

It happens on nodes made from one image, when the image already has the file: the host NQN is meant to be made on each machine
the first time it starts. **Check before installing**, on every node:

```sh
cat /etc/nvme/hostnqn                       # on each node, by SSH

# OpenShift
for n in $(oc get nodes -o name); do
  echo "$n $(oc debug -q "$n" -- chroot /host cat /etc/nvme/hostnqn 2>/dev/null)"
done
```

Every line must differ. If any are the same, give each node its own before installing the driver, along with a host ID:

```sh
nvme gen-hostnqn > /etc/nvme/hostnqn        # on each node with a copy, as root
uuidgen > /etc/nvme/hostid
```

Then make sure the image, or whatever builds the nodes, stops handing out the same one.

**On OpenShift,** a fresh cluster has been seen with the same host NQN and host ID on every node: they come with the image.
Don't change them by hand, as nodes are managed by the Machine Config Operator. Apply a MachineConfig that makes each node's at
every boot, from [`deploy/openshift/`](deploy/openshift/):

- **`99-worker-custom-nvme-hostnqn.yaml`** for clusters with separate worker nodes, which run the driver's volumes.
- **`99-master-custom-nvme-hostnqn.yaml`** for compact clusters, where the control plane nodes are the workers too. There, every
  node is in the `master` pool and the `worker` pool has none, so a worker MachineConfig changes nothing. Check with
  `oc get mcp`: the pool whose MACHINECOUNT covers your nodes is the one to apply to. A cluster with both control plane and
  worker nodes that run workloads needs both.

```sh
oc apply -f csi/deploy/openshift/99-master-custom-nvme-hostnqn.yaml     # or the worker one
oc get mcp -w                                # wait for UPDATED to be True: each node reboots, one at a time
```

Each runs `nvme gen-hostnqn` at boot, which makes the NQN from the machine's DMI product UUID, so it's the same every time a node
boots and different on every node, and sets the host ID to the same UUID. Then run the check above again: every line must
differ. It's been applied to a compact three-node OpenShift 4.22 cluster on vSphere: see `docs/testing.md`.

Changing a node's host NQN once it has volumes cuts it off from them, as nvweaver only lets in the NQN it knew. Fix it first.

## Installing

The image is published for amd64 and arm64 as `ghcr.io/xphyr/nvweaver-csi`, tagged with each release, such as `0.4.0`. To build
your own instead, from the repository's root, push it to a registry the nodes can pull from:

```sh
podman build -t registry.example/nvweaver-csi:dev csi
podman push registry.example/nvweaver-csi:dev
```

Give the driver the API's address, token and CA, in a secret:

```sh
kubectl create namespace nvweaver-csi
kubectl -n nvweaver-csi create secret generic nvweaver-api \
  --from-literal=url=https://storage.example:8420 --from-file=token=prod.token --from-file=ca.crt=ca.crt
```

Snapshots need the snapshot CRDs and controller from
[kubernetes-csi/external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter), which many clusters already have.
The lab installs v8.7.0's: see `cmd_deploy` in `test/vm/k3s-csi.sh`.

Then the driver, with `IMAGE` and `CLUSTER` filled in. `CLUSTER` is a name for this cluster, 1 to 40 lowercase letters, digits
and dashes, that starts every volume, snapshot and subsystem name the driver makes, so several clusters can share one nvweaver.
Use the scoped token's name:

```sh
sed -e 's#IMAGE#ghcr.io/xphyr/nvweaver-csi:0.4.0#' -e 's#CLUSTER#prod#' csi/deploy/nvweaver-csi.yaml | kubectl apply -f -
kubectl -n nvweaver-csi rollout status deployment/nvweaver-csi-controller
kubectl -n nvweaver-csi rollout status daemonset/nvweaver-csi-node
```

**On OpenShift,** the node plugin also needs the privileged SCC, as it connects to NVMe and mounts on the node:

```sh
oc adm policy add-scc-to-user privileged -z nvweaver-csi-node -n nvweaver-csi
```

Last, a StorageClass and a VolumeSnapshotClass. Edit `pool` in [`deploy/classes.yaml`](deploy/classes.yaml) first:

```sh
kubectl apply -f csi/deploy/classes.yaml
```

### With KubeVirt or OpenShift Virtualization

KubeVirt's Containerized Data Importer (CDI) keeps a StorageProfile for each StorageClass, saying what to ask for when it makes
a VM's disk. It fills one in only for drivers it knows, which this isn't, so apply
[`deploy/openshift/storageprofile.yaml`](deploy/openshift/storageprofile.yaml) after the StorageClass. Rename it if you rename
the class:

```sh
oc apply -f csi/deploy/openshift/storageprofile.yaml
oc get storageprofile nvweaver -o yaml      # status: should show the claim property sets and csi-clone
```

Apply it again whenever the StorageClass is recreated: CDI resets it.

**Make it KubeVirt's default class,** or a VM's disk that names no class goes to the cluster's default class instead, and a
clone from an nvweaver golden image becomes a full copy onto other storage:

```sh
oc annotate storageclass nvweaver storageclass.kubevirt.io/is-default-virt-class=true
```

**On a cluster that's running, that moves OpenShift Virtualization's own golden images too,** such as RHEL and Fedora, straight
away: CDI deletes their existing claims on the old default class, and imports them again onto nvweaver, where the StorageProfile
keeps them as VolumeSnapshots rather than claims. Nothing is lost, but they no longer appear among the cluster's claims, and VMs
being created from them meanwhile have to wait. Make the change when nothing depends on them, or set `storageClassName` on each
DataVolume instead.

**Golden images:** [`deploy/openshift/data-import-cron.yaml`](deploy/openshift/data-import-cron.yaml) keeps Fedora's latest
container disk as a snapshot in nvweaver, under the DataSource `fedora`, updated weekly. A VM whose disk names
`sourceRef: {kind: DataSource, name: fedora}` gets a thin clone of it in a fraction of a second.

`oc apply` warns that the StorageProfile has no `last-applied-configuration`, as CDI made it: that's expected.

- **Disks are raw block, ReadWriteMany by preference,** as live migration needs a disk that both nodes can use at once. The
  driver gives such a volume a subsystem of its own. KubeVirt opens raw block disks with direct I/O, which a shared device
  needs. ReadWriteOnce block, then filesystem, are the fallbacks.
- **Clones are the driver's own (`csi-clone`):** thin on LVM, a reflink copy on XFS, with no data copied.
- **Golden images from DataImportCrons are kept as snapshots,** and each VM's disk is a thin clone of one.

A VM, a live migration and a DataImportCron have passed on OpenShift 4.22. **After a live migration, the VM's disk stalls for
about 2 seconds** on nvweaver's kernel target: taking the old node off the volume briefly drops the new node's connection too,
and the node waits its reconnect delay, `--reconnect-delay`, before carrying on. The VM sees no error.

### StorageClass parameters

| Parameter | Meaning |
|---|---|
| `pool` | The nvweaver pool to create volumes in. It can be left out if there's only one. |
| `port` | The ID of the nvweaver port to offer volumes on. 1 by default, or what the driver's `--port` says. |

Filesystems are ext4 unless the StorageClass's `csi.storage.k8s.io/fstype` says `xfs`.

### A dedicated storage network

nvweaver can serve several networks at once, with one port on each address, and each StorageClass chooses its port. So a
cluster with a dedicated storage network, often with jumbo frames, can use it, while other clients use the routed network, and
nvweaver's API stays on the routed network either way:

```sh
nvweaver port create --id 2 --address 172.16.20.60      # nvweaver's address on the storage network
```

Then set `port: "2"` in the StorageClass. Nodes connect from whichever of their interfaces reaches that address, so each needs an
address on that network. Jumbo frames are optional: TCP settles on the smaller MTU of the two ends. Every device on the path has to
pass them, though: if the nodes and nvweaver use 9000 but a switch passes only 1500, connections stall. Check from a node with
`ping -M do -s 8972 <nvweaver's address>`.

**Moving a cluster from one port to another:** a StorageClass's parameters can't be changed, so delete and recreate it, with no
volumes using it. Then remove each node's connection to its old subsystem, `nvme disconnect -n <prefix>:<cluster>:node-…` on the
node, and delete those subsystems in nvweaver, as nodes keep using a connection that's still up rather than adding a new path. The
driver makes them again, on the new port, for each node's next volume. On OpenShift, apply the StorageProfile again too, as
recreating the class resets it. A way to do this without the manual steps is in `TODO.md`.

### Driver flags

Set in `deploy/nvweaver-csi.yaml`, on the `driver` containers.

| Flag | Default | Meaning |
|---|---|---|
| `--cluster` | `k8s` | Starts every name the driver makes. Give each cluster sharing an nvweaver its own. |
| `--port` | `1` | The nvweaver port that volumes are offered on, unless a StorageClass says otherwise. |
| `--ctrl-loss-tmo` | `-1` | Seconds a node keeps reconnecting to nvweaver before giving its volumes up. -1 is for ever. |
| `--reconnect-delay` | `2` | Seconds between a node's attempts to reconnect, which is how long I/O stalls when a connection drops. |
| `--max-volumes-per-node` | `1024` | What each node tells Kubernetes it can take: the namespaces a subsystem can hold. |
| `--nqn-prefix` | `nqn.2026-11.io.github.xphyr.nvweaver:csi` | How the NQN of each subsystem the driver makes starts. |
| `--hostnqn-file` | `/etc/nvme/hostnqn` | The node's host NQN, which is its ID. |
| `--driver-name` | `nvweaver.xphyr.github.io` | The driver's name in Kubernetes. |

**`--cluster`, `--nqn-prefix` and `--driver-name` can't change once volumes exist:** the driver finds its volumes by them, so
changing one orphans every volume it made.

## Moving a cluster from the admin token to a scoped one

What a driver made with the admin token has no owner, so a scoped token isn't allowed to change it. Give the cluster's volumes,
snapshots and subsystems to its token first, as an admin, then change the secret:

```sh
nvweaver volume list -o json | jq -r '.[] | select(.name | startswith("prod-")) | .name' |
  xargs -I{} nvweaver volume update {} --owner prod
nvweaver subsystem list -o json | jq -r '.[] | select(.nqn | contains(":csi:prod:")) | .nqn' |
  xargs -I{} nvweaver subsystem update {} --owner prod
```

Snapshots can't be given away yet: delete them, or leave them for an admin to delete. A way to do all of this in one step is in
`TODO.md`.

## Uninstalling

Delete the PersistentVolumeClaims first, so the driver deletes their volumes. Then:

```sh
kubectl delete -f csi/deploy/classes.yaml
sed -e 's#IMAGE#x#' -e 's#CLUSTER#prod#' csi/deploy/nvweaver-csi.yaml | kubectl delete -f -
kubectl delete namespace nvweaver-csi
```

Each node's subsystem and host stay in nvweaver, as the driver keeps them for the node's next volume, and each node stays
connected to its subsystem. Delete the subsystems, named `<nqn-prefix>:<cluster>:node-…`, with `nvweaver subsystem delete`,
then disconnect each node with `nvme disconnect -n` and the same name.

## Troubleshooting

- **The driver's logs:** `kubectl -n nvweaver-csi logs deploy/nvweaver-csi-controller -c driver`, and for a node,
  `kubectl -n nvweaver-csi logs <node plugin pod> -c driver`. Every failed call is logged with its gRPC code.
- **What a node is connected to:** `nvme list-subsys` on the node.
- **What nvweaver has:** `nvweaver volume list`, `nvweaver subsystem list`, and `nvweaver connection list --subsystem NQN`. The
  OWNER column shows which cluster's token made each.
- **A volume stuck attaching:** check that the node can reach nvweaver's port, and that its host NQN isn't shared with another
  node's.
- **`403` in the controller's log:** the token doesn't own what it was asked to change. See
  [Moving a cluster](#moving-a-cluster-from-the-admin-token-to-a-scoped-one).

## What isn't done yet

All tracked in `TODO.md`:

- Tested on Arm, and at production-like scale
- DH-HMAC-CHAP for nodes
- Refusing to run when two nodes share a host NQN, rather than relying on the check above
- Topology and capacity reporting
- Changed block tracking, for incremental backups
- Removing a node's subsystem when the node leaves the cluster
