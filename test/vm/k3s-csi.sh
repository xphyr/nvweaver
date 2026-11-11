#!/usr/bin/env bash
# The CSI driver in the Kubernetes lab, end to end. Needs a lab from k3s-lab.sh up.
#
#   deploy   serve nvweaver on the lab link, with TLS, a token and an LVM thin pool, build the
#            driver's image and load it into the nodes, and install it with the snapshot controller
#   test     volumes through the driver: filesystem and block, data kept across pods and nodes,
#            growing, snapshots and clones, a volume shared by two nodes, and nothing left after
#   sanity   run csi-sanity, the CSI conformance suite, against the driver on agent1, as cluster "sanity"
#   groups   group snapshots, VolumeGroupSnapshot: consistent across volumes a pod writes to, and refused for a node
#            that can't be paused
#   cbt      changed block tracking, from Kubernetes 1.33: swap the driver for the Helm chart with snapshotMetadata on,
#            and check what a backup client is told through the external-snapshot-metadata sidecar. Run it after the others
#   remove   take the driver, and everything it made, back out
#
# Settings: IMAGE, nvweaver's image (localhost/nvweaver:dev), CSI_IMAGE (localhost/nvweaver-csi:dev),
# SKIP_BUILD=1 to use CSI_IMAGE as it is.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
LAB=$HERE/k3s-lab.sh
IMAGE=${IMAGE:-localhost/nvweaver:dev}
CSI_IMAGE=${CSI_IMAGE:-localhost/nvweaver-csi:dev}
SNAPSHOTTER=v8.7.0
# Helm, for the chart, and the external-snapshot-metadata release, for changed block tracking: pinned. The lister, its example
# client, has no tag of its own; at the release's commit it still used the alpha API, which v1.1.0's CRD doesn't serve, so
# it's built from the later commit that moved it to v1beta1.
HELM_VERSION=v4.2.2
HELM_SHA256=9adafecab4d406853bba163a70e9f104f47dbbf65ce24b7653bae7e36150bcb6
CBT_VERSION=v1.1.0
CBT_COMMIT=ca9890fac37303f89fb5575c24f7fc414bfd80eb
CBT_URL=https://raw.githubusercontent.com/kubernetes-csi/external-snapshot-metadata/$CBT_VERSION
# The last provisioner and resizer for Kubernetes before 1.34, as csi/README.md says.
OLD_PROVISIONER=v5.3.0
OLD_RESIZER=v1.14.0
CSI_TEST=v5.6.0
SNAP_URL=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$SNAPSHOTTER
NVW_IP=10.98.0.1
API=https://$NVW_IP:8420
CLUSTER=lab
NS=nvweaver-csi
CLEANUP=15s
PREFIX=nqn.2026-11.io.github.xphyr.nvweaver:csi:$CLUSTER

die() { echo "k3s-csi: $*" >&2; exit 1; }
nvw() { "$LAB" ssh nvweaver "$@"; }
k() { "$LAB" kubectl "$@"; }

# manifest: the driver's manifest, as csi/README.md says to fill it in, with the older provisioner and resizer below
# Kubernetes 1.34, whose newer releases need it.
manifest() {
	local minor
	minor=$(k version -o json 2>/dev/null | jq -r '.serverVersion.minor' | tr -dc 0-9)
	# The node janitor runs often, so that the test of a node leaving needn't wait five minutes.
	# Group snapshots too, which the manifest leaves off.
	local args=(-e "s#IMAGE#$CSI_IMAGE#g" -e "s#CLUSTER#$CLUSTER#g" -e "s#--mode=controller, #&--node-cleanup-interval=$CLEANUP, #"
		-e "s#--mode=controller, #&--group-snapshots, #")
	if [ -n "$minor" ] && [ "$minor" -lt 34 ]; then
		args+=(-e "s#csi-provisioner:v[0-9.]*#csi-provisioner:$OLD_PROVISIONER#" -e "s#csi-resizer:v[0-9.]*#csi-resizer:$OLD_RESIZER#")
	fi
	sed "${args[@]}" "$ROOT/csi/deploy/nvweaver-csi.yaml"
}

