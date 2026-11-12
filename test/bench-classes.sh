#!/usr/bin/env bash
# Compares the block storage of StorageClasses in a Kubernetes cluster, with fio, from one node: such as a Synology's own
# iSCSI against nvweaver's NVMe/TCP on the same box. Each class gets a raw block volume, filled first, as reading space
# never written flatters whichever storage returns zeros for it. Then the same jobs, in rounds that take the classes in turn,
# so that a busy moment on the storage doesn't land on one class only. The median of the rounds is reported, with every job's
# start and end, to line up with what was recorded on the storage meanwhile (test/synology-capture.sh).
#
#   KUBECONFIG=... test/bench-classes.sh CLASS [CLASS...]
#
# Settings: NODE, the node to run on (any, by default); SIZE (16Gi); RUNTIME, seconds per job (60); RAMP (10); ROUNDS (3);
# OUT, where results go (./bench-<time>); NS, its namespace, made and removed here (nvweaver-bench); FIO_IMAGE.
# It makes nothing outside that namespace, and the volumes go with it.
set -u
[ $# -ge 1 ] || { sed -n '2,/^set -u/p' "$0" | sed -n 's/^# \{0,1\}//p'; exit 2; }
CLASSES=("$@")
NODE=${NODE:-}
SIZE=${SIZE:-16Gi}
RUNTIME=${RUNTIME:-60}
RAMP=${RAMP:-10}
ROUNDS=${ROUNDS:-3}
NS=${NS:-nvweaver-bench}
OUT=${OUT:-./bench-$(date -u +%Y%m%dT%H%M%SZ)}
# Alpine, with fio from its packages, on both architectures. Its version is recorded with the results.
FIO_IMAGE=${FIO_IMAGE:-docker.io/library/alpine:3.22}

# The jobs: name, rw, block size, queue depth, jobs. Queue depth 1 shows latency; deeper, what the storage can sustain.
JOBS=(
	"lat-randread   randread  4k   1  1"
	"lat-randwrite  randwrite 4k   1  1"
	"iops-randread  randread  4k   16 4"
	"iops-randwrite randwrite 4k   16 4"
	"mixed-70-30    randrw    4k   16 4"
	"seq-read       read      128k 16 1"
	"seq-write      write     128k 16 1"
)

k() { kubectl -n "$NS" "$@"; }
die() { echo "bench-classes: $*" >&2; exit 1; }
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/timeline.log"; }

# A pod's name for a class, as a class's name may not suit one.
pod_of() { echo "fio-$(echo "$1" | tr -c 'a-z0-9\n' '-' | cut -c1-40)"; }

# wait_ready POD CLAIM: until the pod runs, or a Warning about it or its claim says it won't.
wait_ready() {
	local why
	for _ in $(seq 1 120); do
		[ "$(k get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && return 0
		# Not the passing ones: detaching from an earlier pod, and, with Immediate binding, scheduling before the claim is bound.
		why=$(k get events --field-selector type=Warning -o jsonpath='{range .items[*]}{.involvedObject.name} {.reason}: {.message}{"\n"}{end}' 2>/dev/null |
			grep -E "^($1|$2) " | grep -E 'Failed|not found|ProvisioningFailed' |
			grep -v -E 'Waiting for detach|unbound immediate PersistentVolumeClaims' | tail -1)
		[ -n "$why" ] && { echo "$why"; return 1; }
		sleep 5
	done
	echo "$1 isn't running after 10 minutes"
	return 1
}

mkdir -p "$OUT" || exit 1
kubectl get ns "$NS" >/dev/null 2>&1 && die "namespace $NS exists already: remove it, or set NS"
for c in "${CLASSES[@]}"; do
	kubectl get storageclass "$c" >/dev/null || die "no StorageClass $c"
done
trap 'log "removing namespace $NS and its volumes"; kubectl delete namespace "$NS" --wait=true --timeout=600s >/dev/null 2>&1' EXIT
kubectl create namespace "$NS" >/dev/null || die "cannot make namespace $NS"
# fio is installed when the pod starts, which needs root: on OpenShift, its anyuid SCC, for this namespace's pods only, by a
# RoleBinding that goes with the namespace.
if kubectl api-resources --api-group=security.openshift.io 2>/dev/null | grep -q securitycontextconstraints; then
	k create rolebinding bench-anyuid --clusterrole=system:openshift:scc:anyuid --serviceaccount="$NS:default" >/dev/null ||
		die "cannot let the pods run as root on OpenShift"
fi
log "classes: ${CLASSES[*]}; size $SIZE; $RUNTIME s per job after $RAMP s; $ROUNDS rounds; node ${NODE:-any}"

# One volume and one pod for each class, the pod idling until it's given jobs.
for c in "${CLASSES[@]}"; do
	p=$(pod_of "$c")
	sel=""
	[ -n "$NODE" ] && sel="  nodeSelector: {kubernetes.io/hostname: $NODE}"
	k apply -f - >/dev/null <<EOF || die "cannot make the claim or pod for $c"
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $p}
spec:
  storageClassName: $c
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  resources: {requests: {storage: $SIZE}}
---
apiVersion: v1
kind: Pod
metadata: {name: $p}
spec:
$sel
  terminationGracePeriodSeconds: 1
  securityContext: {runAsUser: 0}
  containers:
    - name: fio
      image: $FIO_IMAGE
      command: [sh, -c, "apk add --no-cache -q fio jq >/dev/null && fio --version > /tmp/fio-version && sleep infinity"]
      volumeDevices: [{name: v, devicePath: /dev/bench}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: $p}}]
