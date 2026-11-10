#!/usr/bin/env bash
# The CSI driver in the Kubernetes lab, end to end. Needs a lab from k3s-lab.sh up.
#
#   deploy   serve nvweaver on the lab link, with TLS, a token and an LVM thin pool, build the
#            driver's image and load it into the nodes, and install it with the snapshot controller
#   test     volumes through the driver: filesystem and block, data kept across pods and nodes,
#            growing, snapshots and clones, a volume shared by two nodes, and nothing left after
#   sanity   run csi-sanity, the CSI conformance suite, against the driver on agent1, as cluster "sanity"
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
CSI_TEST=v5.6.0
SNAP_URL=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$SNAPSHOTTER
NVW_IP=10.98.0.1
API=https://$NVW_IP:8420
CLUSTER=lab
NS=nvweaver-csi

die() { echo "k3s-csi: $*" >&2; exit 1; }
nvw() { "$LAB" ssh nvweaver "$@"; }
k() { "$LAB" kubectl "$@"; }

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

	echo "The driver"
	k create namespace $NS --dry-run=client -o yaml | k apply -f - >/dev/null
	nvw 'sudo cat /etc/nvweaver-lab/lab.token' >"$WORK/token" && nvw 'sudo cat /etc/nvweaver-lab/tls.crt' >"$WORK/ca.crt" || die "cannot read the token"
	"$LAB" ssh server "sudo k3s kubectl -n $NS delete secret nvweaver-api --ignore-not-found >/dev/null; cat > /tmp/token; " <"$WORK/token"
	"$LAB" ssh server "cat > /tmp/ca.crt" <"$WORK/ca.crt"
	"$LAB" ssh server "sudo k3s kubectl -n $NS create secret generic nvweaver-api --from-literal=url=$API --from-file=token=/tmp/token --from-file=ca.crt=/tmp/ca.crt >/dev/null; rm -f /tmp/token /tmp/ca.crt" ||
		die "cannot create the secret"
	sed -e "s#IMAGE#$CSI_IMAGE#g" -e "s#CLUSTER#$CLUSTER#g" "$ROOT/csi/deploy/nvweaver-csi.yaml" | k apply -f - >/dev/null || die "cannot apply the driver"
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

	echo "== taking everything away"
	k delete volumesnapshot snap --wait=true --timeout=120s >/dev/null
	k delete pvc fs clone ro shared out --wait=true --timeout=120s >/dev/null
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
	podman run --rm -v "$bin:/out:Z" -e GOBIN=/out -e CGO_ENABLED=0 docker.io/library/golang:1.26-alpine \
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

cmd_remove() {
	k delete -f - --ignore-not-found >/dev/null <"$ROOT/csi/deploy/classes.yaml"
	sed -e "s#IMAGE#$CSI_IMAGE#g" -e "s#CLUSTER#$CLUSTER#g" "$ROOT/csi/deploy/nvweaver-csi.yaml" | k delete --ignore-not-found -f - >/dev/null
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
remove) cmd_remove ;;
*)
	sed -n '2,/^set -u/p' "$0" | sed -n 's/^# \{0,1\}//p'
	exit 2
	;;
esac