cmd_deploy() {
	"$LAB" status >/dev/null 2>&1 && nvw true || die "no lab; run: $LAB up"

	echo "nvweaver, on $API with TLS and a token"
	nvw 'sudo bash -s' <<EOF || die "cannot set up nvweaver's TLS, token and pool"
set -e
mkdir -p /etc/nvweaver-lab
cd /etc/nvweaver-lab
if [ ! -f tls.key ]; then
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=nvweaver-lab \
		-addext subjectAltName=IP:$NVW_IP -keyout tls.key -out tls.crt 2>/dev/null
	head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > token
	chmod 600 tls.key token
fi
if ! vgs csivg >/dev/null 2>&1; then
	pvcreate -q -y /dev/disk/by-id/virtio-NVWEAVERK3SLVM
	vgcreate -q csivg /dev/disk/by-id/virtio-NVWEAVERK3SLVM
	lvcreate -q -y --type thin-pool -L 16G -n pool0 csivg
fi
EOF
	"$LAB" load nvweaver "$IMAGE" >/dev/null || die "cannot load $IMAGE"
	# A scoped token for the lab's cluster, which the driver uses, and one for another, to show they can't touch each
	# other's volumes. Made by the real command line utility, in the image. The admin token is for the test's own checks.
	nvw "sudo bash -s" <<EOF || die "cannot make the scoped tokens"
set -e
cd /etc/nvweaver-lab
if [ ! -f tokens ]; then
	for name in lab other; do
		podman run --rm --pull=never -v /etc/nvweaver-lab:/out:Z --entrypoint nvweaver $IMAGE token new \$name --token-file /out/\$name.token
	done > tokens
	chmod 600 tokens lab.token other.token
fi
EOF
	nvw "sudo podman rm -f nvweaver-csi-lab >/dev/null 2>&1; sudo podman run -d --pull=never --name nvweaver-csi-lab --privileged --network host --pid=host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-csi-lab:/var/lib/nvweaver -v /etc/nvweaver-lab:/etc/nvweaver-lab:ro \
		-e NVWEAVER_BACKEND=${BACKEND:-nvmet} -e NVWEAVER_LISTEN=$NVW_IP:8420 -e NVWEAVER_API_TOKEN=\$(sudo cat /etc/nvweaver-lab/token) -e NVWEAVER_TOKENS_FILE=/etc/nvweaver-lab/tokens \
		-e NVWEAVER_TLS_CERT=/etc/nvweaver-lab/tls.crt -e NVWEAVER_TLS_KEY=/etc/nvweaver-lab/tls.key $IMAGE >/dev/null" || die "cannot start nvweaver"

	for _ in $(seq 1 60); do [ "$(api GET /healthz)" = 200 ] && break; sleep 1; done
	[ "$(api GET /healthz)" = 200 ] || die "nvweaver didn't start: $(nvw 'sudo podman logs nvweaver-csi-lab 2>&1 | tail -5')"
	api POST /v1/pools '{"name":"csi","type":"lvm-thin","volume_group":"csivg","thin_pool":"pool0"}' >/dev/null
	api POST /v1/ports "{\"id\":1,\"address\":\"$NVW_IP\",\"service\":4420}" >/dev/null
	[ "$(api GET /v1/pools/csi)" = 200 ] || die "no pool"

	if [ -z "${SKIP_BUILD:-}" ]; then
		echo "Building $CSI_IMAGE"
		podman build -q -t "$CSI_IMAGE" "$ROOT/csi" >/dev/null || die "cannot build the driver"
	fi
	"$LAB" load nodes "$CSI_IMAGE" >/dev/null || die "cannot load $CSI_IMAGE into the nodes"

	echo "Snapshot CRDs and controller, $SNAPSHOTTER"
	local f
	for f in snapshot.storage.k8s.io_volumesnapshotclasses snapshot.storage.k8s.io_volumesnapshotcontents snapshot.storage.k8s.io_volumesnapshots \
		groupsnapshot.storage.k8s.io_volumegroupsnapshotclasses groupsnapshot.storage.k8s.io_volumegroupsnapshotcontents groupsnapshot.storage.k8s.io_volumegroupsnapshots; do
		k apply -f "$SNAP_URL/client/config/crd/$f.yaml" >/dev/null || die "cannot apply $f"
	done
	for f in rbac-snapshot-controller setup-snapshot-controller; do
		k apply -f "$SNAP_URL/deploy/kubernetes/snapshot-controller/$f.yaml" >/dev/null || die "cannot apply $f"
	done
	# Its release's manifest runs controller v8.6.0, which leaves group snapshots off, though the README says they're on: a
	# VolumeGroupSnapshot then just sits there, with no status and no error.
	if ! k -n kube-system get deployment snapshot-controller -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q CSIVolumeGroupSnapshot; then
		k -n kube-system patch deployment snapshot-controller --type=json \
			-p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--feature-gates=CSIVolumeGroupSnapshot=true"}]' >/dev/null ||
			die "cannot turn group snapshots on in the snapshot controller"
	fi

	echo "The driver"
	k create namespace $NS --dry-run=client -o yaml | k apply -f - >/dev/null
	nvw 'sudo cat /etc/nvweaver-lab/lab.token' >"$WORK/token" && nvw 'sudo cat /etc/nvweaver-lab/tls.crt' >"$WORK/ca.crt" || die "cannot read the token"
	"$LAB" ssh server "sudo k3s kubectl -n $NS delete secret nvweaver-api --ignore-not-found >/dev/null; cat > /tmp/token; " <"$WORK/token"
	"$LAB" ssh server "cat > /tmp/ca.crt" <"$WORK/ca.crt"
	"$LAB" ssh server "sudo k3s kubectl -n $NS create secret generic nvweaver-api --from-literal=url=$API --from-file=token=/tmp/token --from-file=ca.crt=/tmp/ca.crt >/dev/null; rm -f /tmp/token /tmp/ca.crt" ||
		die "cannot create the secret"
	manifest | k apply -f - >/dev/null || die "cannot apply the driver"
	k apply -f - <"$ROOT/csi/deploy/classes.yaml" >/dev/null || die "cannot apply the classes"
	k -n $NS rollout restart deployment/nvweaver-csi-controller daemonset/nvweaver-csi-node >/dev/null
	k -n $NS rollout status deployment/nvweaver-csi-controller --timeout=300s || die "the controller isn't running"
	k -n $NS rollout status daemonset/nvweaver-csi-node --timeout=300s || die "the node plugin isn't running"
	k -n kube-system rollout status deployment/snapshot-controller --timeout=300s || die "the snapshot controller isn't running"
	echo "Deployed"
}

# api METHOD PATH [BODY]: the status code, from the nvweaver VM, with the token, over TLS.
api() {
	nvw "curl -s -m 30 -o /tmp/k3s-csi.body -w '%{http_code}' --cacert /etc/nvweaver-lab/tls.crt -H \"Authorization: Bearer \$(sudo cat /etc/nvweaver-lab/token)\" -X $1 ${3:+-d '$3'} $API$2" 2>/dev/null
}
bodyq() { nvw "jq -r '$1' /tmp/k3s-csi.body"; }

fails=0
ok() { echo "ok    $1"; }
bad() { echo "FAIL  $1"; [ -n "${2:-}" ] && echo "$2" | sed 's/^/      /'; fails=$((fails + 1)); }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$2], got [$3]"; fi; }
expect_has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1" "[$2] not in [$3]" ;; esac; }

# pvc NAME SIZE MODE [VOLUMEMODE] [extra spec lines]
pvc() {
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1}
spec:
  storageClassName: nvweaver
  accessModes: [$3]
  volumeMode: ${4:-Filesystem}
  resources: {requests: {storage: $2}}
${5:-}
EOF
}

