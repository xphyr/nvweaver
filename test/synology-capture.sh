#!/bin/sh
# Records what a Synology does while test/bench-classes.sh runs against it: a description of the box, once, then every second
# its CPU, memory, disks and network, and every five its busiest processes and containers. Run it as root, in DSM's shell,
# from before the benchmark starts until after it ends, then stop it with Ctrl-C:
#
#   sudo sh synology-capture.sh OUTDIR [INTERFACE]
#
# INTERFACE is the one the hosts reach it on, such as eth1; every interface is sampled if it's left out.
#
# It only reads: /proc, /sys, and Docker's own statistics. It never records a container's environment, which holds
# nvweaver's token. Every line starts with the time, in seconds since 1970, UTC, to line up with the benchmark's timeline.
set -u
OUT=${1:?usage: synology-capture.sh OUTDIR [INTERFACE]}
IFACE=${2:-}
mkdir -p "$OUT" || exit 1
DOCKER=/usr/local/bin/docker
[ -x "$DOCKER" ] || DOCKER=$(command -v docker 2>/dev/null || true)

{
	echo "== when"; date -u
	echo "== box"
	grep -E '^(upnpmodelname|unique)=' /etc.defaults/synoinfo.conf 2>/dev/null
	grep -E 'productversion|buildnumber' /etc.defaults/VERSION 2>/dev/null
	uname -a
	grep -m1 'model name' /proc/cpuinfo; echo "cpus: $(grep -c ^processor /proc/cpuinfo)"
	grep -E 'MemTotal|SwapTotal' /proc/meminfo
	echo "== storage"
	cat /proc/mdstat 2>/dev/null
	df -h 2>/dev/null | grep -E 'Filesystem|/volume'
	grep -E ' /volume[0-9]+ ' /proc/mounts
	for d in /sys/block/sd* /sys/block/nvme*n* /sys/block/sata*; do
		[ -e "$d" ] || continue
		echo "$(basename "$d"): $(cat "$d/size" 2>/dev/null) sectors, rotational $(cat "$d/queue/rotational" 2>/dev/null), model $(cat "$d/device/model" 2>/dev/null | tr -s ' ')"
	done
	echo "== network"
	for i in /sys/class/net/*; do
		n=$(basename "$i")
		case $n in lo | docker* | veth*) continue ;; esac
		echo "$n: speed $(cat "$i/speed" 2>/dev/null) Mb/s, mtu $(cat "$i/mtu"), $(ip -4 -o addr show "$n" 2>/dev/null | awk '{print $4}' | tr '\n' ' ')"
	done
	echo "interface sampled: ${IFACE:-every one}"
	echo "== iSCSI targets and LUNs"
	ls /sys/kernel/config/target/iscsi 2>/dev/null
	ls /sys/kernel/config/target/core 2>/dev/null
	echo "== containers"
	if [ -n "$DOCKER" ]; then
		"$DOCKER" ps --format '{{.Names}} {{.Image}} {{.Status}}'
		for c in $("$DOCKER" ps --format '{{.Names}}'); do
			# Settings that matter, never the environment.
			"$DOCKER" inspect -f '{{.Name}}: network {{.HostConfig.NetworkMode}}, privileged {{.HostConfig.Privileged}}, cap_add {{.HostConfig.CapAdd}}, cap_drop {{.HostConfig.CapDrop}}, cpus {{.HostConfig.NanoCpus}}, memory {{.HostConfig.Memory}}' "$c"
		done
	fi
} >"$OUT/box.txt" 2>&1
echo "described the box in $OUT/box.txt; sampling every second until stopped, with Ctrl-C"

# Every second: CPU (the "cpu" line of /proc/stat, in jiffies, and each core's), memory, disks, network.
echo "time user nice system idle iowait irq softirq steal" >"$OUT/cpu.txt"
echo "time core user nice system idle iowait irq softirq steal" >"$OUT/cores.txt"
echo "time MemFree MemAvailable Buffers Cached Dirty" >"$OUT/memory.txt"
echo "time device reads reads_merged sectors_read ms_reading writes writes_merged sectors_written ms_writing in_flight ms_io weighted_ms" >"$OUT/disks.txt"
echo "time interface rx_bytes rx_packets rx_errors rx_dropped tx_bytes tx_packets tx_errors tx_dropped" >"$OUT/network.txt"
n=0
trap 'echo "stopped after $n seconds"; exit 0' INT TERM
while :; do
	now=$(date +%s)
	awk -v t="$now" '$1 == "cpu" {print t, $2, $3, $4, $5, $6, $7, $8, $9}' /proc/stat >>"$OUT/cpu.txt"
	awk -v t="$now" '$1 ~ /^cpu[0-9]+$/ {print t, $1, $2, $3, $4, $5, $6, $7, $8, $9}' /proc/stat >>"$OUT/cores.txt"
	awk -v t="$now" '{v[$1]=$2} END {print t, v["MemFree:"], v["MemAvailable:"], v["Buffers:"], v["Cached:"], v["Dirty:"]}' /proc/meminfo >>"$OUT/memory.txt"
	awk -v t="$now" '$3 ~ /^(sd[a-z]+|md[0-9]+|nvme[0-9]+n[0-9]+|sata[0-9]+|dm-[0-9]+)$/ {print t, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14}' /proc/diskstats >>"$OUT/disks.txt"
	awk -v t="$now" -v i="$IFACE" 'NR > 2 {sub(/:/, " "); if (i == "" || $1 == i) print t, $1, $2, $3, $4, $5, $10, $11, $12, $13}' /proc/net/dev >>"$OUT/network.txt"
	if [ $((n % 5)) = 0 ]; then
		{ echo "== $now"; top -b -n 1 2>/dev/null | head -25; } >>"$OUT/top.txt"
		[ -n "$DOCKER" ] && "$DOCKER" stats --no-stream --format "$now {{.Name}} cpu {{.CPUPerc}} mem {{.MemUsage}} net {{.NetIO}} block {{.BlockIO}}" >>"$OUT/containers.txt" 2>/dev/null
	fi
	n=$((n + 1))
	sleep 1
done
