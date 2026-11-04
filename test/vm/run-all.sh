#!/usr/bin/env bash
# Runs every end-to-end suite in fresh VMs, and fails if any of them does. The images must
# already be built: see "make images". Logs go to RESULTS (default ~/nvweaver-vms/results).
set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LAB=$ROOT/test/vm/lab.sh
RESULTS=${RESULTS:-${LAB_DIR:-$HOME/nvweaver-vms}/results}
mkdir -p "$RESULTS" || exit 1
failed=0

report() { # NAME EXIT-STATUS LOG
	printf '%-16s %s\n' "$1" "$(tail -1 "$3")"
	if [ "$2" != 0 ]; then
		failed=1
		grep -E '^  FAIL' "$3" | head -20
	fi
}

# These VMs are started and deleted by vm-e2e.sh itself.
"$ROOT/test/vm/vm-e2e.sh" >"$RESULTS/vm.log" 2>&1 </dev/null
report vm-e2e $? "$RESULTS/vm.log"

"$LAB" down >/dev/null 2>&1 </dev/null
"$LAB" up >/dev/null </dev/null || { echo "VMs did not start"; exit 1; }
trap '"$LAB" down >/dev/null 2>&1 </dev/null' EXIT
for image in dev spdk systemd-test; do
	"$LAB" load tgt "localhost/nvweaver:$image" >/dev/null || { echo "cannot load $image"; exit 1; }
done
"$LAB" sync tgt </dev/null || exit 1

in_vm() { "$LAB" ssh tgt "cd nvweaver && SKIP_BUILD=1 $1" </dev/null; }
in_vm "./test/e2e.sh" >"$RESULTS/nvmet.log" 2>&1
report kernel $? "$RESULTS/nvmet.log"
in_vm "BACKEND=spdk ./test/e2e.sh" >"$RESULTS/spdk.log" 2>&1
report spdk $? "$RESULTS/spdk.log"
in_vm "./test/systemd-e2e.sh" >"$RESULTS/systemd.log" 2>&1
report systemd $? "$RESULTS/systemd.log"

left=$("$LAB" ssh tgt 'echo "$(sudo podman ps -a --format "{{.Names}}" | wc -l) $(sudo podman volume ls -q | wc -l) $(lsmod | grep -c ^nvmet) $(sudo losetup -a | wc -l)"' </dev/null)
if [ "$left" != "0 0 0 0" ]; then
	echo "left behind (containers, volumes, modules, loop devices): $left"
	failed=1
fi

exit "$failed"