EOF
done
for c in "${CLASSES[@]}"; do
	p=$(pod_of "$c")
	out=$(wait_ready "$p" "$p") || die "$c: $out"
	for _ in $(seq 1 60); do k exec "$p" -- test -s /tmp/fio-version 2>/dev/null && break; sleep 2; done
	k exec "$p" -- test -s /tmp/fio-version || die "$c: fio didn't install in the pod: $(k logs "$p" 2>&1 | tail -2)"
	log "$c: pod $p on $(k get pod "$p" -o jsonpath='{.spec.nodeName}'), $(k exec "$p" -- cat /tmp/fio-version), volume $(k get pvc "$p" -o jsonpath='{.spec.volumeName}')"
done

# Fill each volume, so that every block read has been written.
for c in "${CLASSES[@]}"; do
	p=$(pod_of "$c")
	log "$c: filling the volume"
	k exec "$p" -- fio --name=fill --filename=/dev/bench --rw=write --bs=1M --iodepth=16 --ioengine=libaio --direct=1 \
		--output-format=json --output=/tmp/fill.json >/dev/null || die "$c: filling the volume failed"
	log "$c: filled"
done

# The rounds.
for round in $(seq 1 "$ROUNDS"); do
	for job in "${JOBS[@]}"; do
		read -r name rw bs qd nj <<<"$job"
		for c in "${CLASSES[@]}"; do
			p=$(pod_of "$c")
			log "round $round, $c, $name: start"
			k exec "$p" -- fio --name="$name" --filename=/dev/bench --rw="$rw" --rwmixread=70 --bs="$bs" --iodepth="$qd" \
				--numjobs="$nj" --group_reporting --ioengine=libaio --direct=1 --time_based --runtime="$RUNTIME" \
				--ramp_time="$RAMP" --randrepeat=0 --output-format=json --output="/tmp/$name.json" >/dev/null ||
				die "$c: fio $name failed"
			k exec "$p" -- cat "/tmp/$name.json" >"$OUT/$(echo "$c" | tr '/' '-')-$name-$round.json"
			log "round $round, $c, $name: end"
		done
	done
done

# The medians: IOPS, MB/s, and mean and 99th percentile completion latency, in µs, reads and writes together.
summary() { # FILE: iops MBps mean_us p99_us
	jq -r '.jobs[0] as $j | ($j.read.iops + $j.write.iops) as $iops | (($j.read.bw_bytes + $j.write.bw_bytes) / 1e6) as $mbps
		| ([$j.read, $j.write] | map(select(.iops > 0))) as $d
		| (($d | map(.clat_ns.mean * .iops) | add) / ($d | map(.iops) | add) / 1000) as $mean
		| ($d | map(.clat_ns.percentile["99.000000"] // 0) | max / 1000) as $p99
		| "\($iops) \($mbps) \($mean) \($p99)"' "$1"
}
median() { sort -g | awk '{a[NR]=$1} END {if (NR % 2) print a[(NR+1)/2]; else print (a[NR/2]+a[NR/2+1])/2}'; }
{
	printf '%-16s %-26s %10s %9s %10s %10s\n' job class IOPS MB/s "mean µs" "p99 µs"
	for job in "${JOBS[@]}"; do
		read -r name _ <<<"$job"
		for c in "${CLASSES[@]}"; do
			f=()
			for r in $(seq 1 "$ROUNDS"); do f+=("$OUT/$(echo "$c" | tr '/' '-')-$name-$r.json"); done
			vals=$(for x in "${f[@]}"; do summary "$x"; done)
			printf '%-16s %-26s %10.0f %9.1f %10.1f %10.1f\n' "$name" "$c" \
				"$(echo "$vals" | awk '{print $1}' | median)" "$(echo "$vals" | awk '{print $2}' | median)" \
				"$(echo "$vals" | awk '{print $3}' | median)" "$(echo "$vals" | awk '{print $4}' | median)"
		done
	done
} | tee "$OUT/summary.txt"
log "done: results in $OUT"
