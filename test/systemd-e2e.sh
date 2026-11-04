#!/usr/bin/env bash
# Runs the installed service under real systemd, inside a disposable privileged
# container. Nothing is installed on the machine running this test.
#
# The container shares the host's kernel, so as with test/e2e.sh this temporarily
# loads nvmet/nvmet-tcp and creates NVMe devices, then removes them. It does not
# bind-mount the host's /dev, and masks the systemd units that could change the host.
set -u

ENGINE=${ENGINE:-sudo podman}
IMAGE=${IMAGE:-nvweaver:systemd-test}
SPDK_IMAGE=${SPDK_IMAGE:-nvweaver:spdk}
TGT=nvweaver-sd-machine
VOL=nvweaver-sd-data
INI=nvweaver-sd-client
API=http://127.0.0.1:8420
PORT=14480
NQN=nqn.2026-11.net.xphyr.sd
SUB=$NQN:sub
HOST=$NQN:host
UNIT=nvweaver.service
LOOP=
pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
section() { printf '\n%s\n' "$1"; }
ini() { $ENGINE exec "$INI" sh -c "$1"; }
sd() { $ENGINE exec "$TGT" sh -c "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$2], got [$3]"; fi; }
expect_has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1" "[$2] not in [$3]" ;; esac; }
expect_lacks() { case "$3" in *"$2"*) bad "$1" "[$2] unexpectedly in [$3]" ;; *) ok "$1" ;; esac; }
prop() { sd "systemctl show -p $1 --value $UNIT"; }
state() { sd "systemctl is-active $UNIT" 2>/dev/null; }
wait_active() { # SECONDS
	local i
	for i in $(seq 1 "$1"); do
		[ "$(state)" = active ] && return 0
		sleep 1
	done
	return 1
}
post() { ini "curl -s -m 10 -o /tmp/b -w '%{http_code}' -X POST -d '$2' $API$1"; }
create() { # NAME PATH JSON
	local code
	code=$(post "$2" "$3")
	if [ "$code" = 201 ]; then ok "$1"; else bad "$1" "HTTP $code: $(ini 'cat /tmp/b')"; fi
}
ctrl() { ini "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$SUB' ] && basename \$c && exit 0; done"; }
live() { ini "c=\$(for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$SUB' ] && echo \$c; done | head -1); [ -n \"\$c\" ] && cat \$c/state"; }
cntlid() { ini "cat /sys/class/nvme/$(ctrl)/cntlid"; }
wait_live() {
	local i
	for i in $(seq 1 60); do
		[ "$(live)" = live ] && return 0
		sleep 0.5
	done
	return 1
}
dev_of() { ini "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = '$SUB' ] && basename \$s/nvme*n1 && exit 0; done"; }
# A fresh connection's namespace appears shortly after its controller goes live.
wait_dev() {
	local i d
	for i in $(seq 1 60); do
		d=$(dev_of)
		if [ -n "$d" ] && ini "[ -b /dev/$d ]"; then
			echo "$d"
			return 0
		fi
		sleep 0.25
	done
	return 1
}
checksum() { ini "dd if=/dev/$(wait_dev) bs=1M count=8 iflag=direct status=none | md5sum | cut -d' ' -f1"; }
kernel_has() { ini "mount -t configfs configfs /mnt 2>/dev/null; ls /mnt/nvmet/subsystems 2>/dev/null; umount /mnt 2>/dev/null; true"; }
boot() {
	$ENGINE run -d --name "$TGT" --privileged --network host --cgroupns=private \
		-v /lib/modules:/lib/modules:ro --tmpfs /run --tmpfs /run/lock \
		-v "$VOL:/var/lib/nvweaver" "$IMAGE" >/dev/null || return 1
	wait_boot
}
wait_boot() {
	local i s
	for i in $(seq 1 60); do
		s=$($ENGINE exec "$TGT" systemctl is-system-running 2>/dev/null)
		case "$s" in running | degraded) return 0 ;; esac
		sleep 1
	done
	echo "system state: $s"
	return 1
}

cleanup() {
	section "Cleaning up"
	ini "pkill -f 'nc -l -p $PORT'; nvme disconnect -n $SUB >/dev/null 2>&1"
	$ENGINE stop -t 30 "$TGT" >/dev/null 2>&1
	$ENGINE rm -f "$TGT" >/dev/null 2>&1
	[ -n "$LOOP" ] && ini "losetup -d $LOOP"
	if [ "${MODULES_WERE_LOADED:-1}" = 0 ]; then
		ini "modprobe -r nvmet-tcp nvmet" || echo "  note: nvmet modules left loaded"
	fi
	$ENGINE rm -f "$INI" >/dev/null 2>&1
	$ENGINE volume rm -f "$VOL" >/dev/null 2>&1
	printf '\n%d passed, %d failed\n' "$pass" "$fail"
}

