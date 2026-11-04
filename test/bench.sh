#!/usr/bin/env bash
# Measures what NVMe/TCP costs over a null_blk device, which does no real I/O,
# so the numbers show the overhead of the protocol path and not of a disk. It
# also records the CPU used by the control-plane process during the test.
#
# Traffic is over loopback: treat results as an upper bound on protocol overhead
# for this machine, not as a prediction of performance on a real network.
#
# BACKEND=nvmet (default) or spdk. For SPDK, EXTRA_ENV passes settings through, for
# example EXTRA_ENV="-e NVWEAVER_SPDK_POLLING=true".
set -u
ENGINE=${ENGINE:-sudo podman}
BACKEND=${BACKEND:-nvmet}
case "$BACKEND" in
nvmet) IMAGE=${IMAGE:-nvweaver:dev} ;;
spdk) IMAGE=${IMAGE:-nvweaver:spdk} ;;
*) echo "BACKEND must be nvmet or spdk"; exit 2 ;;
esac
EXTRA_ENV=${EXTRA_ENV:-}
TGT=nvweaver-bench-target
INI=nvweaver-bench-client
VOL=nvweaver-bench-data
API=http://127.0.0.1:18421
NQN=nqn.2026-11.net.xphyr.bench
SUB=$NQN:null
HOST=$NQN:host
RUNTIME=${RUNTIME:-20}

ini() { $ENGINE exec "$INI" sh -c "$1"; }
post() { ini "curl -sf -o /dev/null -X POST -d '$2' $API$1" || { echo "POST $1 failed"; exit 1; }; }
cleanup() {
	ini "nvme disconnect -n $SUB >/dev/null 2>&1"
	$ENGINE stop -t 20 "$TGT" >/dev/null 2>&1
	$ENGINE rm -f "$TGT" >/dev/null 2>&1
	[ "$NVMET_WAS_LOADED" = 0 ] && ini "modprobe -r nvmet-tcp nvmet"
	ini "modprobe -r null_blk"
	$ENGINE rm -f "$INI" >/dev/null 2>&1
	$ENGINE volume rm -f "$VOL" >/dev/null 2>&1
}
[ -e /sys/module/nvmet_tcp ] && NVMET_WAS_LOADED=1 || NVMET_WAS_LOADED=0
if [ -e /sys/module/null_blk ]; then echo "null_blk is already loaded; refusing to disturb it"; exit 1; fi

$ENGINE rm -f "$TGT" "$INI" >/dev/null 2>&1
$ENGINE run -d --name "$INI" --privileged --network host -v /lib/modules:/lib/modules:ro -v /dev:/dev \
	docker.io/library/alpine:3.22 sleep infinity >/dev/null || exit 1
trap cleanup EXIT
ini "apk add -q --no-progress nvme-cli curl jq fio kmod util-linux" || exit 1
ini "modprobe null_blk nr_devices=1 gb=64 queue_mode=2 submit_queues=$(nproc) bs=4096" || { echo "null_blk is not available on this kernel"; exit 1; }
$ENGINE run -d --name "$TGT" --privileged --network host -v /lib/modules:/lib/modules:ro -v /dev:/dev \
	-v "$VOL:/var/lib/nvweaver" -e NVWEAVER_LISTEN=127.0.0.1:18421 -e NVWEAVER_BACKEND="$BACKEND" $EXTRA_ENV "$IMAGE" >/dev/null || exit 1
for _ in $(seq 1 40); do ini "curl -sf -o /dev/null $API/healthz" && break; sleep 0.25; done

post /v1/ports '{"id":1,"address":"127.0.0.1","service":14430}'
post /v1/hosts "{\"nqn\":\"$HOST\"}"
post /v1/subsystems "{\"nqn\":\"$SUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
post "/v1/subsystems/$SUB/namespaces" '{"nsid":1,"device_path":"/dev/nullb0"}'
ini "nvme connect -t tcp -a 127.0.0.1 -s 14430 -n $SUB --hostnqn=$HOST >/dev/null" || exit 1
sleep 1
DEV=$(ini "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn)\" = '$SUB' ] && basename \$s/nvme*n*; done")
[ -n "$DEV" ] || { echo "no NVMe device appeared"; exit 1; }

run() { # NAME DEVICE RW BS IODEPTH JOBS
	ini "fio --name=$1 --filename=$2 --rw=$3 --bs=$4 --iodepth=$5 --numjobs=$6 --direct=1 --ioengine=libaio \
		--time_based --runtime=$RUNTIME --group_reporting --output-format=json" |
		$ENGINE exec -i "$INI" jq -r --arg name "$1" '.jobs[0] as $j | ($j.read.iops + $j.write.iops) as $iops | (($j.read.bw_bytes + $j.write.bw_bytes) / 1048576) as $mb |
			(if $j.read.iops > 0 then $j.read else $j.write end) as $d |
			"\($name)\t\($iops|floor)\t\($mb|floor)\t\($d.lat_ns.mean/1000|floor)\t\($d.clat_ns.percentile["99.000000"]/1000|floor)\t\($j.usr_cpu + $j.sys_cpu|floor)"'
}
# CPU ticks used by a process in the target container: 1 is the control plane.
ticks() { $ENGINE exec "$TGT" sh -c "cut -d' ' -f14,15 /proc/$1/stat" | { read -r u s; echo $((u + s)); }; }
cpu() { ticks 1; }

# With nothing connected or running: what the target costs a shared host at rest.
if [ "$BACKEND" = spdk ]; then
	SPDK_PID=$($ENGINE exec "$TGT" sh -c 'cat /proc/1/task/1/children' | awk '{print $1}')
	I0=$(ticks "$SPDK_PID")
	sleep 10
	I1=$(ticks "$SPDK_PID")
	echo "SPDK idle CPU: $(((I1 - I0) / 10))% of one core"
	echo "SPDK resident memory: $($ENGINE exec "$TGT" sh -c "grep VmRSS /proc/$SPDK_PID/status")"
	echo
fi
printf 'test\tIOPS\tMiB/s\tmean_us\tp99_us\tclient_cpu_pct\n'
run direct-4k-randread  /dev/nullb0 randread 4k 32 4
run direct-1m-read      /dev/nullb0 read 1m 8 2
C0=$(cpu)
run tcp-4k-randread     "/dev/$DEV" randread 4k 32 4
run tcp-4k-randwrite    "/dev/$DEV" randwrite 4k 32 4
run tcp-1m-read         "/dev/$DEV" read 1m 8 2
run tcp-1m-write        "/dev/$DEV" write 1m 8 2
run tcp-qd1-4k-randread "/dev/$DEV" randread 4k 1 1
C1=$(cpu)
echo
echo "control plane CPU during all NVMe/TCP tests: $((C1 - C0)) ticks at $($ENGINE exec "$TGT" getconf CLK_TCK 2>/dev/null || echo 100)Hz"
echo "control plane resident memory: $($ENGINE exec "$TGT" sh -c 'grep VmRSS /proc/1/status')"
echo "image size: $($ENGINE image inspect "$IMAGE" --format '{{.Size}}') bytes"
