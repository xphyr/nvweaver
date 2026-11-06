#!/usr/bin/env bash
# End-to-end test: the container image against the real kernel target, with the
# real Linux initiator connecting over loopback.
#
# Needs rootful Podman, because the kernel target and the initiator both need real
# root. ENGINE sets the command, by default "sudo podman". Docker is a fallback, with
# ENGINE=docker. It temporarily loads
# nvmet/nvmet-tcp on the host and creates NVMe devices there, using only
# 127.0.0.1, throwaway files and one loop device. It uses no other disks, except
# that it checks a mounted disk is refused (see "in-use").
#
# BACKEND=nvmet (default) or BACKEND=spdk picks the target implementation. Checks
# that differ between them are marked below; everything else must pass on both.
set -u
# TEST_TIMEOUT_SCALE lengthens the waits, for slow machines such as emulated VMs.
SCALE=${TEST_TIMEOUT_SCALE:-1}

ENGINE=${ENGINE:-sudo podman}
BACKEND=${BACKEND:-nvmet}
case "$BACKEND" in
nvmet) IMAGE=${IMAGE:-nvweaver:dev}; CONTAINERFILE=Containerfile ;;
spdk) IMAGE=${IMAGE:-nvweaver:spdk}; CONTAINERFILE=Containerfile.spdk ;;
*) echo "BACKEND must be nvmet or spdk"; exit 2 ;;
esac
TGT=nvweaver-e2e-target
INI=nvweaver-e2e-initiator
VOL=nvweaver-e2e-data
API=http://127.0.0.1:18420
P1=14420
P2=14421
NQN=nqn.2026-11.net.xphyr.e2e
H1=$NQN:host1
H2=$NQN:host2
H3=$NQN:host3
HID1=11111111-1111-4111-8111-111111111111
HID2=22222222-2222-4222-8222-222222222222
HID3=33333333-3333-4333-8333-333333333333
SA=$NQN:file
SB=$NQN:block
SC=$NQN:shared
SD=$NQN:auth
SE=$NQN:passthru
SF=$NQN:nvmeblock
LOOP=
pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
section() { printf '\n%s\n' "$1"; }
ini() { $ENGINE exec "$INI" sh -c "$1"; }

# expect_eq NAME WANT GOT
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$2], got [$3]"; fi; }
# expect_has NAME NEEDLE HAYSTACK
expect_has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1" "[$2] not in [$3]" ;; esac; }
expect_lacks() { case "$3" in *"$2"*) bad "$1" "[$2] unexpectedly in [$3]" ;; *) ok "$1" ;; esac; }

