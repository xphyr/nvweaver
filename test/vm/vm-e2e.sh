#!/usr/bin/env bash
# Tests that need more than one kernel, or a real boot, using the VMs from lab.sh:
#
# - A separate initiator, over the private link
# - Emulated NVMe, SATA and virtio disks, on both backends
# - Passthrough of an emulated PCIe NVMe controller, with real I/O
# - A real install under systemd, including rebooting the VM, with SELinux enforcing
#
# Run it on the machine that runs the VMs. It starts fresh VMs and deletes them at the end,
# unless KEEP_VMS is set. Local Podman images are copied in, so build these first:
#   podman build -t nvweaver:dev -f Containerfile .
#   podman build -t nvweaver:spdk -f Containerfile.spdk .
set -u

LAB=$(dirname "$0")/lab.sh
API=http://127.0.0.1:8420
TARGET_IP=10.99.0.1
PORT=4420
NQN=nqn.2026-11.net.xphyr.vm
NVME=/dev/disk/by-id/nvme-QEMU_NVMe_Ctrl_NVWEAVER0001
SATA=/dev/disk/by-id/ata-QEMU_HARDDISK_NVWEAVERSATA
VIRTIO=/dev/disk/by-id/virtio-NVWEAVERVIRTIO
pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
section() { printf '\n%s\n' "$1"; }
t() { "$LAB" ssh tgt "$1"; }
i() { "$LAB" ssh ini "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$2], got [$3]"; fi; }
expect_has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1" "[$2] not in [$3]" ;; esac; }
expect_lacks() { case "$3" in *"$2"*) bad "$1" "[$2] unexpectedly in [$3]" ;; *) ok "$1" ;; esac; }

