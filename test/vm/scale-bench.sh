#!/usr/bin/env bash
# Measures how the API copes as configuration grows, on both backends, in a fresh VM. See
# scale-bench.py. Results are in a VM with a small, virtual disk, on loopback: use them to
# find limits and compare changes, not as absolute numbers.
#
# Build the images first, with "make images". BUDGET_SECONDS limits each backend (600). SHAPE is
# node or volume, as in scale-bench.py. BACKENDS picks which to run. SPDK needs fs.aio-max-nr raised for this many volumes.
set -u

LAB=$(dirname "$0")/lab.sh
export BUDGET_SECONDS=${BUDGET_SECONDS:-600}
SHAPE=${SHAPE:-node}
C=nvweaver-scale
API=http://127.0.0.1:8420
NQN=nqn.2026-11.net.xphyr.scale

t() { "$LAB" ssh tgt "$1" </dev/null; }
seconds() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f", b - a }'; }

run_backend() { # BACKEND IMAGE
	local backend=$1 image=$2
	local run="sudo podman run -d --pull=never --name $C --privileged --network host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-scale-data:/var/lib/nvweaver \
		-e NVWEAVER_BACKEND=$backend $image >/dev/null"
	printf '\n=== %s\n' "$backend"
	t "$run" || return 1
	t "for i in \$(seq 1 120); do curl -sf -o /dev/null $API/healthz && exit 0; sleep 0.5; done; exit 1" || {
		t "sudo podman logs $C 2>&1 | tail -5"
		return 1
	}
	t "SHAPE=$SHAPE BUDGET_SECONDS=$BUDGET_SECONDS CONTAINER=$C python3 nvweaver/test/vm/scale-bench.py"

	echo '# --- with this many volumes:'
	if [ "$SHAPE" = node ]; then
		local host=$NQN:h0 sub=$NQN:node0
	else
		local host=$NQN:every sub=$NQN:v0
	fi
	# Ask rather than assume: moving volumes changes how many each node has.
	local want
	want=$(t "curl -s -m 300 $API/v1/subsystems/$sub/namespaces | jq length")
	t "sudo modprobe nvme-tcp; \
		s=\$(date +%s.%N); n=\$(sudo nvme discover -t tcp -a 127.0.0.1 -s 14500 --hostnqn=$host 2>&1 | grep -c '^subnqn.*$NQN:'); e=\$(date +%s.%N); \
		echo \"# discovery for one host: \$n entries in \$(awk -v a=\$s -v b=\$e 'BEGIN{printf \"%.2f\", b-a}')s\"; \
		s=\$(date +%s.%N); sudo nvme connect -t tcp -a 127.0.0.1 -s 14500 -n $sub --hostnqn=$host >/dev/null 2>/tmp/c.err && r=ok || r=\"failed: \$(cat /tmp/c.err)\"; e=\$(date +%s.%N); \
		echo \"# connect to one subsystem: \$r in \$(awk -v a=\$s -v b=\$e 'BEGIN{printf \"%.2f\", b-a}')s\"; \
		for i in \$(seq 1 600); do c=\$(ls /sys/class/nvme-subsystem/*/ 2>/dev/null | grep -cE '^nvme[0-9]+n[0-9]+\$'); [ \"\$c\" -ge $want ] && break; sleep 0.1; done; e=\$(date +%s.%N); \
		echo \"# namespaces visible to the host: \$c of $want in \$(awk -v a=\$s -v b=\$e 'BEGIN{printf \"%.2f\", b-a}')s\"; sudo nvme disconnect -n $sub >/dev/null 2>&1"
	t "sudo podman exec $C sh -c 'cat /proc/1/limits | grep -i \"open files\"; ls /proc/1/fd | wc -l'" | tr '\n' ' ' | sed 's/^/# /; s/$/\n/'
	t "s=\$(date +%s.%N); timeout 600 sudo podman stop -t 500 $C >/dev/null; e=\$(date +%s.%N); \
		echo \"# stop, removing everything from the target: \$(awk -v a=\$s -v b=\$e 'BEGIN{printf \"%.1f\", b-a}')s\"; sudo podman rm -f $C >/dev/null"
	local start
	start=$(date +%s.%N)
	t "$run"
	if t "for i in \$(seq 1 1200); do curl -sf -o /dev/null $API/healthz && exit 0; sleep 0.5; done; exit 1"; then
		echo "# start, restoring everything: $(seconds "$start" "$(date +%s.%N)")s"
		echo "# subsystems restored: $(t "curl -s -m 300 $API/v1/system | jq .subsystems")"
	else
		echo "# start: not healthy after 10 minutes"
		t "sudo podman logs $C 2>&1 | grep -v NOTICE | tail -5" | cut -c1-200
	fi
	t "timeout 600 sudo podman stop -t 500 $C >/dev/null; sudo podman rm -f $C >/dev/null; sudo podman volume rm -f nvweaver-scale-data >/dev/null; sudo modprobe -r nvmet-tcp nvmet 2>/dev/null; true"
}

"$LAB" down >/dev/null 2>&1 </dev/null
"$LAB" up >/dev/null </dev/null || { echo "VMs did not start"; exit 1; }
trap '"$LAB" down >/dev/null 2>&1 </dev/null' EXIT
"$LAB" load tgt localhost/nvweaver:dev >/dev/null || exit 1
"$LAB" load tgt localhost/nvweaver:spdk >/dev/null || exit 1
"$LAB" sync tgt </dev/null || exit 1
t 'sudo sysctl -q -w fs.aio-max-nr=1048576; uname -r; nproc; free -m | sed -n 2p' | tr '\n' ' '
echo

for backend in ${BACKENDS:-nvmet spdk}; do
	case "$backend" in
	nvmet) run_backend nvmet localhost/nvweaver:dev ;;
	spdk) run_backend spdk localhost/nvweaver:spdk ;;
	esac
done