# call METHOD PATH [JSON]: prints the status code then the body.
call() {
	if [ $# -ge 3 ]; then
		ini "curl -s -o /tmp/body -w '%{http_code}' -X $1 -H 'Content-Type: application/json' -d '$3' '$API$2'; echo; cat /tmp/body"
	else
		ini "curl -s -o /tmp/body -w '%{http_code}' -X $1 '$API$2'; echo; cat /tmp/body"
	fi
}
# api NAME WANT_STATUS METHOD PATH [JSON]
api() {
	local name=$1 want=$2
	shift 2
	local out code
	out=$(call "$@")
	code=${out%%$'\n'*}
	BODY=${out#*$'\n'}
	if [ "$code" = "$want" ]; then ok "$name"; else bad "$name" "HTTP $code, want $want: $BODY"; fi
}

connect() { # HOSTNQN HOSTID SUBNQN PORT [extra]
	ini "nvme connect -t tcp -a 127.0.0.1 -s $4 -n $3 --hostnqn=$1 --hostid=$2 -l $((5 * SCALE)) ${5:-}" 2>&1
}
disconnect() { ini "nvme disconnect -n $1 >/dev/null 2>&1"; }
discover() { ini "nvme discover -t tcp -a 127.0.0.1 -s $2 --hostnqn=$1 --hostid=$3" 2>&1; }

# Namespace block device of a subsystem, by the NQN the kernel reports.
blockdev_of() {
	ini "for s in /sys/class/nvme-subsystem/*; do
		[ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = '$1' ] || continue
		for n in \$s/nvme*n*; do [ -e \"\$n\" ] && echo /dev/\$(basename \$n) && exit 0; done
	done"
}
ctrl_of() {
	ini "for c in /sys/class/nvme/*; do
		[ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$1' ] && echo /dev/\$(basename \$c) && exit 0
	done"
}
live_paths() {
	ini "n=0; for c in /sys/class/nvme/*; do
		[ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$1' ] && [ \"\$(cat \$c/state)\" = live ] && n=\$((n+1))
	done; echo \$n"
}
wait_dev() {
	local d
	for _ in $(seq 1 $((20 * SCALE))); do
		d=$(blockdev_of "$1")
		if [ -n "$d" ] && ini "[ -b $d ]"; then
			echo "$d"
			return 0
		fi
		sleep 0.5
	done
	return 1
}
start_target() { # [keep-on-exit]
	$ENGINE rm -f "$TGT" >/dev/null 2>&1
	$ENGINE run -d --name "$TGT" --privileged --network host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v "$VOL:/var/lib/nvweaver" \
		-e NVWEAVER_LISTEN=127.0.0.1:18420 -e NVWEAVER_KEEP_ON_EXIT="${1:-false}" \
		-e NVWEAVER_BACKEND="$BACKEND" \
		"$IMAGE" >/dev/null || return 1
	for _ in $(seq 1 $((40 * SCALE))); do
		[ "$(ini "curl -s -o /dev/null -w '%{http_code}' $API/healthz")" = 200 ] && return 0
		sleep 0.25
	done
	$ENGINE logs "$TGT" >&2
	return 1
}

cleanup() {
	section "Cleaning up"
	ini "touch /tmp/io.stop"
	for s in "$NQN:node1" "$NQN:node2" "$NQN:overlay" "$SE" "$SF" "$SA" "$SB" "$SC" "$SD"; do disconnect "$s"; done
	$ENGINE stop -t 20 "$TGT" >/dev/null 2>&1
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
if [ -z "${SKIP_BUILD:-}" ]; then
	$ENGINE build -q -t "$IMAGE" -f "$(dirname "$0")/../$CONTAINERFILE" "$(dirname "$0")/.." >/dev/null || exit 1
fi
$ENGINE rm -f "$TGT" "$INI" >/dev/null 2>&1
$ENGINE volume rm -f "$VOL" >/dev/null 2>&1
$ENGINE run -d --name "$INI" --privileged --network host -v /lib/modules:/lib/modules:ro -v /dev:/dev \
	docker.io/library/alpine:3.22 sleep infinity >/dev/null || exit 1
trap cleanup EXIT
ini "apk add -q --no-progress nvme-cli curl jq util-linux kmod coreutils busybox-extras iproute2-ss" || exit 1
ini "modprobe nvme-tcp" || exit 1

section "Startup"
start_target false || { bad "target starts"; exit 1; }
ok "target starts and loads nvmet-tcp"
api "system reports nvmet ready" 200 GET /v1/system
expect_has "target_ready is true" '"target_ready": true' "$BODY"
expect_has "reports a version" '"version": "' "$BODY"
expect_has "running the requested backend" "\"backend\": \"$BACKEND\"" "$BODY"

section "License notices"
case "$BACKEND" in
nvmet) NOTICES="LICENSE THIRD_PARTY_NOTICES.md go/LICENSE go/PATENTS" ;;
spdk) NOTICES="LICENSE THIRD_PARTY_NOTICES.md go/LICENSE go/PATENTS spdk/LICENSE spdk/bsd-3-clause.txt dpdk/bsd-3-clause.txt dpdk/README isa-l/LICENSE" ;;
esac
for f in $NOTICES; do
	$ENGINE exec "$TGT" test -s "/usr/share/licenses/nvweaver/$f" && ok "$f is in the image" || bad "$f is in the image"
done

section "CRUD"
api "create port 1" 201 POST /v1/ports "{\"id\":1,\"address\":\"127.0.0.1\",\"service\":$P1}"
api "create port 2" 201 POST /v1/ports "{\"id\":2,\"address\":\"127.0.0.1\",\"service\":$P2}"
expect_eq "a port's transport defaults to tcp" tcp "$(ini "jq -r .transport /tmp/body")"
api "a transport this service can't use is refused" 422 POST /v1/ports '{"id":8,"address":"127.0.0.1","service":14438,"transport":"rdma"}'
api "system info lists the transports" 200 GET /v1/system
expect_eq "just tcp, for now" '["tcp"]' "$(ini "jq -c .capabilities.transports /tmp/body")"
api "duplicate listener refused" 409 POST /v1/ports "{\"address\":\"127.0.0.1\",\"service\":$P1}"
api "create host1" 201 POST /v1/hosts "{\"nqn\":\"$H1\"}"
api "create host2" 201 POST /v1/hosts "{\"nqn\":\"$H2\"}"
api "subsystem backed by a file" 201 POST /v1/subsystems "{\"nqn\":\"$SA\",\"allowed_hosts\":[\"$H1\"],\"ports\":[1]}"
api "managed volume namespace" 201 POST "/v1/subsystems/$SA/namespaces" '{"nsid":1,"file":{"name":"a.img","size_bytes":67108864}}'
api "list subsystems" 200 GET /v1/subsystems
expect_has "list includes the subsystem" "$SA" "$BODY"

section "Non-NVMe block device (loop)"
ini "truncate -s 64M /tmp/loop.img"
LOOP=$(ini "losetup -f --show /tmp/loop.img")
expect_has "loop device created" /dev/loop "$LOOP"
api "subsystem backed by a block device" 201 POST /v1/subsystems "{\"nqn\":\"$SB\",\"allowed_hosts\":[\"$H1\",\"$H2\"],\"ports\":[1,2]}"
api "block device namespace" 201 POST "/v1/subsystems/$SB/namespaces" "{\"nsid\":1,\"device_path\":\"$LOOP\"}"
api "same device twice refused" 409 POST "/v1/subsystems/$SA/namespaces" "{\"nsid\":2,\"device_path\":\"$LOOP\"}"

section "Safety guards"
# -v drops any "[/subvolume]" suffix, which isn't part of the device path.
ROOT_DISK=$(findmnt -nvo SOURCE /)
if [ -b "$ROOT_DISK" ]; then
	api "in-use: mounted disk refused ($ROOT_DISK)" 409 POST "/v1/subsystems/$SA/namespaces" "{\"nsid\":3,\"device_path\":\"$ROOT_DISK\"}"
else
	bad "in-use: found the root disk to test with" "got '$ROOT_DISK'"
fi
ini "(nc -l -p 14499 >/dev/null 2>&1 &) ; sleep 0.3"
api "create port on an address someone else holds" 201 POST /v1/ports '{"id":3,"address":"127.0.0.1","service":14499}'
api "kernel EADDRINUSE becomes 409" 409 PUT "/v1/subsystems/$SA/ports/3"
api "failed attach was rolled back" 200 GET "/v1/subsystems/$SA"
expect_eq "port 3 not attached" '[1]' "$(ini "jq -c .ports /tmp/body")"
api "remove port 3" 204 DELETE /v1/ports/3
ini "pkill -f 'nc -l -p 14499' || true"

section "Discovery"
out=$(discover "$H1" $P1 "$HID1")
expect_has "host1 discovers its file subsystem" "$SA" "$out"
expect_has "host1 discovers its block subsystem" "$SB" "$out"
out=$(discover "$H2" $P2 "$HID2")
expect_has "host2 discovers the block subsystem" "$SB" "$out"
expect_lacks "host2 does not see subsystem it may not use" "$SA" "$out"
out=$(discover "$H1" $P2 "$HID1")
expect_has "port 2 lists what is attached to it" "$SB" "$out"
expect_lacks "port 2 does not list what is only on port 1" "$SA" "$out"
out=$(discover "$H3" $P1 "$HID3")
expect_lacks "unknown host discovers nothing" "$NQN:" "$out"

section "Access control"
if out=$(connect "$H3" "$HID3" "$SA" $P1); then bad "unknown host refused" "$out"; disconnect "$SA"; else ok "unknown host refused"; fi
if out=$(connect "$H2" "$HID2" "$SA" $P1); then bad "host not on allow list refused" "$out"; disconnect "$SA"; else ok "host not on allow list refused"; fi

section "Data integrity, multi-target"
connect "$H1" "$HID1" "$SA" $P1 >/dev/null && ok "host1 connects to file subsystem" || bad "host1 connects to file subsystem"
connect "$H1" "$HID1" "$SB" $P1 >/dev/null && ok "host1 connects to block subsystem" || bad "host1 connects to block subsystem"
DA=$(wait_dev "$SA")
DB=$(wait_dev "$SB")
expect_has "file namespace appears" /dev/nvme "$DA"
expect_has "block namespace appears" /dev/nvme "$DB"
expect_eq "model reported to host" nvweaver "$(ini "cat /sys/class/nvme/$(basename "$(ctrl_of "$SA")")/model" | tr -d ' ')"
ini "head -c 16M /dev/urandom > /tmp/pat; md5sum < /tmp/pat | cut -d' ' -f1 > /tmp/pat.md5"
SUM=$(ini "cat /tmp/pat.md5")
ini "dd if=/tmp/pat of=$DA bs=1M oflag=direct status=none; dd if=/tmp/pat of=$DB bs=1M oflag=direct status=none"
expect_eq "read back from file namespace" "$SUM" "$(ini "dd if=$DA bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
expect_eq "read back from block namespace" "$SUM" "$(ini "dd if=$DB bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
expect_eq "data reached the loop device" "$SUM" "$(ini "dd if=$LOOP bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
expect_eq "data reached the volume file" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=/var/lib/nvweaver/volumes/a.img bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"

section "Block sizes hosts see"
FS_BLK=$($ENGINE exec "$TGT" stat -c %o /var/lib/nvweaver/volumes/a.img)
[ "$FS_BLK" -gt 4096 ] && FS_BLK=4096
expect_eq "file namespace uses the filesystem block size" "$FS_BLK" "$(ini "blockdev --getss $DA")"
expect_eq "block namespace keeps the device sector size" "$(ini "blockdev --getss $LOOP")" "$(ini "blockdev --getss $DB")"

section "Live resize"
before=$(ini "blockdev --getsize64 $DA")
api "grow managed volume" 200 PATCH "/v1/subsystems/$SA/namespaces/1" '{"size_bytes":134217728}'
sleep 2
expect_eq "host sees the new size without reconnecting" 134217728 "$(ini "blockdev --getsize64 $DA")"
expect_eq "size was 64M before" 67108864 "$before"

section "IPv6"
if ini "[ -e /proc/net/if_inet6 ] && grep -q '^0*1 ' /proc/net/if_inet6"; then
	api "port on ::1" 201 POST /v1/ports '{"id":6,"address":"::1","service":14426}'
	api "attach it" 204 PUT "/v1/subsystems/$SA/ports/6"
	expect_has "discovery over IPv6" "$SA" "$(ini "nvme discover -t tcp -a ::1 -s 14426 --hostnqn=$H1 --hostid=$HID1" 2>&1)"
	api "detach it" 204 DELETE "/v1/subsystems/$SA/ports/6"
	api "remove it" 204 DELETE /v1/ports/6
else
	echo "  skip  no IPv6 loopback on this host"
fi

section "Wildcard listen address"
if [ "$BACKEND" = nvmet ]; then
	api "port on 0.0.0.0" 201 POST /v1/ports '{"id":7,"address":"0.0.0.0","service":14427}'
	api "attach it" 204 PUT "/v1/subsystems/$SA/ports/7"
	expect_has "hosts can discover through it" "$SA" "$(ini "nvme discover -t tcp -a 127.0.0.1 -s 14427 --hostnqn=$H1 --hostid=$HID1" 2>&1)"
	api "detach it" 204 DELETE "/v1/subsystems/$SA/ports/7"
	api "remove it" 204 DELETE /v1/ports/7
else
	api "0.0.0.0 refused on SPDK" 422 POST /v1/ports '{"id":7,"address":"0.0.0.0","service":14427}'
	expect_has "and says why" "wildcard" "$BODY"
	api "port 7 was not created" 404 GET /v1/ports/7
fi

section "Backing file on overlayfs"
OV=$NQN:overlay
$ENGINE exec "$TGT" sh -c 'dd if=/dev/zero of=/overlay.img bs=1M count=0 seek=64 status=none'
expect_eq "the file really is on overlayfs" 794c7630 "$($ENGINE exec "$TGT" stat -f -c %t /overlay.img)"
api "subsystem for it" 201 POST /v1/subsystems "{\"nqn\":\"$OV\",\"allowed_hosts\":[\"$H1\"],\"ports\":[1]}"
if [ "$BACKEND" = nvmet ]; then
	api "refused: hosts would see a zero-size disk" 422 POST "/v1/subsystems/$OV/namespaces" '{"nsid":1,"device_path":"/overlay.img"}'
	expect_has "and says why" "overlayfs" "$BODY"
else
	api "namespace on it" 201 POST "/v1/subsystems/$OV/namespaces" '{"nsid":1,"device_path":"/overlay.img"}'
	connect "$H1" "$HID1" "$OV" $P1 >/dev/null && ok "host connects" || bad "host connects"
	DO=$(wait_dev "$OV")
	expect_has "namespace appears on the host" /dev/nvme "$DO"
	ini "dd if=/tmp/pat of=$DO bs=1M oflag=direct status=none"
	expect_eq "data round trip" "$SUM" "$(ini "dd if=$DO bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	disconnect "$OV"
fi
api "remove it" 204 DELETE "/v1/subsystems/$OV"

section "Pools and volumes"
POOL_DIR=/var/lib/nvweaver/pool1
$ENGINE exec "$TGT" mkdir -p "$POOL_DIR" /overlay-pool
api "a pool on overlayfs is refused" 422 POST /v1/pools '{"name":"bad","type":"reflink","path":"/overlay-pool"}'
expect_has "and says why" overlayfs "$BODY"
api "a pool type that doesn't exist is refused" 422 POST /v1/pools '{"name":"bad","type":"zfs","path":"/x"}'
api "an LVM pool needs a volume group and thin pool" 422 POST /v1/pools '{"name":"bad","type":"lvm-thin"}'
api "and they must exist" 422 POST /v1/pools '{"name":"bad","type":"lvm-thin","volume_group":"nosuchvg","thin_pool":"pool0"}'
out=$(call POST /v1/pools "{\"name\":\"p1\",\"type\":\"reflink\",\"path\":\"$POOL_DIR\",\"labels\":{\"media\":\"test\"}}")
if [ "${out%%$'\n'*}" != 201 ] && echo "$out" | grep -q "doesn't support reflink"; then
	echo "  skip  this machine's filesystem has no reflink"
else
	expect_eq "adopt a reflink pool" 201 "${out%%$'\n'*}"
	api "adopting it again succeeds" 200 POST /v1/pools "{\"name\":\"p1\",\"type\":\"reflink\",\"path\":\"$POOL_DIR\"}"
	api "the pool reports its state and capacity" 200 GET /v1/pools/p1
	expect_eq "online" online "$(ini "jq -r .state /tmp/body")"
	expect_eq "with a real capacity" true "$(ini "jq '.capacity.total_bytes > 0' /tmp/body")"
	api "create a volume, in the only pool" 201 POST /v1/volumes '{"name":"v1","size_bytes":67108864}'
	VOL_NGUID=$(ini "jq -r .nguid /tmp/body")
	VOL_UUID=$(ini "jq -r .uuid /tmp/body")
	VOL_FILE=$(ini "jq -r .device_path /tmp/body")
	expect_has "its file is real" "$VOL_FILE" "$($ENGINE exec "$TGT" sh -c "ls $POOL_DIR/*.img")"
	api "creating it again succeeds" 200 POST /v1/volumes '{"name":"v1","size_bytes":67108864}'
	expect_eq "with the same identity" "$VOL_NGUID" "$(ini "jq -r .nguid /tmp/body")"
	api "the same name with another size is a conflict" 409 POST /v1/volumes '{"name":"v1","size_bytes":134217728}'
	N1=$NQN:node1
	N2=$NQN:node2
	api "subsystem for node 1" 201 POST /v1/subsystems "{\"nqn\":\"$N1\",\"allowed_hosts\":[\"$H1\"],\"ports\":[1]}"
	api "subsystem for node 2" 201 POST /v1/subsystems "{\"nqn\":\"$N2\",\"allowed_hosts\":[\"$H2\"],\"ports\":[2]}"
	api "map the volume to node 1" 201 POST "/v1/subsystems/$N1/namespaces" '{"volume":"v1"}'
	api "a volume is in one subsystem at a time" 409 POST "/v1/subsystems/$N2/namespaces" '{"volume":"v1"}'
	api "its identity can't be overridden" 422 POST "/v1/subsystems/$N2/namespaces" '{"volume":"v1","uuid":"11111111-1111-4111-8111-111111111111"}'
	api "a mapped volume can't be deleted" 409 DELETE /v1/volumes/v1
	api "the volume shows where it's mapped" 200 GET /v1/volumes/v1
	expect_eq "mapped to node 1" "$N1" "$(ini "jq -r .mapped_to.subsystem /tmp/body")"
	out=$(connect "$H1" "$HID1" "$N1" $P1) && ok "node 1 connects" || bad "node 1 connects" "$out"
	DV=$(wait_dev "$N1")
	expect_has "the volume appears" /dev/nvme "$DV"
	expect_eq "with the volume's NGUID" "$(echo "$VOL_NGUID" | tr -d -)" "$(ini "cat /sys/block/$(basename "$DV")/nguid" | tr -d -)"
	ini "dd if=/tmp/pat of=$DV bs=1M oflag=direct status=none"
	expect_eq "data round trip" "$SUM" "$(ini "dd if=$DV bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	expect_eq "data is in the volume's file" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$VOL_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	api "grow it" 200 PATCH /v1/volumes/v1 '{"size_bytes":134217728}'
	sleep 2
	expect_eq "the host sees the new size" 134217728 "$(ini "blockdev --getsize64 $DV")"
	api "it can't shrink" 422 PATCH /v1/volumes/v1 '{"size_bytes":67108864}'
	disconnect "$N1"
	api "unmap it from node 1" 204 DELETE "/v1/subsystems/$N1/namespaces/1"
	api "it survives being unmapped" 200 GET /v1/volumes/v1
	expect_eq "and is no longer mapped" null "$(ini "jq -r .mapped_to /tmp/body")"
	api "map it to node 2" 201 POST "/v1/subsystems/$N2/namespaces" '{"volume":"v1"}'
	out=$(connect "$H2" "$HID2" "$N2" $P2) && ok "node 2 connects" || bad "node 2 connects" "$out"
	DV=$(wait_dev "$N2")
	expect_eq "same NGUID on another node" "$(echo "$VOL_NGUID" | tr -d -)" "$(ini "cat /sys/block/$(basename "$DV")/nguid" | tr -d -)"
	expect_eq "same UUID on another node" "$VOL_UUID" "$(ini "cat /sys/block/$(basename "$DV")/uuid")"
	expect_eq "same data on another node" "$SUM" "$(ini "dd if=$DV bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	api "snapshot the volume, while a host has it connected" 201 POST /v1/snapshots '{"name":"s1","volume":"v1"}'
	SNAP_FILE=$(ini "jq -r .device_path /tmp/body")
	api "snapshotting it again succeeds" 200 POST /v1/snapshots '{"name":"s1","volume":"v1"}'
	expect_eq "the same file" "$SNAP_FILE" "$(ini "jq -r .device_path /tmp/body")"
	api "the snapshot's file can't be exported directly" 409 POST "/v1/subsystems/$N1/namespaces" "{\"device_path\":\"$SNAP_FILE\"}"
	ini "dd if=/dev/urandom of=$DV bs=1M count=1 oflag=direct status=none"
	V_SUM=$(ini "dd if=$DV bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")
	if [ "$V_SUM" != "$SUM" ]; then ok "the host changes the volume after the snapshot"; else bad "the host changes the volume after the snapshot"; fi
	expect_eq "the snapshot still holds the old data" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$SNAP_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	api "clone the snapshot" 201 POST /v1/volumes '{"name":"c1","source":{"snapshot":"s1"},"size_bytes":268435456}'
	C_NGUID=$(ini "jq -r .nguid /tmp/body")
	C_UUID=$(ini "jq -r .uuid /tmp/body")
	C_FILE=$(ini "jq -r .device_path /tmp/body")
	if [ "$C_NGUID" != "$VOL_NGUID" ] && [ "$C_UUID" != "$VOL_UUID" ]; then ok "the clone has its own identity"; else bad "the clone has its own identity" "$C_NGUID $C_UUID"; fi
	api "cloning it again succeeds" 200 POST /v1/volumes '{"name":"c1","source":{"snapshot":"s1"}}'
	api "the same name from another source is a conflict" 409 POST /v1/volumes '{"name":"c1","source":{"volume":"v1"}}'
	api "a clone can't be smaller than its source" 422 POST /v1/volumes '{"name":"c9","source":{"snapshot":"s1"},"size_bytes":4096}'
	api "map the clone to node 1, alongside the original on node 2" 201 POST "/v1/subsystems/$N1/namespaces" '{"volume":"c1"}'
	out=$(connect "$H1" "$HID1" "$N1" $P1) && ok "node 1 connects to the clone" || bad "node 1 connects to the clone" "$out"
	DC=$(wait_dev "$N1")
	expect_eq "the host sees the clone's larger size" 268435456 "$(ini "blockdev --getsize64 $DC")"
	expect_eq "with the clone's NGUID" "$(echo "$C_NGUID" | tr -d -)" "$(ini "cat /sys/block/$(basename "$DC")/nguid" | tr -d -)"
	expect_eq "the clone holds the snapshot's data, not the volume's" "$SUM" "$(ini "dd if=$DC bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	ini "dd if=/dev/urandom of=$DC bs=1M count=1 seek=4 oflag=direct status=none"
	C_SUM=$(ini "dd if=$DC bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")
	if [ "$C_SUM" != "$SUM" ]; then ok "the host changes the clone"; else bad "the host changes the clone"; fi
	expect_eq "and the original doesn't change" "$V_SUM" "$(ini "dd if=$DV bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	expect_eq "nor does the snapshot" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$SNAP_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	api "clone a volume directly, while it's in use" 201 POST /v1/volumes '{"name":"c2","source":{"volume":"v1"}}'
	C2_FILE=$(ini "jq -r .device_path /tmp/body")
	expect_eq "it holds the volume's current data" "$V_SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$C2_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	disconnect "$N1"
	api "unmap the clone" 204 DELETE "/v1/subsystems/$N1/namespaces/1"
	api "delete the snapshot's first clone" 204 DELETE /v1/volumes/c1
	api "delete the other clone" 204 DELETE /v1/volumes/c2
	expect_eq "their files are gone" 0 "$($ENGINE exec "$TGT" sh -c "ls $C_FILE $C2_FILE 2>/dev/null | wc -l")"
	expect_eq "the snapshot is untouched" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$SNAP_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	disconnect "$N2"
	api "a pool with volumes can't be forgotten" 409 DELETE /v1/pools/p1
	api "a second volume, for paging" 201 POST /v1/volumes '{"name":"v2","pool":"p1","size_bytes":4096}'
	api "first page" 200 GET '/v1/volumes?limit=1'
	expect_eq "one volume, and where to continue" "1 v1" "$(ini "jq -r '\"\(.items | length) \(.next_after)\"' /tmp/body")"
	api "second page" 200 GET '/v1/volumes?limit=1&after=v1'
	expect_eq "the other volume, and no more" "v2 null" "$(ini "jq -r '\"\(.items[0].name) \(.next_after)\"' /tmp/body")"
	$ENGINE exec "$TGT" sh -c "touch $POOL_DIR/nvw-stray.img; touch $POOL_DIR/someone-elses.img"
	api "pool status" 200 GET /v1/pools/p1
	expect_has "reports a file nothing refers to" nvw-stray "$BODY"
	expect_lacks "but ignores what isn't ours" someone-elses "$BODY"
	$ENGINE exec "$TGT" rm -f "$POOL_DIR/nvw-stray.img" "$POOL_DIR/someone-elses.img"
	api "delete a namespace on node 2" 204 DELETE "/v1/subsystems/$N2/namespaces/1"
	api "delete the volume" 204 DELETE /v1/volumes/v1
	api "delete the other volume" 204 DELETE /v1/volumes/v2
	api "the snapshot outlives its volume" 200 GET /v1/snapshots/s1
	expect_eq "and keeps its data" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$SNAP_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	api "a pool with snapshots can't be forgotten" 409 DELETE /v1/pools/p1
	api "clone it after its volume has gone" 201 POST /v1/volumes '{"name":"c3","source":{"snapshot":"s1"}}'
	C3_FILE=$(ini "jq -r .device_path /tmp/body")
	api "delete the snapshot" 204 DELETE /v1/snapshots/s1
	expect_eq "and the clone keeps its data" "$SUM" "$($ENGINE exec "$TGT" sh -c "dd if=$C3_FILE bs=1048576 count=16 2>/dev/null | md5sum | cut -d' ' -f1")"
	api "delete that clone" 204 DELETE /v1/volumes/c3
	expect_eq "their files are gone" 0 "$($ENGINE exec "$TGT" sh -c "ls $POOL_DIR | grep -c img")"
	api "remove node 1" 204 DELETE "/v1/subsystems/$N1"
	api "remove node 2" 204 DELETE "/v1/subsystems/$N2"
	api "forget the pool" 204 DELETE /v1/pools/p1
fi

section "Multi-host and multipath"
api "shared subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$SC\",\"allowed_hosts\":[\"$H1\",\"$H2\"],\"ports\":[1,2]}"
api "shared namespace" 201 POST "/v1/subsystems/$SC/namespaces" '{"nsid":1,"file":{"name":"c.img","size_bytes":67108864}}'
connect "$H1" "$HID1" "$SC" $P1 >/dev/null && ok "host1 connects" || bad "host1 connects"
out=$(connect "$H2" "$HID2" "$SC" $P2) && ok "host2 connects at the same time" || bad "host2 connects at the same time" "$out"
expect_eq "two live controllers" 2 "$(live_paths "$SC")"
DC=$(wait_dev "$SC")
ini "dd if=/tmp/pat of=$DC bs=1M oflag=direct status=none"
ini "nvme disconnect -n $SC >/dev/null"
connect "$H2" "$HID2" "$SC" $P2 >/dev/null && ok "host2 reconnects alone" || bad "host2 reconnects alone"
DC=$(wait_dev "$SC")
expect_eq "host2 reads what was written" "$SUM" "$(ini "dd if=$DC bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
disconnect "$SC"

section "DH-HMAC-CHAP"
KEY=$(ini "nvme gen-dhchap-key -m 1 -n $H3 $NQN:unused 2>/dev/null || nvme gen-dhchap-key --nqn=$H3 --hmac=1")
WRONG=$(ini "nvme gen-dhchap-key --nqn=$H3 --hmac=1")
expect_has "generated a key" DHHC-1 "$KEY"
api "host with a key" 201 POST /v1/hosts "{\"nqn\":\"$H3\",\"dhchap_key\":\"$KEY\",\"dhchap_hash\":\"hmac(sha256)\"}"
expect_lacks "key is not echoed" "$KEY" "$BODY"
api "subsystem requiring it" 201 POST /v1/subsystems "{\"nqn\":\"$SD\",\"allowed_hosts\":[\"$H3\"],\"ports\":[1]}"
api "its namespace" 201 POST "/v1/subsystems/$SD/namespaces" '{"nsid":1,"file":{"name":"d.img","size_bytes":67108864}}'
if out=$(connect "$H3" "$HID3" "$SD" $P1); then bad "no key refused" "$out"; disconnect "$SD"; else ok "no key refused"; fi
if out=$(connect "$H3" "$HID3" "$SD" $P1 "--dhchap-secret=$WRONG"); then bad "wrong key refused" "$out"; disconnect "$SD"; else ok "wrong key refused"; fi
out=$(connect "$H3" "$HID3" "$SD" $P1 "--dhchap-secret=$KEY") && ok "right key accepted" || bad "right key accepted" "$out"
disconnect "$SD"
api "remove auth subsystem" 204 DELETE "/v1/subsystems/$SD"

section "NVMe block device as namespace backing"
api "subsystem backed by an NVMe namespace" 201 POST /v1/subsystems "{\"nqn\":\"$SF\",\"allowed_hosts\":[\"$H2\"],\"ports\":[2]}"
api "namespace on $DA" 201 POST "/v1/subsystems/$SF/namespaces" "{\"nsid\":1,\"device_path\":\"$DA\"}"
connect "$H2" "$HID2" "$SF" $P2 >/dev/null && ok "host connects" || bad "host connects"
DF=$(wait_dev "$SF")
expect_eq "reads the data on the NVMe namespace" "$SUM" "$(ini "dd if=$DF bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
disconnect "$SF"
api "remove it" 204 DELETE "/v1/subsystems/$SF"

section "NVMe passthrough"
if [ "$BACKEND" = nvmet ]; then
# One kernel cannot attach the same namespace twice ("duplicate IDs"), so no
# block device appears here. Identify still proves commands reach the real
# controller: it must return the underlying namespace's NGUID.
PCTRL=$(ctrl_of "$SA")
api "passthrough subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$SE\",\"allowed_hosts\":[\"$H2\"],\"ports\":[2],\"passthru_device\":\"$PCTRL\"}"
api "namespaces refused on passthrough" 409 POST "/v1/subsystems/$SE/namespaces" '{"file":{"name":"no.img","size_bytes":4096}}'
out=$(connect "$H2" "$HID2" "$SE" $P2) && ok "host connects through passthrough" || bad "host connects through passthrough" "$out"
api "read file namespace identity" 200 GET "/v1/subsystems/$SA/namespaces/1"
WANT_NGUID=$(ini "jq -r .nguid /tmp/body")
PDEV=$(ctrl_of "$SE")
expect_eq "Identify reaches the underlying namespace" "$WANT_NGUID" "$(ini "nvme id-ns $PDEV -n 1 -o json | jq -r .nguid")"
disconnect "$SE"
api "remove passthrough subsystem" 204 DELETE "/v1/subsystems/$SE"
else
	api "passthrough refused on SPDK" 422 POST /v1/subsystems "{\"nqn\":\"$SE\",\"passthru_device\":\"/dev/nvme0\"}"
	api "buffered_io refused on SPDK" 422 POST "/v1/subsystems/$SA/namespaces" "{\"nsid\":9,\"buffered_io\":true,\"file\":{\"name\":\"no.img\",\"size_bytes\":4096}}"
	api "inline_data_size refused on SPDK" 422 POST /v1/ports '{"id":9,"address":"127.0.0.1","service":14429,"inline_data_size":8192}'
	api "failed namespace left no file behind" 201 POST "/v1/subsystems/$SA/namespaces" '{"nsid":9,"file":{"name":"no.img","size_bytes":4096}}'
	api "remove it" 204 DELETE "/v1/subsystems/$SA/namespaces/9?delete_file=true"
fi

section "Restart with teardown (default): hosts recover, identity is kept"
ID_BEFORE=$(call GET "/v1/subsystems/$SA" | tail -n +2)
$ENGINE stop -t 20 "$TGT" >/dev/null
KEEP=true
[ "$BACKEND" = spdk ] && KEEP=false
start_target $KEEP || bad "restart"
for _ in $(seq 1 $((60 * SCALE))); do [ "$(live_paths "$SA")" = 1 ] && break; sleep 0.5; done
expect_eq "host reconnected by itself" 1 "$(live_paths "$SA")"
expect_eq "data survived" "$SUM" "$(ini "dd if=$DA bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
expect_eq "serial, UUID and NGUID unchanged" "$ID_BEFORE" "$(call GET "/v1/subsystems/$SA" | tail -n +2)"

section "Restart with NVWEAVER_KEEP_ON_EXIT: hosts do not notice"
if [ "$BACKEND" = nvmet ]; then
CTRL=$(basename "$(ctrl_of "$SA")")
CNTLID=$(ini "cat /sys/class/nvme/$CTRL/cntlid")
ini "rm -f /tmp/io.stop /tmp/io.err; echo 0 > /tmp/io.n"
ini "(n=0; while [ ! -f /tmp/io.stop ]; do
	dd if=/dev/urandom of=$DA bs=64k count=64 seek=256 oflag=direct status=none || { echo \$n > /tmp/io.err; break; }
	n=\$((n+1)); echo \$n > /tmp/io.n
done) >/dev/null 2>&1 &"
sleep 2
N1=$(ini "cat /tmp/io.n")
$ENGINE stop -t 20 "$TGT" >/dev/null
N2=$(ini "cat /tmp/io.n")
sleep 2
N3=$(ini "cat /tmp/io.n")
if [ "$N3" -gt "$N2" ]; then ok "I/O continues with no container running ($N2 -> $N3 passes)"; else bad "I/O continues with no container running" "$N2 -> $N3"; fi
start_target true || bad "restart"
sleep 2
ini "touch /tmp/io.stop"
sleep 1
N4=$(ini "cat /tmp/io.n")
if [ "$N4" -gt "$N1" ]; then ok "I/O ran throughout ($N4 passes)"; else bad "I/O ran throughout" "$N1 -> $N4"; fi
expect_eq "no I/O errors" no "$(ini "[ -f /tmp/io.err ] && echo yes || echo no")"
expect_eq "same controller: the host never reconnected" "$CNTLID" "$(ini "cat /sys/class/nvme/$CTRL/cntlid")"
expect_eq "controller live" live "$(ini "cat /sys/class/nvme/$CTRL/state")"
else
	$ENGINE rm -f "$TGT" >/dev/null 2>&1
	$ENGINE run --rm --privileged --network host -v "$VOL:/var/lib/nvweaver" -e NVWEAVER_LISTEN=127.0.0.1:18420 \
		-e NVWEAVER_BACKEND=spdk -e NVWEAVER_KEEP_ON_EXIT=true "$IMAGE" >/tmp/nvweaver-e2e.$$ 2>&1
	expect_has "refused on SPDK, whose data path stops with the container" "cannot work with the spdk backend" "$(cat /tmp/nvweaver-e2e.$$)"
	rm -f /tmp/nvweaver-e2e.$$
	start_target false || bad "restart"
	for _ in $(seq 1 $((60 * SCALE))); do [ "$(live_paths "$SA")" = 1 ] && break; sleep 0.5; done
fi

section "Teardown"
disconnect "$SA"
disconnect "$SB"

section "A crash while a pool is changing"
KPOOL=/var/lib/nvweaver/kill-pool
$ENGINE exec "$TGT" mkdir -p "$KPOOL"
out=$(call POST /v1/pools "{\"name\":\"kp\",\"type\":\"reflink\",\"path\":\"$KPOOL\"}")
if [ "${out%%$'\n'*}" != 201 ] && echo "$out" | grep -q "doesn't support reflink"; then
	echo "  skip  this machine's filesystem has no reflink"
else
	expect_eq "adopt a pool" 201 "${out%%$'\n'*}"
	ini "rm -f /tmp/kill.*"
	IN_PROGRESS=0
	for round in 1 2 3; do
		# Each line is something the service said it did, so it must still be true afterwards.
		ini "(n=0; while :; do n=\$((n+1)); v=k$round-\$n
			[ \"\$(curl -s -m 20 -o /dev/null -w '%{http_code}' -X POST $API/v1/volumes -d '{\"name\":\"'\$v'\",\"size_bytes\":4096}')\" = 201 ] || break
			[ \"\$(curl -s -m 20 -o /dev/null -w '%{http_code}' -X POST $API/v1/snapshots -d '{\"name\":\"s'\$v'\",\"volume\":\"'\$v'\"}')\" = 201 ] || break
			echo s\$v >> /tmp/kill.snapshots
			if [ \$((n % 2)) = 0 ]; then
				[ \"\$(curl -s -m 20 -o /dev/null -w '%{http_code}' -X DELETE $API/v1/volumes/\$v)\" = 204 ] || break
				echo \$v >> /tmp/kill.deleted
			else
				echo \$v >> /tmp/kill.volumes
			fi
		done; touch /tmp/kill.done$round) >/dev/null 2>&1 &"
		sleep $((2 * SCALE))
		$ENGINE kill -s KILL "$TGT" >/dev/null
		for _ in $(seq 1 $((60 * SCALE))); do [ "$(ini "[ -f /tmp/kill.done$round ] && echo yes")" = yes ] && break; sleep 0.5; done
		n=$($ENGINE run --rm --pull=never --entrypoint sh -v "$VOL:/d" "$IMAGE" -c 'ls /d/intents | wc -l')
		IN_PROGRESS=$((IN_PROGRESS + n))
		start_target false || bad "restart after a crash"
	done
	echo "  (changes in progress at the moment of the crash: $IN_PROGRESS)"
	DONE=$(ini "cat /tmp/kill.snapshots /tmp/kill.volumes 2>/dev/null | wc -l")
	if [ "$DONE" -gt 0 ]; then ok "the service was really doing work when killed ($DONE changes confirmed)"; else bad "the service was really doing work when killed"; fi
	expect_eq "no intents are left" 0 "$($ENGINE exec "$TGT" sh -c 'ls /var/lib/nvweaver/intents | wc -l')"
	expect_eq "every volume it confirmed is there" "" "$(ini "for v in \$(cat /tmp/kill.volumes 2>/dev/null); do [ \"\$(curl -s -o /dev/null -w '%{http_code}' $API/v1/volumes/\$v)\" = 200 ] || echo \$v; done")"
	expect_eq "every snapshot it confirmed is there" "" "$(ini "for v in \$(cat /tmp/kill.snapshots 2>/dev/null); do [ \"\$(curl -s -o /dev/null -w '%{http_code}' $API/v1/snapshots/\$v)\" = 200 ] || echo \$v; done")"
	expect_eq "and no volume it deleted" "" "$(ini "for v in \$(cat /tmp/kill.deleted 2>/dev/null); do [ \"\$(curl -s -o /dev/null -w '%{http_code}' $API/v1/volumes/\$v)\" = 404 ] || echo \$v; done")"
	api "pool status" 200 GET /v1/pools/kp
	expect_eq "no file is left that nothing refers to" 0 "$(ini "jq '[.warnings[] | select(contains(\"no volume refers\"))] | length' /tmp/body")"
	expect_eq "the pool holds one file for each" "$(ini "jq '.volumes + .snapshots' /tmp/body")" "$($ENGINE exec "$TGT" sh -c "ls $KPOOL | grep -c img")"
	for v in $(ini "curl -s '$API/v1/volumes?limit=1000' | jq -r '.items[].name'"); do call DELETE "/v1/volumes/$v" >/dev/null; done
	for v in $(ini "curl -s '$API/v1/snapshots?limit=1000' | jq -r '.items[].name'"); do call DELETE "/v1/snapshots/$v" >/dev/null; done
	expect_eq "it all deletes" 0 "$($ENGINE exec "$TGT" sh -c "ls $KPOOL | grep -c img")"
	api "forget the pool" 204 DELETE /v1/pools/kp
fi

section "Teardown continues"
if [ "$BACKEND" = nvmet ]; then
	FOREIGN=$NQN:not-ours
	ini "mount -t configfs configfs /mnt && mkdir /mnt/nvmet/subsystems/$FOREIGN"
	start_target false || bad "restart in teardown mode"
	expect_has "kernel has the subsystem while running" "$SA" "$($ENGINE exec "$TGT" ls /sys/kernel/config/nvmet/subsystems)"
	$ENGINE stop -t 20 "$TGT" >/dev/null
	expect_eq "configfs readable to check" yes "$(ini "[ -d /mnt/nvmet/subsystems ] && echo yes || echo no")"
	expect_eq "stopping removes all our subsystems" "$FOREIGN" "$(ini "ls /mnt/nvmet/subsystems")"
	expect_eq "stopping removes all our ports" "" "$(ini "ls /mnt/nvmet/ports")"
	expect_eq "stopping removes all our hosts" "" "$(ini "ls /mnt/nvmet/hosts")"
	ini "rmdir /mnt/nvmet/subsystems/$FOREIGN; umount /mnt"
else
	expect_has "port is open while running" LISTEN "$(ini "ss -ltn sport = :$P1 2>/dev/null || netstat -ltn | grep :$P1")"
	$ENGINE stop -t 20 "$TGT" >/dev/null
	expect_eq "container exits cleanly" 0 "$($ENGINE inspect "$TGT" --format '{{.State.ExitCode}}')"
	expect_lacks "stopping closes the port" LISTEN "$(ini "ss -ltn sport = :$P1 2>/dev/null || netstat -ltn | grep :$P1")"
	LOGS=$($ENGINE logs "$TGT" 2>&1)
	expect_has "the target stops with the service" 'target stops with the service' "$LOGS"
	expect_lacks "and isn't taken apart first" 'target configuration removed' "$LOGS"
	expect_lacks "and didn't have to be killed" 'did not stop in time' "$LOGS"
	section "SPDK process failure"
	start_target false || bad "start"
	# A child belongs to the thread that started it, and Go can use any thread, so read them all.
	SPDK_PID=$($ENGINE exec "$TGT" sh -c 'cat /proc/1/task/*/children' 2>/dev/null | tr -d ' \n')
	if [ -n "$SPDK_PID" ]; then ok "found nvmf_tgt to kill"; else bad "found nvmf_tgt to kill" "the service has no child process"; fi
	# The image has no kill binary, only the shell's builtin. Errors stay visible, to explain a failure below.
	$ENGINE exec "$TGT" sh -c "kill -9 $SPDK_PID"
	for _ in $(seq 1 $((40 * SCALE))); do [ "$($ENGINE inspect "$TGT" --format '{{.State.Running}}')" = false ] && break; sleep 0.25; done
	expect_eq "service exits so a restart policy can recover" false "$($ENGINE inspect "$TGT" --format '{{.State.Running}}')"
	expect_eq "with a failure status" 1 "$($ENGINE inspect "$TGT" --format '{{.State.ExitCode}}')"

	section "Automatic fallback, and moving between backends"
	if [ "$MODULES_WERE_LOADED" = 0 ]; then
		run_target() { # BACKEND [extra run args]
			local backend=$1
			shift
			$ENGINE rm -f "$TGT" >/dev/null 2>&1
			$ENGINE run -d --name "$TGT" --privileged --network host -v /dev:/dev -v "$VOL:/var/lib/nvweaver" \
				-e NVWEAVER_LISTEN=127.0.0.1:18420 -e NVWEAVER_BACKEND="$backend" "$@" "$IMAGE" >/dev/null || return 1
			for _ in $(seq 1 $((40 * SCALE))); do
				[ "$(ini "curl -s -o /dev/null -w '%{http_code}' $API/healthz")" = 200 ] && return 0
				sleep 0.25
			done
			return 1
		}
		identity() { # DEVICE: what a host uses to recognise a namespace
			local n
			n=/sys/block/$(basename "$1")
			ini "cat $n/uuid $n/nguid $n/queue/logical_block_size" | tr '\n' ' '
		}
		# With no /lib/modules the kernel target cannot load, as on a host without it.
		run_target auto || bad "auto starts without kernel modules"
		api "system info" 200 GET /v1/system
		expect_has "auto falls back to SPDK" '"backend": "spdk"' "$BODY"
		expect_has "and says why" "falling back to SPDK" "$($ENGINE logs "$TGT" 2>&1)"
		connect "$H1" "$HID1" "$SA" $P1 >/dev/null && ok "host connects to the fallback" || bad "host connects to the fallback"
		DA=$(wait_dev "$SA")
		ID_SPDK=$(identity "$DA")
		expect_eq "data is there" "$SUM" "$(ini "dd if=$DA bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
		disconnect "$SA"
		$ENGINE stop -t 20 "$TGT" >/dev/null

		run_target auto -v /lib/modules:/lib/modules:ro || bad "auto starts with kernel modules"
		api "system info" 200 GET /v1/system
		expect_has "auto prefers the kernel when it can load" '"backend": "nvmet"' "$BODY"
		connect "$H1" "$HID1" "$SA" $P1 >/dev/null && ok "same host connects to the kernel target" || bad "same host connects to the kernel target"
		DA=$(wait_dev "$SA")
		expect_eq "same data after moving backend" "$SUM" "$(ini "dd if=$DA bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")"
		expect_eq "same UUID, NGUID and block size" "$ID_SPDK" "$(identity "$DA")"
		disconnect "$SA"
		$ENGINE stop -t 20 "$TGT" >/dev/null
	else
		echo "  skip  needs nvmet-tcp unloaded on the host"
	fi
fi

[ "$fail" = 0 ]