[ -e /sys/module/nvmet_tcp ] && MODULES_WERE_LOADED=1 || MODULES_WERE_LOADED=0
ROOT=$(dirname "$0")/..
if [ -z "${SKIP_BUILD:-}" ]; then
	$ENGINE build -q -t "$IMAGE" -f "$ROOT/test/systemd/Containerfile" "$ROOT" >/dev/null || exit 1
fi
$ENGINE rm -f "$TGT" "$INI" >/dev/null 2>&1
$ENGINE volume rm -f "$VOL" >/dev/null 2>&1
$ENGINE run -d --name "$INI" --privileged --network host -v /lib/modules:/lib/modules:ro -v /dev:/dev \
	docker.io/library/alpine:3.22 sleep infinity >/dev/null || exit 1
trap cleanup EXIT
ini "apk add -q --no-progress nvme-cli curl jq util-linux kmod coreutils busybox-extras" || exit 1
ini "modprobe nvme-tcp" || exit 1
# Made before the machine boots: it gets its own copy of /dev, taken at startup.
ini "truncate -s 64M /tmp/loop.img"
LOOP=$(ini "losetup -f --show /tmp/loop.img")

# Every namespace of the subsystem that a host can actually use.
usable() { ini "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = '$SUB' ] || continue; for n in \$s/nvme*n*; do [ -b /dev/\$(basename \$n) ] && basename \$n; done; done"; }
wait_usable() { # COUNT
	local i
	for i in $(seq 1 40); do
		[ "$(usable | wc -l)" = "$1" ] && return 0
		sleep 0.25
	done
	return 1
}

section "Installation"
boot || { bad "machine boots"; $ENGINE logs "$TGT" 2>&1 | tail -20; exit 1; }
ok "machine boots under systemd"
expect_eq "make install does not enable anything" disabled "$(sd "systemctl is-enabled $UNIT")"
expect_eq "or start anything" inactive "$(state)"
expect_eq "environment file is private" 600 "$(sd 'stat -c %a /etc/nvweaver/nvweaver.env')"
expect_eq "unit points at the installed binary" /usr/local/bin/nvweaverd "$(sd "systemctl show -p ExecStart --value $UNIT | sed -E 's/.*path=([^ ;]+).*/\1/'")"
out=$(sd "systemd-analyze verify /etc/systemd/system/$UNIT" 2>&1)
expect_lacks "unit passes systemd-analyze verify" "$UNIT" "$out"
out=$(sd '/usr/lib/systemd/system-generators/podman-system-generator --dryrun /tmp/quadlet 2>&1 || /usr/libexec/podman/quadlet -dryrun 2>&1')
expect_has "Quadlet file generates a service" "nvweaver-spdk.service" "$out"
expect_lacks "without Quadlet errors" "unsupported key" "$out"

section "Start and readiness"
if sd "systemctl enable --now $UNIT" >/dev/null 2>&1; then ok "enable --now succeeds"; else
	bad "enable --now succeeds" "$(sd "journalctl -u $UNIT --no-pager -n 25")"
	exit 1
fi
# No retry: with Type=notify, start must not return until the API is usable.
expect_eq "API answers the moment start returns" 200 "$(ini "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")"
expect_eq "service type is notify" notify "$(prop Type)"
expect_has "status says what is running" "nvmet" "$(prop StatusText)"
expect_has "and it reports the kernel backend" '"nvmet"' "$(ini "curl -s $API/v1/system | jq .backend")"

section "Sandbox"
PID=$(prop MainPID)
expect_eq "no new privileges" 1 "$(sd "awk '/NoNewPrivs/{print \$2}' /proc/$PID/status")"
CAPS=$(sd "awk '/CapBnd/{print \$2}' /proc/$PID/status")
# CAP_SYS_ADMIN (21), CAP_SYS_MODULE (16), CAP_NET_BIND_SERVICE (10).
expect_eq "only the three capabilities it needs" 0000000000210400 "$CAPS"
sd "systemd-analyze security --no-pager $UNIT 2>/dev/null | tail -1" | sed 's/^/        /'