# pod NAME PVC NODE COMMAND [block|fs] [RESTART]: a pod on that node, with the volume at /data, or at /dev/xvda for a block volume.
pod() {
	local vol="volumeMounts: [{name: v, mountPath: /data}]"
	[ "${5:-}" = block ] && vol="volumeDevices: [{name: v, devicePath: /dev/xvda}]"
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  nodeSelector: {kubernetes.io/hostname: $3}
  restartPolicy: ${6:-Never}
  terminationGracePeriodSeconds: 1
  containers:
    - name: c
      image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
      command: [sh, -c, "$4"]
      $vol
  volumes: [{name: v, persistentVolumeClaim: {claimName: $2}}]
EOF
}

# finished POD: waits for it to end, and says how
finished() {
	local phase=
	for _ in $(seq 1 120); do
		phase=$(k get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)
		case $phase in Succeeded | Failed) break ;; esac
		sleep 2
	done
	echo "$phase"
}
logs() { k logs "$1" 2>&1 | tail -5; }
gone() { k delete pod "$@" --wait=true --timeout=120s >/dev/null 2>&1; }

cmd_test() {
	echo "== a filesystem volume, written on one node and read on another"
	pvc fs 64Mi ReadWriteOnce
	pod w1 fs agent1 'echo hello-from-agent1 > /data/f && sync && df -k /data | tail -1'
	expect_eq "a pod writes to a new volume" Succeeded "$(finished w1)"
	expect_eq "the claim is bound" Bound "$(k get pvc fs -o jsonpath='{.status.phase}')"
	local vol
	vol=$(k get pvc fs -o jsonpath='{.spec.volumeName}')
	expect_eq "nvweaver has the volume, named for the cluster" 200 "$(api GET "/v1/volumes/lab-$vol")"
	expect_eq "owned by the cluster's scoped token" lab "$(bodyq .owner)"
	expect_eq "which another cluster's token can't delete" 403 "$(nvw "curl -s -m 30 -o /dev/null -w '%{http_code}' --cacert /etc/nvweaver-lab/tls.crt -H \"Authorization: Bearer \$(sudo cat /etc/nvweaver-lab/other.token)\" -X DELETE $API/v1/volumes/lab-$vol")"
	gone w1
	pod r1 fs agent2 'cat /data/f'
	expect_eq "a pod on another node reads it" Succeeded "$(finished r1)"
	expect_eq "and sees what was written" hello-from-agent1 "$(k logs r1 2>&1)"
	local a2
	a2=$("$LAB" ssh agent2 'printf %s "$(cat /etc/nvme/hostnqn)" | sha256sum | cut -c1-16')
	api GET "/v1/volumes/lab-$vol" >/dev/null
	expect_eq "it's exported in agent2's own subsystem now" "nqn.2026-11.io.github.xphyr.nvweaver:csi:lab:node-$a2" "$(bodyq .mapped_to.subsystem)"

	echo "== growing it"
	k patch pvc fs -p '{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}' >/dev/null
	# The volume grows first, then its filesystem, when a pod next mounts it, and only then does the claim say so.
	local cap=
	for _ in $(seq 1 60); do
		cap=$(k get pv "$vol" -o jsonpath='{.spec.capacity.storage}')
		[ "$cap" = 128Mi ] && break
		sleep 2
	done
	expect_eq "the volume grows" 128Mi "$cap"
	api GET "/v1/volumes/lab-$vol" >/dev/null
	expect_eq "in nvweaver too" 134217728 "$(bodyq .size_bytes)"
	gone r1
	pod g1 fs agent2 "df -k /data | awk 'NR==2 {print \$2}'"
	expect_eq "a pod runs on it" Succeeded "$(finished g1)"
	local kb
	kb=$(k logs g1 2>&1)
	if [ "${kb:-0}" -gt 100000 ]; then ok "and its filesystem grew too ($kb KiB)"; else bad "and its filesystem grew too" "$kb KiB"; fi
	expect_eq "and then the claim says so" 128Mi "$(k get pvc fs -o jsonpath='{.status.capacity.storage}')"
	gone g1

	echo "== a snapshot, and a clone from it"
	k apply -f - >/dev/null <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: snap}