api() { # NAME WANT METHOD PATH [JSON]
	local data=
	[ $# -ge 5 ] && data="-d '$5'"
	local code
	code=$(t "curl -s -m 20 -o /tmp/b -w '%{http_code}' -X $3 $data $API$4")
	if [ "$code" = "$2" ]; then ok "$1"; else bad "$1" "HTTP $code, want $2: $(t 'cat /tmp/b' | tr -s ' \n' ' ' | cut -c1-200)"; fi
}

wait_api() {
	for _ in $(seq 1 120); do
		[ "$(t "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz" 2>/dev/null)" = 200 ] && return 0
		sleep 0.5
	done
	return 1
}

# The namespace's block device on the initiator, once it can be opened.
ini_dev() { # SUBNQN
	local d
	for _ in $(seq 1 60); do
		d=$(i "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = '$1' ] || continue; for n in \$s/nvme*n*; do [ -b /dev/\$(basename \$n) ] && echo /dev/\$(basename \$n) && exit 0; done; done")
		if [ -n "$d" ]; then
			echo "$d"
			return 0
		fi
		sleep 0.25
	done
	return 1
}

connect() { # SUBNQN [extra nvme connect args]
	i "sudo nvme connect -t tcp -a $TARGET_IP -s $PORT -n $1 ${2:-} >/dev/null" 2>&1
}
disconnect() { i "sudo nvme disconnect -n $1 >/dev/null 2>&1"; }
disconnect_all() { i "sudo nvme disconnect-all >/dev/null 2>&1"; }
sum_of() { # RUNNER DEVICE
	"$1" "sudo dd if=$2 bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1"
}
wait_active() { # UNIT SECONDS
	for _ in $(seq 1 "$2"); do
		[ "$(t "systemctl is-active $1" 2>/dev/null)" = active ] && return 0
		sleep 1
	done
	return 1
}
reboot_target() {
	t 'sudo systemctl reboot' 2>/dev/null
	sleep 5
	for _ in $(seq 1 90); do
		t true 2>/dev/null && return 0
		sleep 2
	done
	return 1
}
selinux_denials() { # Printed one per line, from this boot.
	t "sudo journalctl -b --no-pager 2>/dev/null | grep -E 'avc: +denied' | grep -E 'nvweaver|nvmf_tgt|modprobe' | cut -c1-200"
}

cleanup() {
	section "Cleaning up"
	disconnect_all 2>/dev/null
	if [ -z "${KEEP_VMS:-}" ]; then
		"$LAB" down
	fi
	printf '\n%d passed, %d failed\n' "$pass" "$fail"
}

"$LAB" down >/dev/null 2>&1
"$LAB" up >/dev/null || exit 1
trap cleanup EXIT
"$LAB" load tgt localhost/nvweaver:dev >/dev/null || exit 1
"$LAB" load tgt localhost/nvweaver:spdk >/dev/null || exit 1
"$LAB" sync tgt || exit 1
# The initiator has no NVMe devices of its own, so nothing has loaded these yet.
i 'sudo modprobe nvme-tcp' || exit 1
expect_eq "initiator has the NVMe fabrics device" yes "$(i '[ -c /dev/nvme-fabrics ] && echo yes || echo no')"
HOST=$(i 'cat /etc/nvme/hostnqn')
expect_has "VMs have different host identities" nqn. "$HOST"
[ "$HOST" != "$(t 'cat /etc/nvme/hostnqn')" ] && ok "and they differ" || bad "and they differ"
expect_eq "SELinux is enforcing on the target" Enforcing "$(t getenforce)"
expect_eq "initiator can reach the target only over the link" "$TARGET_IP" "$(i "ip -o route get $TARGET_IP | sed -E 's/.* dev lab0 .*/$TARGET_IP/'")"

backend_tests() { # BACKEND IMAGE [run args]
	local backend=$1 image=$2 name disk sub dev sum
	shift 2
	section "$backend: every disk type, from a separate initiator"
	t "sudo podman run -d --pull=never --name nvweaver-vm --privileged --network host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-vm-data:/var/lib/nvweaver \
		-e NVWEAVER_BACKEND=$backend $image >/dev/null"
	wait_api && ok "service starts" || bad "service starts" "$(t 'sudo podman logs nvweaver-vm 2>&1 | tail -5')"
	api "port on the link address" 201 POST /v1/ports "{\"id\":1,\"address\":\"$TARGET_IP\",\"service\":$PORT}"
	api "host" 201 POST /v1/hosts "{\"nqn\":\"$HOST\"}"
	for entry in "nvme:$NVME" "sata:$SATA" "virtio:$VIRTIO"; do
		name=${entry%%:*}
		disk=${entry#*:}
		sub=$NQN:$backend-$name
		api "$name: subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
		api "$name: namespace on its by-id path" 201 POST "/v1/subsystems/$sub/namespaces" "{\"nsid\":1,\"device_path\":\"$disk\"}"
	done
	sub=$NQN:$backend-file
	api "file: subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "file: managed volume" 201 POST "/v1/subsystems/$sub/namespaces" '{"nsid":1,"file":{"name":"f.img","size_bytes":67108864}}'

	local out
	out=$(i "sudo nvme discover -t tcp -a $TARGET_IP -s $PORT" 2>&1)
	for name in nvme sata virtio file; do
		expect_has "discovery lists $name" "$NQN:$backend-$name" "$out"
	done
	expect_eq "discovery reports the link address" "$TARGET_IP" "$(echo "$out" | awk '/traddr:/{print $2; exit}')"
	# Both of these must fail for the right reason: a setup error also "refuses" a host.
	out=$(i "sudo nvme discover -t tcp -a $TARGET_IP -s $PORT --hostnqn=$NQN:stranger" 2>&1)
	expect_has "a host not on the list gets a discovery log" "Discovery Log" "$out"
	expect_lacks "listing none of the subsystems" "$NQN:" "$out"
	if out=$(connect "$NQN:$backend-nvme" "--hostnqn=$NQN:stranger"); then
		bad "a host not on the list cannot connect" "connected: $out"
		disconnect "$NQN:$backend-nvme"
	else
		expect_lacks "a host not on the list cannot connect" "scan topology" "$out"
	fi

	i "head -c 16M /dev/urandom > /tmp/p"
	sum=$(i "md5sum < /tmp/p | cut -d' ' -f1")
	for entry in "nvme:$NVME" "sata:$SATA" "virtio:$VIRTIO" "file:"; do
		name=${entry%%:*}
		disk=${entry#*:}
		sub=$NQN:$backend-$name
		out=$(connect "$sub") && ok "$name: connects" || bad "$name: connects" "$out"
		dev=$(ini_dev "$sub")
		if [ -z "$dev" ]; then
			bad "$name: namespace appears on the initiator"
			continue
		fi
		i "sudo dd if=/tmp/p of=$dev bs=1M oflag=direct status=none"
		expect_eq "$name: reads back through NVMe/TCP" "$sum" "$(sum_of i "$dev")"
		if [ -n "$disk" ]; then
			expect_eq "$name: data is on the target's disk" "$sum" "$(sum_of t "$disk")"
			expect_eq "$name: host sees the disk's sector size" \
				"$(t "sudo blockdev --getss $disk")" "$(i "sudo blockdev --getss $dev")"
		else
			expect_eq "$name: data is in the target's file" "$sum" \
				"$(t "sudo podman exec nvweaver-vm sh -c 'dd if=/var/lib/nvweaver/volumes/f.img bs=1M count=16 2>/dev/null | md5sum | cut -d\" \" -f1'")"
		fi
		disconnect "$sub"
	done
	api "a disk can't be exported twice" 409 POST "/v1/subsystems/$NQN:$backend-file/namespaces" "{\"nsid\":2,\"device_path\":\"$NVME\"}"
}

backend_cleanup() {
	t 'sudo podman stop -t 25 nvweaver-vm >/dev/null; sudo podman rm -f nvweaver-vm >/dev/null; sudo podman volume rm -f nvweaver-vm-data >/dev/null'
}

backend_tests nvmet localhost/nvweaver:dev

section "nvmet: passthrough of an emulated PCIe NVMe controller"
PCTRL=$(t "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/serial | tr -d ' ')\" = NVWEAVER0002 ] && echo /dev/\$(basename \$c); done")
PBLOCK=/dev/disk/by-id/nvme-QEMU_NVMe_Ctrl_NVWEAVER0002
expect_has "found the controller" /dev/nvme "$PCTRL"
PSUB=$NQN:passthru
api "passthrough subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$PSUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1],\"passthru_device\":\"$PCTRL\"}"
out=$(connect "$PSUB") && ok "connects" || bad "connects" "$out"
PDEV=$(ini_dev "$PSUB")
if [ -n "$PDEV" ]; then
	PINI=$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn)\" = '$PSUB' ] && echo /dev/\$(basename \$c); done")
	expect_eq "host sees the real controller's serial" NVWEAVER0002 "$(i "sudo nvme id-ctrl $PINI -o json | jq -r .sn | tr -d ' '")"
	expect_eq "and its model" "QEMU NVMe Ctrl" "$(i "sudo nvme id-ctrl $PINI -o json | jq -r .mn | sed 's/ *\$//'")"
	SUM=$(i "md5sum < /tmp/p | cut -d' ' -f1")
	i "sudo dd if=/tmp/p of=$PDEV bs=1M oflag=direct status=none"
	expect_eq "reads back through passthrough" "$SUM" "$(sum_of i "$PDEV")"
	expect_eq "data is on the real namespace" "$SUM" "$(sum_of t "$PBLOCK")"
else
	bad "namespace appears on the initiator" "$(t 'sudo dmesg | grep -iE "passthru|nvmet" | tail -5')"
fi
disconnect "$PSUB"
api "remove passthrough subsystem" 204 DELETE "/v1/subsystems/$PSUB"
backend_cleanup

backend_tests spdk localhost/nvweaver:spdk
backend_cleanup
disconnect_all

section "systemd: real install on the VM"
t 'sudo dnf -y -q install golang >/dev/null 2>&1'
t 'cd nvweaver && make >/dev/null && sudo make install >/dev/null' && ok "make install as root" || bad "make install as root"
t 'sudo systemctl daemon-reload'
expect_eq "installing enables nothing" disabled "$(t 'systemctl is-enabled nvweaver' 2>&1)"
t 'sudo systemctl enable --now nvweaver' 2>/dev/null && ok "enable --now" || bad "enable --now" "$(t 'sudo journalctl -u nvweaver --no-pager -n 15')"
expect_eq "API answers the moment start returns" 200 "$(t "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")"
expect_has "binary has the expected SELinux type" bin_t "$(t 'ls -Z /usr/local/bin/nvweaverd')"
t 'ps -eZ | grep nvweaverd' | sed 's/^/        /' | cut -c1-90
SSUB=$NQN:service
api "port" 201 POST /v1/ports "{\"id\":1,\"address\":\"$TARGET_IP\",\"service\":$PORT}"
api "host" 201 POST /v1/hosts "{\"nqn\":\"$HOST\"}"
api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$SSUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
api "namespace on a SATA disk" 201 POST "/v1/subsystems/$SSUB/namespaces" "{\"nsid\":1,\"device_path\":\"$SATA\"}"
api "namespace on a file" 201 POST "/v1/subsystems/$SSUB/namespaces" '{"nsid":2,"file":{"name":"s.img","size_bytes":67108864}}'
out=$(connect "$SSUB" "-l 300 -c 5") && ok "host connects" || bad "host connects" "$out"
SDEV=$(ini_dev "$SSUB")
SUM=$(i "md5sum < /tmp/p | cut -d' ' -f1")
i "sudo dd if=/tmp/p of=$SDEV bs=1M oflag=direct status=none"
expect_eq "data round trip" "$SUM" "$(sum_of i "$SDEV")"

section "systemd: real reboot of the target"
BOOT1=$(t 'cat /proc/sys/kernel/random/boot_id')
reboot_target && ok "VM reboots" || bad "VM reboots"
[ "$BOOT1" != "$(t 'cat /proc/sys/kernel/random/boot_id')" ] && ok "it really was a new boot" || bad "it really was a new boot"
wait_active nvweaver 60 && ok "service started at boot" || bad "service started at boot" "$(t 'sudo journalctl -b -u nvweaver --no-pager -n 15')"
expect_eq "first time, without any retries" 0 "$(t 'systemctl show -p NRestarts --value nvweaver')"
expect_has "it started after the network" network-online.target "$(t 'systemctl show -p After --value nvweaver')"
t 'systemd-analyze critical-chain nvweaver.service 2>/dev/null | tail -n +3 | head -6' | sed 's/^/        /' | cut -c1-80
expect_has "nvmet-tcp was loaded from inside the sandbox" nvmet_tcp "$(t 'lsmod')"
expect_has "port is listening on the link address" "$TARGET_IP:$PORT" "$(t 'ss -ltn')"
LIVE=
for _ in $(seq 1 60); do
	LIVE=$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$SSUB' ] && cat \$c/state; done")
	[ "$LIVE" = live ] && break
	sleep 2
done
expect_eq "host reconnected by itself" live "$LIVE"
SDEV=$(ini_dev "$SSUB")
expect_eq "data survived the reboot" "$SUM" "$(sum_of i "$SDEV")"
expect_eq "no SELinux denials" "" "$(selinux_denials)"
disconnect_all

section "systemd: stop and uninstall"
t 'sudo systemctl disable --now nvweaver' 2>/dev/null
expect_eq "stop leaves it inactive, not failed" inactive "$(t 'systemctl is-active nvweaver' 2>/dev/null)"
expect_lacks "stop closes the port" "$TARGET_IP:$PORT" "$(t 'ss -ltn')"
t 'cd nvweaver && sudo make uninstall >/dev/null; sudo systemctl daemon-reload'
expect_eq "unit is gone" not-found "$(t 'systemctl show -p LoadState --value nvweaver')"
expect_eq "saved configuration is kept" yes "$(t 'sudo test -s /var/lib/nvweaver/state.json && echo yes || echo no')"

section "Quadlet: SPDK on the VM's own Podman, with no capabilities"
QSUB=$NQN:quadlet
t "cd nvweaver && sudo make install-quadlet >/dev/null" && ok "make install-quadlet" || bad "make install-quadlet"
QDISK=/dev/nvweaver0
t "sudo sed -i 's|^#AddDevice=.*|AddDevice=$VIRTIO:$QDISK|' /etc/containers/systemd/nvweaver-spdk.container"
t 'sudo systemctl daemon-reload'
t 'sudo systemctl start nvweaver-spdk' 2>/dev/null && ok "service starts" || bad "service starts" "$(t 'sudo journalctl -u nvweaver-spdk --no-pager -n 15')"
expect_eq "API answers the moment start returns" 200 "$(t "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")"
expect_has "it runs SPDK" '"spdk"' "$(t "curl -s $API/v1/system | jq .backend")"
expect_eq "with no capabilities" 0000000000000000 "$(t "sudo podman exec systemd-nvweaver-spdk awk '/CapEff/{print \$2}' /proc/1/status")"
api "port" 201 POST /v1/ports "{\"id\":1,\"address\":\"$TARGET_IP\",\"service\":$PORT}"
api "host" 201 POST /v1/hosts "{\"nqn\":\"$HOST\"}"
api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$QSUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
api "namespace on the one disk it was given" 201 POST "/v1/subsystems/$QSUB/namespaces" "{\"nsid\":1,\"device_path\":\"$QDISK\"}"
api "a disk it wasn't given is out of reach" 422 POST "/v1/subsystems/$QSUB/namespaces" "{\"nsid\":2,\"device_path\":\"$NVME\"}"
out=$(connect "$QSUB" "-l 300 -c 5") && ok "host connects" || bad "host connects" "$out"
QDEV=$(ini_dev "$QSUB")
SUM=$(i "md5sum < /tmp/p | cut -d' ' -f1")
i "sudo dd if=/tmp/p of=$QDEV bs=1M oflag=direct status=none"
expect_eq "data round trip" "$SUM" "$(sum_of i "$QDEV")"
expect_eq "data is on the target's disk" "$SUM" "$(sum_of t "$VIRTIO")"

section "Quadlet: real reboot of the target"
reboot_target && ok "VM reboots" || bad "VM reboots"
wait_active nvweaver-spdk 90 && ok "service started at boot" || bad "service started at boot" "$(t 'sudo journalctl -b -u nvweaver-spdk --no-pager -n 15')"
expect_eq "first time, without any retries" 0 "$(t 'systemctl show -p NRestarts --value nvweaver-spdk')"
LIVE=
for _ in $(seq 1 60); do
	LIVE=$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$QSUB' ] && cat \$c/state; done")
	[ "$LIVE" = live ] && break
	sleep 2
done
expect_eq "host reconnected by itself" live "$LIVE"
QDEV=$(ini_dev "$QSUB")
expect_eq "data survived the reboot" "$SUM" "$(sum_of i "$QDEV")"
expect_eq "no SELinux denials" "" "$(selinux_denials)"
disconnect_all
t 'sudo systemctl stop nvweaver-spdk' 2>/dev/null
expect_eq "stop leaves it inactive, not failed" inactive "$(t 'systemctl is-active nvweaver-spdk' 2>/dev/null)"

[ "$fail" = 0 ]