section "Configuration and data"
create "port" /v1/ports "{\"id\":1,\"address\":\"127.0.0.1\",\"service\":$PORT}"
create "host" /v1/hosts "{\"nqn\":\"$HOST\"}"
create "subsystem" /v1/subsystems "{\"nqn\":\"$SUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
create "file namespace, inside the sandbox" "/v1/subsystems/$SUB/namespaces" '{"nsid":1,"file":{"name":"a.img","size_bytes":67108864}}'
create "block device namespace, inside the sandbox" "/v1/subsystems/$SUB/namespaces" "{\"nsid\":2,\"device_path\":\"$LOOP\"}"
ini "nvme connect -t tcp -a 127.0.0.1 -s $PORT -n $SUB --hostnqn=$HOST -l 30 >/dev/null" && ok "host connects" || bad "host connects"
wait_live || bad "controller goes live"
wait_usable 2 && ok "both namespaces are usable by the host" || bad "both namespaces are usable by the host" "$(usable | tr '\n' ' ')"
ini "dd if=/dev/urandom of=/tmp/p bs=1M count=8 status=none"
for d in $(usable); do ini "dd if=/tmp/p of=/dev/$d bs=1M oflag=direct status=none"; done
SUM=$(ini "md5sum < /tmp/p | cut -d' ' -f1")
expect_eq "data round trip" "$SUM" "$(checksum)"
expect_eq "data reached the file" "$SUM" "$(sd "dd if=/var/lib/nvweaver/volumes/a.img bs=1M count=8 status=none | md5sum | cut -d' ' -f1")"
expect_eq "data reached the block device" "$SUM" "$(ini "dd if=$LOOP bs=1M count=8 iflag=direct status=none | md5sum | cut -d' ' -f1")"

section "systemctl stop and start"
sd "systemctl stop $UNIT"
expect_eq "stop leaves the unit inactive, not failed" inactive "$(state)"
expect_lacks "stop removes the target from the kernel" "$SUB" "$(kernel_has)"
sd "systemctl start $UNIT"
expect_has "start restores the target" "$SUB" "$(kernel_has)"
wait_live && ok "host reconnects by itself" || bad "host reconnects by itself"
expect_eq "data intact" "$SUM" "$(checksum)"

section "Crash: systemd restarts it, and hosts do not notice"
BEFORE=$(cntlid)
OLD=$(prop MainPID)
sd "systemctl kill -s SIGKILL $UNIT"
sleep 7
wait_active 20 && ok "back up after being killed" || bad "back up after being killed" "$(sd "journalctl -u $UNIT --no-pager -n 15")"
[ "$(prop MainPID)" != "$OLD" ] && ok "as a new process" || bad "as a new process"
expect_eq "systemd counted the restart" 1 "$(prop NRestarts)"
expect_eq "same controller: the host never reconnected" "$BEFORE" "$(cntlid)"
expect_eq "data intact" "$SUM" "$(checksum)"

section "Reboot: an enabled service comes back with its configuration"
ini "nvme disconnect -n $SUB >/dev/null"
$ENGINE restart -t 30 "$TGT" >/dev/null || bad "machine restarts"
wait_boot && ok "machine reboots" || bad "machine reboots"
wait_active 30 && ok "service started at boot" || bad "service started at boot" "$(sd "journalctl -u $UNIT --no-pager -n 15")"
expect_has "configuration restored" "$SUB" "$(kernel_has)"
ini "nvme connect -t tcp -a 127.0.0.1 -s $PORT -n $SUB --hostnqn=$HOST -l 30 >/dev/null" && ok "host connects after reboot" || bad "host connects after reboot"
wait_live
wait_usable 2 && ok "both namespaces usable after reboot" || bad "both namespaces usable after reboot"
expect_eq "data intact" "$SUM" "$(checksum)"
ini "nvme disconnect -n $SUB >/dev/null"

section "Late resources: it keeps trying until the address is free"
sd "systemctl stop $UNIT"
ini "(nc -l -p $PORT >/dev/null 2>&1 &); sleep 0.5"
sd "systemctl start --no-block $UNIT"
sleep 8
[ "$(state)" != active ] && ok "not reported active while it can't serve" || bad "not reported active while it can't serve"
RESTARTS=$(prop NRestarts)
[ "$RESTARTS" -ge 1 ] && ok "systemd is retrying ($RESTARTS so far)" || bad "systemd is retrying" "NRestarts=$RESTARTS"
expect_has "the journal says why" "already in use" "$(sd "journalctl -u $UNIT --no-pager -n 30")"
ini "pkill -f 'nc -l -p $PORT'"
wait_active 30 && ok "starts by itself once the address is free" || bad "starts by itself once the address is free"
expect_eq "API is usable" 200 "$(ini "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")"