spec: {volumeSnapshotClassName: nvweaver, source: {persistentVolumeClaimName: fs}}
EOF
	local ready=
	for _ in $(seq 1 60); do
		ready=$(k get volumesnapshot snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)
		[ "$ready" = true ] && break
		sleep 2
	done
	expect_eq "the snapshot is ready" true "$ready"
	pvc clone 128Mi ReadWriteOnce Filesystem "  dataSource: {name: snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}"
	pod c1 clone server 'cat /data/f'
	expect_eq "a pod uses the clone" Succeeded "$(finished c1)"
	expect_eq "with the snapshot's data" hello-from-agent1 "$(k logs c1 2>&1)"
	gone c1

	echo "== a filesystem shared read-only by two nodes"
	# Both nodes mount it at once, so each must mount it read-only, or their journals corrupt it: decision 48. A read-only
	# filesystem shows as ro in the pod even though the pod didn't ask, which a read-write staging mount wouldn't.
	pvc ro 128Mi ReadOnlyMany Filesystem "  dataSource: {name: snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}"
	# No double quotes in it: pod puts it in a double-quoted YAML string.
	local n said check="cat /data/f; grep ' /data ' /proc/mounts | cut -d' ' -f4 | cut -d, -f1; touch /data/x 2>/dev/null && echo wrote || echo refused; sleep 300"
	pod ro1 ro agent1 "$check"
	pod ro2 ro agent2 "$check"
	for n in ro1 ro2; do
		said=
		for _ in $(seq 1 90); do
			said=$(k logs $n 2>/dev/null | tr '\n' ' ')
			case $said in *refused* | *wrote*) break ;; esac
			sleep 2
		done
		expect_eq "$n reads the snapshot's data, on a read-only mount, and can't write" "hello-from-agent1 ro refused " "$said"
	done
	gone ro1 ro2

	echo "== a raw block volume shared by two nodes"
	pvc shared 32Mi ReadWriteMany Block
	# Direct I/O, as anything sharing a raw device must use: each node's page cache would otherwise keep what it read first,
	# and udev reads every new device as soon as it appears.
	pod b1 shared agent1 'echo shared-block | dd of=/dev/xvda bs=512 count=1 conv=sync oflag=direct 2>/dev/null && echo written; sleep 300' block
	local said=
	for _ in $(seq 1 90); do
		said=$(k logs b1 2>/dev/null)
		[ "$said" = written ] && break
		sleep 2
	done
	expect_eq "one node writes to it" written "$said"
	# It stays running, as a finished pod's volume is detached from its node.
	pod b2 shared agent2 'dd if=/dev/xvda bs=512 count=1 iflag=direct 2>/dev/null | head -c 12; echo; sleep 300' block
	for _ in $(seq 1 90); do
		said=$(k logs b2 2>/dev/null)
		[ -n "$said" ] && break
		sleep 2
	done
	expect_eq "a second node reads it while the first still has it, and sees what the first wrote" shared-block "$said"
	vol=$(k get pvc shared -o jsonpath='{.spec.volumeName}')
	api GET "/v1/subsystems/nqn.2026-11.io.github.xphyr.nvweaver:csi:lab:vol-lab-$vol" >/dev/null
	expect_eq "both nodes are allowed on its own subsystem" 2 "$(bodyq '.allowed_hosts | length')"
	gone b1 b2

	echo "== nvweaver stopping for a while, then a node rebooting, while a pod writes"
	# Longer than a reconnect attempt, which comes every 10 s. Nodes retry for ever by default (--ctrl-loss-tmo), so a pod's
	# writes wait for nvweaver rather than failing.
	local outage=${OUTAGE:-90} before after
	pvc out 64Mi ReadWriteOnce
	pod o1 out agent1 'i=$(tail -1 /data/log 2>/dev/null || echo 0); while true; do i=$((i+1)); echo $i >> /data/log; sync; sleep 1; done' fs Always
	k wait --for=condition=Ready pod/o1 --timeout=180s >/dev/null 2>&1
	sleep 5
	before=$(k exec o1 -- tail -1 /data/log 2>/dev/null)
	nvw 'sudo podman stop -t 30 nvweaver-csi-lab >/dev/null'
	echo "      nvweaver stopped for ${outage}s"
	sleep "$outage"
	nvw 'sudo podman start nvweaver-csi-lab >/dev/null'
	for _ in $(seq 1 60); do [ "$(api GET /healthz)" = 200 ] && break; sleep 1; done
	local now=
	for _ in $(seq 1 60); do
		now=$(k exec o1 -- tail -1 /data/log 2>/dev/null)
		[ "${now:-0}" -gt "$((before + 5))" ] && break
		sleep 2
	done
	if [ "${now:-0}" -gt "$((before + 5))" ]; then ok "the pod carries on writing when nvweaver is back ($before, then $now)"; else bad "the pod carries on writing when nvweaver is back" "$before, then $now"; fi
	expect_eq "it never restarted" 0 "$(k get pod o1 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
	expect_eq "and its filesystem is still writable" ok "$(k exec o1 -- sh -c 'echo x > /data/probe && echo ok' 2>&1)"

	before=$(k exec o1 -- tail -1 /data/log 2>/dev/null)
	"$LAB" ssh agent1 'sudo systemctl reboot' >/dev/null 2>&1
	echo "      agent1 rebooting"
	sleep 20
	local ready=
	for _ in $(seq 1 90); do
		ready=$(k get node agent1 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
		[ "$ready" = True ] && [ "$(k get pod o1 -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && break
		sleep 4
	done
	expect_eq "agent1 is back" True "$ready"
	after=
	for _ in $(seq 1 60); do
		after=$(k exec o1 -- tail -1 /data/log 2>/dev/null)
		[ "${after:-0}" -gt "${before:-0}" ] && break
		sleep 2
	done
	if [ "${after:-0}" -gt "${before:-0}" ]; then ok "the pod runs again on its volume, carrying on from $before to $after"; else bad "the pod runs again on its volume" "$before, then $after"; fi
	expect_eq "with every earlier line still there" 1 "$(k exec o1 -- head -1 /data/log 2>/dev/null)"
	gone o1

	echo "== each node's host, labelled, and a node leaving the cluster"
	local nqn1 nqn2 sub1 sub2
	nqn1=$("$LAB" ssh agent1 'cat /etc/nvme/hostnqn')
	nqn2=$("$LAB" ssh agent2 'cat /etc/nvme/hostnqn')
	sub1=$PREFIX:node-$(printf %s "$nqn1" | sha256sum | cut -c1-16)
	sub2=$PREFIX:node-$(printf %s "$nqn2" | sha256sum | cut -c1-16)
	api GET "/v1/hosts/$nqn1" >/dev/null
	expect_eq "agent1's host belongs to the cluster's token, and says which cluster and node it is" "lab lab agent1" \
		"$(bodyq '[.owner, .labels["csi-cluster"], .labels["csi-node"]] | join(" ")')"
	# It was away for longer than two of the janitor's checks while it rebooted, but it was still a node.
	expect_eq "agent1 kept its subsystem through its reboot" 200 "$(api GET "/v1/subsystems/$sub1")"
	expect_eq "agent2 has a subsystem" 200 "$(api GET "/v1/subsystems/$sub2")"
	expect_eq "with nothing in it now" 0 "$(bodyq '.namespaces | length')"
	# Drained first, as a node being removed is: stopping k3s leaves its containers running, so a controller or snapshot
	# controller on it would keep its leader lease, unseen, and nothing would replace it. Then its kubelet stops, or it
	# would register the node again at once.
	k drain agent2 --ignore-daemonsets --delete-emptydir-data --force --timeout=180s >/dev/null 2>&1
	"$LAB" ssh agent2 'sudo systemctl stop k3s' >/dev/null 2>&1
	k delete node agent2 --wait=true --timeout=120s >/dev/null
	local code=
	for _ in $(seq 1 40); do
		code=$(api GET "/v1/subsystems/$sub2")
		[ "$code" = 404 ] && break
		sleep 3
	done
	expect_eq "once agent2 has left the cluster, its subsystem goes" 404 "$code"
	expect_eq "and its host" 404 "$(api GET "/v1/hosts/$nqn2")"
	expect_eq "while agent1's stays" 200 "$(api GET "/v1/subsystems/$sub1")"
	"$LAB" ssh agent2 'sudo systemctl start k3s' >/dev/null 2>&1
	local back=
	for _ in $(seq 1 60); do
		back=$(k get node agent2 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
		[ "$back" = True ] && break
		sleep 3
	done
	expect_eq "agent2 joins again" True "$back"
	k -n $NS rollout status daemonset/nvweaver-csi-node --timeout=300s >/dev/null 2>&1
	pvc again 64Mi ReadWriteOnce
	pod a1 again agent2 'echo back > /data/f && cat /data/f'
	expect_eq "and a pod on it uses a new volume" Succeeded "$(finished a1)"
	api GET "/v1/hosts/$nqn2" >/dev/null
	expect_eq "with its host made again, as the cluster's" "lab lab" "$(bodyq '[.owner, .labels["csi-cluster"]] | join(" ")')"
	gone a1

	echo "== taking everything away"
	k delete volumesnapshot snap --wait=true --timeout=120s >/dev/null
	k delete pvc fs clone ro shared out again --wait=true --timeout=120s >/dev/null
	local left=
	for _ in $(seq 1 60); do
		api GET /v1/volumes >/dev/null
		left=$(bodyq '.items | length')
		[ "$left" = 0 ] && break
		sleep 2
	done
	expect_eq "nvweaver has no volumes left" 0 "$left"
	api GET /v1/snapshots >/dev/null
	expect_eq "nor snapshots" 0 "$(bodyq '.items | length')"
	api GET /v1/subsystems >/dev/null
	expect_eq "and no namespaces in any subsystem" 0 "$(bodyq '[.[].namespaces[]] | length')"
	expect_eq "no persistent volumes are left" "" "$(k get pv --no-headers 2>/dev/null)"
	local n
	for n in server agent1 agent2; do
		expect_eq "$n has no NVMe devices or mounts left" "0 0" "$("$LAB" ssh $n 'echo $(ls /dev/nvme*n* 2>/dev/null | wc -l) $(grep -c kubernetes.io/csi /proc/mounts)')"
	done
	echo "$fails failed"
	[ "$fails" -eq 0 ]
}

# csi-sanity calls every CSI method directly, with awkward arguments, against a driver it starts itself. That's a copy of
# the driver's binary from its image, in "all" mode on agent1, with a cluster name of its own, so it can't touch the lab's.
cmd_sanity() {
	local bin=$WORK/bin
	mkdir -p "$bin"
	echo "Building csi-sanity $CSI_TEST"
	podman run --rm -v "$bin:/out:Z" -e GOBIN=/out -e CGO_ENABLED=0 docker.io/library/golang:1.26.10-alpine \
		go install "github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@$CSI_TEST" >/dev/null 2>&1 || die "cannot build csi-sanity"
	local c
	c=$(podman create "$CSI_IMAGE") || die "no $CSI_IMAGE"
	podman cp "$c:/usr/local/bin/nvweaver-csi" "$bin/" && podman rm "$c" >/dev/null || die "cannot copy the driver"
	nvw 'sudo cat /etc/nvweaver-lab/token' >"$bin/token" && nvw 'sudo cat /etc/nvweaver-lab/tls.crt' >"$bin/ca.crt" || die "no token"
	local f
	for f in csi-sanity nvweaver-csi token ca.crt; do
		"$LAB" ssh agent1 "cat > /tmp/$f" <"$bin/$f" || die "cannot copy $f"
	done
	"$LAB" ssh agent1 "sudo bash -s" <<EOF
set -u
d=/var/lib/csi-sanity
# A run that failed can leave its mounts behind, which would fail every test in the next.
for m in \$(findmnt -rn -o TARGET | grep "^\$d/" | sort -r); do umount "\$m"; done
rm -rf \$d && mkdir -p \$d
install -m 0755 /tmp/csi-sanity /tmp/nvweaver-csi \$d/
install -m 0600 /tmp/token /tmp/ca.crt \$d/
rm -f /tmp/csi-sanity /tmp/nvweaver-csi /tmp/token /tmp/ca.crt
NVWEAVER_API_URL=$API NVWEAVER_API_TOKEN_FILE=\$d/token NVWEAVER_CA=\$d/ca.crt \\
	\$d/nvweaver-csi --mode=all --cluster=sanity --endpoint=unix://\$d/csi.sock >\$d/driver.log 2>&1 &
pid=\$!
for _ in \$(seq 1 50); do [ -S \$d/csi.sock ] && break; sleep 0.1; done
\$d/csi-sanity --csi.endpoint=\$d/csi.sock --csi.stagingdir=\$d/staging --csi.mountdir=\$d/mount \\
	--csi.testvolumesize=$((64 << 20)) --csi.testvolumeexpandsize=$((128 << 20)) --ginkgo.no-color 2>&1 | tail -40
rc=\${PIPESTATUS[0]}
kill \$pid
echo "csi-sanity exit \$rc"
[ \$rc -eq 0 ] || tail -30 \$d/driver.log
exit \$rc
EOF
	local rc=$? sub
	# The node's subsystem for the "sanity" cluster stays, as it would for a running driver; this one has stopped.
	"$LAB" ssh agent1 "sudo nvme disconnect -n nqn.2026-11.io.github.xphyr.nvweaver:csi:sanity:node-\$(printf %s \"\$(cat /etc/nvme/hostnqn)\" | sha256sum | cut -c1-16) >/dev/null 2>&1; true"
	api GET /v1/subsystems >/dev/null
	for sub in $(bodyq '.[].nqn | select(startswith("nqn.2026-11.io.github.xphyr.nvweaver:csi:sanity:"))'); do
		api DELETE "/v1/subsystems/$sub" >/dev/null
	done
	return $rc
}

# covered RANGES OFFSET LENGTH: whether one range holds all of [OFFSET, OFFSET+LENGTH).
covered() {
	awk -v s="$2" -v e="$(($2 + $3))" '$1 <= s && $1 + $2 >= e { found = 1 } END { exit !found }' <<<"$1"
}

# lister ARGS: what the upstream example client is told, through the sidecar, as "offset length" lines.
lister() {
	local out
	out=$(k exec cbt-client -- /go/bin/snapshot-metadata-lister -n default -o json "$@" 2>&1) || { echo "ERROR $out"; return; }
	jq -r '.[].block_metadata[] | "\(.byte_offset // 0) \(.size_bytes)"' <<<"$out" 2>/dev/null || echo "ERROR $out"
}

# snapshot NAME: a VolumeSnapshot of the cbt claim, once it's ready.
snapshot() {
	k apply -f - >/dev/null <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: $1, namespace: default}
spec: {volumeSnapshotClassName: nvweaver, source: {persistentVolumeClaimName: cbt}}
YAML
	for _ in $(seq 1 60); do
		[ "$(k get volumesnapshot "$1" -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ] && return 0
		sleep 2
	done
	return 1
}

# A pod writes a counter to two block volumes in turn, each write only once the one before has finished, so at any instant
# the first holds the second's number or one more. A group snapshot of both, restored, must too. Decision 56.
cmd_groups() {
	echo "== group snapshots"
	local n
	for n in server agent1 agent2; do
		expect_eq "$n's node plugin says it can be paused" true \
			"$(k -n $NS get lease "nvweaver-node-$n" -o jsonpath='{.metadata.labels.nvweaver\.xphyr\.github\.io/pause-nvme}' 2>&1)"
	done
	k apply -f - >/dev/null <<EOF
apiVersion: groupsnapshot.storage.k8s.io/v1beta2
kind: VolumeGroupSnapshotClass
metadata: {name: nvweaver}
driver: nvweaver.xphyr.github.io
deletionPolicy: Delete
EOF
	local v
	for v in ga gb; do
		pvc $v 32Mi ReadWriteOnce Block
		k label pvc $v app=cg >/dev/null
	done
	k apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: cgw}
spec:
  nodeSelector: {kubernetes.io/hostname: agent1}
  terminationGracePeriodSeconds: 1
  containers:
    - name: c
      image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
      command: [sh, -c, "i=0; while true; i=$((i+1)); do echo $i | dd of=/dev/xa bs=4096 count=1 conv=sync oflag=direct 2>/dev/null || echo fail-a $i; echo $i | dd of=/dev/xb bs=4096 count=1 conv=sync oflag=direct 2>/dev/null || echo fail-b $i; done"]
      volumeDevices: [{name: a, devicePath: /dev/xa}, {name: b, devicePath: /dev/xb}]
  volumes: [{name: a, persistentVolumeClaim: {claimName: ga}}, {name: b, persistentVolumeClaim: {claimName: gb}}]
EOF
	k wait --for=condition=Ready pod/cgw --timeout=180s >/dev/null 2>&1
	expect_eq "a pod writes to both" Running "$(k get pod cgw -o jsonpath='{.status.phase}')"
	sleep 3
	k apply -f - >/dev/null <<EOF
apiVersion: groupsnapshot.storage.k8s.io/v1beta2
kind: VolumeGroupSnapshot
metadata: {name: cg}
spec:
  volumeGroupSnapshotClassName: nvweaver
  source: {selector: {matchLabels: {app: cg}}}
EOF
	local ready=
	for _ in $(seq 1 60); do
		ready=$(k get volumegroupsnapshot cg -o jsonpath='{.status.readyToUse}' 2>/dev/null)
		[ "$ready" = true ] && break
		sleep 2
	done
	expect_eq "a group snapshot of both, while it writes" true "$ready"
	sleep 2
	expect_eq "the pod's writes all succeeded, paused or not" "" "$(k logs cgw 2>&1 | grep fail | head -3)"
	k delete pod cgw --wait=true --timeout=60s >/dev/null 2>&1
	# Each member snapshot, through its content's handle, which is nvweaver's snapshot, and its volume.
	local snap content handle vol pv claim got=0
	for snap in $(k get volumesnapshot -o jsonpath='{range .items[?(@.metadata.ownerReferences[0].name=="cg")]}{.metadata.name}{"\n"}{end}'); do
		content=$(k get volumesnapshot "$snap" -o jsonpath='{.status.boundVolumeSnapshotContentName}')
		handle=$(k get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
		api GET "/v1/snapshots/$handle" >/dev/null
		vol=$(bodyq .volume)
		pv=${vol#"$CLUSTER"-}
		claim=$(k get pv "$pv" -o jsonpath='{.spec.claimRef.name}')
		pvc "r-$claim" 32Mi ReadWriteOnce Block "  dataSource: {name: $snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}"
		got=$((got + 1))
	done
	expect_eq "it has a snapshot of each" 2 "$got"
	api GET /v1/snapshot-groups >/dev/null
	expect_eq "one group in nvweaver, which paused hosts" "1 true" "$(bodyq '"\(.items | length) \(.items[0].paused_ms > 0)"')"
	k apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: cgr}
spec:
  nodeSelector: {kubernetes.io/hostname: agent2}
  restartPolicy: Never
  containers:
    - name: c
      image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
      command: [sh, -c, "echo $(dd if=/dev/xa bs=4096 count=1 2>/dev/null | tr -d '\\000\\n') $(dd if=/dev/xb bs=4096 count=1 2>/dev/null | tr -d '\\000\\n')"]
      volumeDevices: [{name: a, devicePath: /dev/xa}, {name: b, devicePath: /dev/xb}]
  volumes: [{name: a, persistentVolumeClaim: {claimName: r-ga}}, {name: b, persistentVolumeClaim: {claimName: r-gb}}]
EOF
	expect_eq "a pod reads the restored volumes" Succeeded "$(finished cgr)"
	local a b
	read -r a b <<<"$(k logs cgr 2>&1)"
	if [ "${b:-0}" -gt 0 ] && [ "${a:-0}" -ge "$b" ] && [ "${a:-0}" -le $((b + 1)) ]; then
		ok "and they're consistent with each other ($a and $b)"
	else
		bad "and they're consistent with each other" "ga's holds [$a], gb's [$b]: want the same, or ga's one more"
	fi
	gone cgr
	k delete pvc r-ga r-gb --wait=true --timeout=120s >/dev/null

	# A node that can't be paused, as its Lease says, refuses a group of volumes it uses. The node plugin says so again
	# within the hour, but the label is put back here.
	pod cgp ga agent2 'sleep 300' block Always
	k wait --for=condition=Ready pod/cgp --timeout=180s >/dev/null 2>&1
	k -n $NS label lease nvweaver-node-agent2 nvweaver.xphyr.github.io/pause-nvme=false --overwrite >/dev/null
	k apply -f - >/dev/null <<EOF
apiVersion: groupsnapshot.storage.k8s.io/v1beta2
kind: VolumeGroupSnapshot
metadata: {name: cg2}
spec:
  volumeGroupSnapshotClassName: nvweaver
  source: {selector: {matchLabels: {app: cg}}}
EOF
	local said=
	for _ in $(seq 1 60); do
		said=$(k get volumegroupsnapshotcontent -o jsonpath='{range .items[*]}{.status.error.message}{"\n"}{end}' 2>/dev/null; k get events --field-selector involvedObject.name=cg2 -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null)
		case $said in *"can't be paused"*) break ;; esac
		sleep 2
	done
	expect_has "a group on a node that can't be paused is refused, naming it" "node agent2, which can't be paused" "$said"
	api GET /v1/snapshot-groups >/dev/null
	expect_eq "and nvweaver wasn't asked" 1 "$(bodyq '.items | length')"
	k -n $NS label lease nvweaver-node-agent2 nvweaver.xphyr.github.io/pause-nvme=true --overwrite >/dev/null
	k delete volumegroupsnapshot cg2 --wait=true --timeout=120s >/dev/null 2>&1
	gone cgp

	k delete volumegroupsnapshot cg --wait=true --timeout=120s >/dev/null
	k delete pvc ga gb --wait=true --timeout=120s >/dev/null
	local left=
	for _ in $(seq 1 30); do
		api GET /v1/snapshot-groups >/dev/null
		left=$(bodyq '.items | length')
		[ "$left" = 0 ] && break
		sleep 2
	done
	expect_eq "deleting the group snapshot deletes it in nvweaver" 0 "$left"
	api GET /v1/snapshots >/dev/null
	expect_eq "with its snapshots" 0 "$(bodyq '.items | length')"
	k delete volumegroupsnapshotclass nvweaver >/dev/null
	echo "$fails failed"
	[ "$fails" = 0 ]
}

cmd_cbt() {
	echo "== changed block tracking, through the chart and the external-snapshot-metadata sidecar"
	local minor
	minor=$(k version -o json 2>/dev/null | jq -r '.serverVersion.minor' | tr -dc 0-9)
	[ "${minor:-0}" -ge 33 ] || die "changed block tracking needs Kubernetes 1.33, and this is 1.$minor"

	local cache=${LAB_DIR:-$HOME/nvweaver-vms}/k3s-cache tgz
	tgz=$cache/helm-$HELM_VERSION-linux-amd64.tar.gz
	if [ ! -f "$tgz" ]; then
		mkdir -p "$cache" && curl -fsSL -o "$tgz.part" "https://get.helm.sh/$(basename "$tgz")" || die "cannot download Helm"
		[ "$(sha256sum "$tgz.part" | cut -d' ' -f1)" = "$HELM_SHA256" ] || die "Helm's checksum doesn't match"
		mv "$tgz.part" "$tgz"
	fi
	tar -xzO -f "$tgz" linux-amd64/helm | "$LAB" ssh server 'sudo tee /usr/local/bin/helm >/dev/null && sudo chmod 755 /usr/local/bin/helm' || die "cannot install Helm"
	tar -C "$ROOT/csi/deploy/helm" -cz nvweaver-csi | "$LAB" ssh server 'rm -rf /tmp/chart && mkdir /tmp/chart && tar -xz -C /tmp/chart' || die "cannot copy the chart"
	k apply -f "$CBT_URL/client/config/crd/cbt.storage.k8s.io_snapshotmetadataservices.yaml" >/dev/null || die "cannot apply the SnapshotMetadataService CRD"
	k apply -f "$CBT_URL/deploy/snapshot-metadata-client-cluster-role.yaml" >/dev/null || die "cannot apply the client's role"

	# The same driver, from the chart this time, with the sidecar.
	k delete -f - --ignore-not-found >/dev/null <"$ROOT/csi/deploy/classes.yaml"
	manifest | k delete --ignore-not-found -f - >/dev/null
	"$LAB" ssh server "cat > /tmp/values.yaml" <<YAML
cluster: $CLUSTER
nvweaver: {existingSecret: nvweaver-api}
image: {repository: ${CSI_IMAGE%:*}, tag: ${CSI_IMAGE##*:}, pullPolicy: Never}
storageClasses:
  - {name: nvweaver, pool: csi, port: "", fsType: "", reclaimPolicy: Delete, volumeBindingMode: WaitForFirstConsumer, allowVolumeExpansion: true, default: false, defaultVirt: false}
snapshotMetadata: {enabled: true}
YAML
	"$LAB" ssh server "sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml upgrade --install nvweaver-csi /tmp/chart/nvweaver-csi -n $NS -f /tmp/values.yaml --wait --timeout 5m" ||
		die "the chart didn't install"
	expect_eq "the chart installs the driver with the sidecar, and its SnapshotMetadataService" "nvweaver.xphyr.github.io" \
		"$(k get snapshotmetadataservice nvweaver.xphyr.github.io -o jsonpath='{.metadata.name}' 2>&1)"

	# The upstream example client, allowed by its role, and an account with no role at all.
	k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: cbt-client, namespace: default}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: cbt-nobody, namespace: default}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: cbt-client}
subjects: [{kind: ServiceAccount, name: cbt-client, namespace: default}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: external-snapshot-metadata-client-runner}
---
apiVersion: v1
kind: Pod
metadata: {name: cbt-client, namespace: default}
spec:
  serviceAccountName: cbt-client
  restartPolicy: Never
  containers:
    - name: c
      image: docker.io/library/golang:1.26.10
      command: [sh, -c, "go install github.com/kubernetes-csi/external-snapshot-metadata/tools/snapshot-metadata-lister@$CBT_COMMIT && sleep 3600"]
YAML
	local built=
	for _ in $(seq 1 150); do
		k exec cbt-client -- test -x /go/bin/snapshot-metadata-lister 2>/dev/null && built=yes && break
		sleep 4
	done
	[ -n "$built" ] || die "the example client didn't build: $(k logs cbt-client 2>&1 | tail -5)"

	# Data at known places on a raw block volume, snapshotted before and after.
	pvc cbt 64Mi ReadWriteOnce Block
	pod cbt-w cbt agent1 'dd if=/dev/urandom of=/dev/xvda bs=1M count=4 oflag=direct 2>/dev/null && echo written; sleep 900' block
	local said='' n
	for _ in $(seq 1 90); do
		said=$(k logs cbt-w 2>/dev/null)
		[ "$said" = written ] && break
		sleep 2
	done
	expect_eq "a pod writes 4 MiB at the start of a raw volume" written "$said"
	snapshot cbt-1 && ok "snapshot it" || bad "snapshot it"
	for n in 2560 7680; do # 10 MiB and 30 MiB in, 4 KiB each
		k exec cbt-w -- dd if=/dev/urandom of=/dev/xvda bs=4096 count=1 seek=$n oflag=direct conv=notrunc 2>/dev/null
	done
	snapshot cbt-2 && ok "write 4 KiB at 10 MiB and 30 MiB, and snapshot it again" || bad "snapshot it again"

	local ranges total
	ranges=$(lister -s cbt-2 -p cbt-1)
	case $ranges in ERROR*) bad "a backup client asks what changed" "$ranges" ;; *) ok "a backup client asks what changed, through the sidecar" ;; esac
	covered "$ranges" 10485760 4096 && ok "it's told of the write at 10 MiB" || bad "it's told of the write at 10 MiB" "$ranges"
	covered "$ranges" 31457280 4096 && ok "and of the one at 30 MiB" || bad "and of the one at 30 MiB" "$ranges"
	total=$(awk '{ n += $2 } END { print n + 0 }' <<<"$ranges")
	if [ "$total" -gt 0 ] && [ "$total" -le 2097152 ]; then ok "and of nothing much else ($total bytes)"; else bad "and of nothing much else" "$total bytes"; fi
	ranges=$(lister -s cbt-1)
	covered "$ranges" 0 4194304 && ok "the first snapshot's allocated blocks hold the 4 MiB written" || bad "the first snapshot's allocated blocks hold the 4 MiB written" "$ranges"
	total=$(awk '{ n += $2 } END { print n + 0 }' <<<"$ranges")
	if [ "$total" -gt 0 ] && [ "$total" -le 8388608 ]; then ok "and not the rest of the volume ($total bytes)"; else bad "and not the rest of the volume" "$total bytes"; fi
	# Refused for that reason, not any other: an error alone would pass if nothing worked at all.
	ranges=$(lister -s cbt-2 -p cbt-1 --service-account cbt-nobody --service-account-namespace default)
	case $ranges in
	ERROR*PermissionDenied* | ERROR*Unauthenticated*) ok "an account with no access to the snapshots is refused" ;;
	*) bad "an account with no access to the snapshots is refused, as not allowed" "$ranges" ;;
	esac

	k delete pod cbt-w cbt-client --wait=true --timeout=120s >/dev/null 2>&1
	k delete volumesnapshot cbt-1 cbt-2 --wait=true --timeout=120s >/dev/null
	k delete pvc cbt --wait=true --timeout=120s >/dev/null
	k delete clusterrolebinding cbt-client --ignore-not-found >/dev/null
	k delete serviceaccount cbt-client cbt-nobody --ignore-not-found >/dev/null
	"$LAB" ssh server "sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml uninstall nvweaver-csi -n $NS --wait >/dev/null"
	expect_eq "uninstalling the chart takes its SnapshotMetadataService" "" "$(k get snapshotmetadataservice -o name 2>/dev/null)"
	k delete -f "$CBT_URL/deploy/snapshot-metadata-client-cluster-role.yaml" --ignore-not-found >/dev/null
	k delete -f "$CBT_URL/client/config/crd/cbt.storage.k8s.io_snapshotmetadataservices.yaml" --ignore-not-found >/dev/null
	api GET /v1/volumes >/dev/null
	expect_eq "nvweaver has no volumes left" 0 "$(bodyq '.items | length')"
	echo "$fails failed"
	[ "$fails" = 0 ]
}