section "Settings from the environment file"
sd "echo NVWEAVER_KEEP_ON_EXIT=true >> /etc/nvweaver/nvweaver.env; systemctl restart $UNIT"
ini "nvme connect -t tcp -a 127.0.0.1 -s $PORT -n $SUB --hostnqn=$HOST -l 30 >/dev/null" && ok "host connects" || bad "host connects"
wait_live
wait_usable 2 || bad "both namespaces usable" "$(usable | tr '\n' ' ')"
BEFORE=$(cntlid)
sd "systemctl stop $UNIT"
expect_has "KEEP_ON_EXIT: stop leaves the target serving" "$SUB" "$(kernel_has)"
expect_eq "with the host still connected" live "$(live)"
expect_eq "and still doing I/O" "$SUM" "$(checksum)"
sd "systemctl start $UNIT"
expect_eq "start adopts it: same controller" "$BEFORE" "$(cntlid)"
ini "nvme disconnect -n $SUB >/dev/null"
sd "sed -i '/KEEP_ON_EXIT/d' /etc/nvweaver/nvweaver.env; systemctl restart $UNIT"
sd "systemctl stop $UNIT"
expect_lacks "without it, stop removes the target again" "$SUB" "$(kernel_has)"

section "Uninstall"
sd "systemctl disable $UNIT >/dev/null 2>&1; rm -f /etc/systemd/system/$UNIT; systemctl daemon-reload"
expect_eq "unit is gone" not-found "$(prop LoadState)"
expect_eq "configuration is kept" yes "$(sd '[ -s /var/lib/nvweaver/state.json ] && echo yes || echo no')"

section "Quadlet: the SPDK image as a service, with no capabilities"
if $ENGINE image inspect "$SPDK_IMAGE" >/dev/null 2>&1; then
	QUNIT=nvweaver-spdk.service
	$ENGINE save "$SPDK_IMAGE" | $ENGINE exec -i "$TGT" podman load -q >/dev/null && ok "image loaded into Podman" || bad "image loaded into Podman"
	# Only an image saved by Docker lacks the name that a Podman build gives it.
	sd "podman image exists localhost/$SPDK_IMAGE || podman tag docker.io/library/$SPDK_IMAGE localhost/$SPDK_IMAGE"
	sd "systemctl daemon-reload"
	# Quadlet units start at boot, so it has been retrying without an image until now.
	sd "systemctl reset-failed $QUNIT" 2>/dev/null
	if sd "systemctl start $QUNIT" 2>/dev/null; then ok "Quadlet service starts"; else
		bad "Quadlet service starts" "$(sd "journalctl -u $QUNIT --no-pager -n 25")"
	fi
	# No retry here either: Notify=true must carry readiness out of the container.
	expect_eq "API answers the moment start returns" 200 "$(ini "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")"
	expect_has "it runs the SPDK backend" '"spdk"' "$(ini "curl -s $API/v1/system | jq .backend")"
	expect_eq "with no capabilities" 0000000000000000 "$(sd "podman exec systemd-nvweaver-spdk awk '/CapEff/{print \$2}' /proc/1/status")"
	create "port" /v1/ports "{\"id\":1,\"address\":\"127.0.0.1\",\"service\":$PORT}"
	create "host" /v1/hosts "{\"nqn\":\"$HOST\"}"
	create "subsystem" /v1/subsystems "{\"nqn\":\"$SUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	create "namespace" "/v1/subsystems/$SUB/namespaces" '{"nsid":1,"file":{"name":"q.img","size_bytes":67108864}}'
	ini "nvme connect -t tcp -a 127.0.0.1 -s $PORT -n $SUB --hostnqn=$HOST -l 30 >/dev/null" && ok "host connects" || bad "host connects"
	wait_live || bad "controller goes live"
	ini "dd if=/tmp/p of=/dev/$(wait_dev) bs=1M oflag=direct status=none"
	expect_eq "data round trip" "$SUM" "$(checksum)"
	ini "nvme disconnect -n $SUB >/dev/null"
	sd "systemctl restart $QUNIT"
	expect_has "restart restores the configuration" "$SUB" "$(ini "curl -s $API/v1/subsystems | jq -r '.[].nqn'")"
	sd "systemctl stop $QUNIT"
	expect_eq "stop leaves it inactive, not failed" inactive "$(sd "systemctl is-active $QUNIT" 2>/dev/null)"
else
	echo "  skip  $SPDK_IMAGE has not been built"
fi

[ "$fail" = 0 ]