cmd_remove() {
	k delete -f - --ignore-not-found >/dev/null <"$ROOT/csi/deploy/classes.yaml"
	manifest | k delete --ignore-not-found -f - >/dev/null
	k delete namespace $NS --ignore-not-found >/dev/null
	# Each node's subsystem and host stay while the driver runs, for its next volume. Taking the driver out takes them too.
	local sub host n
	api GET /v1/subsystems >/dev/null
	for sub in $(bodyq ".[].nqn | select(startswith(\"nqn.2026-11.io.github.xphyr.nvweaver:csi:$CLUSTER:\"))"); do
		api DELETE "/v1/subsystems/$sub" >/dev/null
	done
	for n in server agent1 agent2; do
		host=$("$LAB" ssh $n 'cat /etc/nvme/hostnqn')
		api DELETE "/v1/hosts/$host" >/dev/null
		"$LAB" ssh $n "sudo nvme disconnect-all >/dev/null 2>&1; true"
	done
	nvw 'sudo podman rm -f nvweaver-csi-lab >/dev/null 2>&1; sudo podman volume rm nvweaver-csi-lab >/dev/null 2>&1; true'
	echo "Removed"
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
case "${1:-}" in
deploy) cmd_deploy ;;
test) cmd_test ;;
sanity) cmd_sanity ;;
groups) cmd_groups ;;
cbt) cmd_cbt ;;
remove) cmd_remove ;;
*)
	sed -n '2,/^set -u/p' "$0" | sed -n 's/^# \{0,1\}//p'
	exit 2
	;;
esac
