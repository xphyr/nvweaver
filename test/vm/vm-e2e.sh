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
# TEST_TIMEOUT_SCALE lengthens the waits, for slow machines such as emulated VMs.
SCALE=${TEST_TIMEOUT_SCALE:-1}

LAB=$(dirname "$0")/lab.sh
API=http://127.0.0.1:8420
TARGET_IP=10.99.0.1
PORT=4420
NQN=nqn.2026-11.net.xphyr.vm
NVME=/dev/disk/by-id/nvme-QEMU_NVMe_Ctrl_NVWEAVER0001
SATA=/dev/disk/by-id/ata-QEMU_HARDDISK_NVWEAVERSATA
VIRTIO=/dev/disk/by-id/virtio-NVWEAVERVIRTIO
LVM_DISK=/dev/disk/by-id/virtio-NVWEAVERLVM
VG=nvwlab
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
	for _ in $(seq 1 $((120 * SCALE))); do
		[ "$(t "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz" 2>/dev/null)" = 200 ] && return 0
		sleep 0.5
	done
	return 1
}

# The namespace's block device on the initiator, once it can be opened.
ini_dev() { # SUBNQN
	local d
	for _ in $(seq 1 $((60 * SCALE))); do
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
	for _ in $(seq 1 $((90 * SCALE))); do
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
t "sudo pvcreate -q -y --metadatasize 16m $LVM_DISK && sudo vgcreate -q $VG $LVM_DISK && sudo lvcreate -q -y --type thin-pool -L 6G --poolmetadatasize 256m -n pool0 $VG" &&
	ok "create a thin pool to adopt" || bad "create a thin pool to adopt"
expect_eq "initiator can reach the target only over the link" "$TARGET_IP" "$(i "ip -o route get $TARGET_IP | sed -E 's/.* dev lab0 .*/$TARGET_IP/'")"

# --pid=host is what lets the service use the host's LVM.
start_service() { # BACKEND IMAGE
	t "sudo podman rm -f nvweaver-vm >/dev/null 2>&1; sudo podman run -d --pull=never --name nvweaver-vm --privileged --network host --pid=host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-vm-data:/var/lib/nvweaver \
		-e NVWEAVER_BACKEND=$1 $2 >/dev/null"
}

backend_tests() { # BACKEND IMAGE [run args]
	local backend=$1 image=$2 name disk sub dev sum
	shift 2
	section "$backend: every disk type, from a separate initiator"
	start_service "$backend" "$image"
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
	snapshot_tests "$backend" reflink
	snapshot_tests "$backend" lvm-thin
	concurrent_tests "$backend" reflink
	concurrent_tests "$backend" lvm-thin
	disconnect_tests "$backend"
	[ "$backend" = spdk ] && aio_limit_tests
	lvm_tests "$backend" "$image"
	full_pool_tests "$backend"
	power_tests "$backend" "$image" lvm-thin
	power_tests "$backend" "$image" reflink
}

our_lvs() { t "sudo lvs --noheadings -o lv_name $VG | grep -c nvw-"; }
pool_data_percent() { t "sudo sync; sudo lvs --noheadings -o data_percent $VG/pool0 | tr -d ' '"; }

# What a client relies on a snapshot for: real files on a real filesystem, including one taken while
# they're being written. A file whose fsync finished before the snapshot was asked for must be in it.
snapshot_tests() { # BACKEND POOL-TYPE
	local backend=$1 kind=$2 sub=$NQN:$1-$2-snap sub_a=$NQN:$1-$2-clone-a sub_b=$NQN:$1-$2-clone-b
	local dev dev_a dev_b out written committed pool_dir="" before=""
	section "$backend, $kind: snapshots and clones, checked as real files on a real filesystem"
	if [ "$kind" = reflink ]; then
		t "sudo podman exec nvweaver-vm mkdir -p /var/lib/nvweaver/snap-pool"
		api "adopt a pool" 201 POST /v1/pools '{"name":"snap","type":"reflink","path":"/var/lib/nvweaver/snap-pool"}'
		pool_dir=$(t "sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}'")/snap-pool
	else
		api "adopt a pool" 201 POST /v1/pools "{\"name\":\"snap\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
	fi
	api "create a volume" 201 POST /v1/volumes '{"name":"fs","size_bytes":1073741824}'
	for entry in "$sub:" "$sub_a:" "$sub_b:"; do
		api "subsystem ${entry%:*}" 201 POST /v1/subsystems "{\"nqn\":\"${entry%:*}\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	done
	api "map the volume" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"fs"}'
	out=$(connect "$sub") && ok "host connects" || bad "host connects" "$out"
	dev=$(ini_dev "$sub")
	if [ -z "$dev" ]; then
		bad "the volume appears on the initiator"
		return
	fi
	i "sudo mkfs.xfs -q -f $dev && sudo mkdir -p /mnt/snap /mnt/snap-a /mnt/snap-b && sudo mount $dev /mnt/snap && sudo chmod 777 /mnt/snap" &&
		ok "make and mount a filesystem on it" || bad "make and mount a filesystem on it"
	i 'rm -f /tmp/snap.*; cd /mnt/snap && for n in $(seq 1 32); do head -c 8M /dev/urandom > base$n; done && sync -f . && md5sum base* > /tmp/snap.manifest'
	expect_eq "write 256 MiB of files, and record their checksums" 32 "$(i 'wc -l < /tmp/snap.manifest')"

	[ "$kind" = lvm-thin ] && before=$(pool_data_percent)
	api "snapshot it with the filesystem mounted" 201 POST /v1/snapshots '{"name":"snap-a","volume":"fs"}'
	api "clone that snapshot" 201 POST /v1/volumes '{"name":"clone-a","source":{"snapshot":"snap-a"}}'
	local a_internal
	a_internal=$(t "jq -r .internal_name /tmp/b")
	if [ "$kind" = reflink ]; then
		# Total, exclusive and shared bytes, for a clone that nothing has written to yet.
		out=$(t "sync; sudo btrfs filesystem du --raw $pool_dir/$a_internal.img | tail -1")
		local total exclusive
		total=$(echo "$out" | awk '{print $1}')
		exclusive=$(echo "$out" | awk '{print $2}')
		if [ "${total:-0}" -ge 268435456 ] && [ "${exclusive:-1073741824}" -lt 16777216 ]; then
			ok "the clone shares its data, rather than copying it ($total bytes, $exclusive its own)"
		else
			bad "the clone shares its data, rather than copying it" "btrfs says: $out"
		fi
	else
		# A full copy of the 256 MiB of files would take over 4% of this pool.
		out=$(pool_data_percent)
		if [ -n "$before" ] && [ -n "$out" ] && awk "BEGIN { exit !($before > 3 && $out - $before < 1) }"; then
			ok "the clone shares its data, rather than copying it (the pool went from $before% to $out%)"
		else
			bad "the clone shares its data, rather than copying it" "the pool went from $before% to $out%"
		fi
		expect_eq "the snapshot stays inactive until it's needed" "" "$(t "sudo lvs --noheadings -o lv_active $VG/\$(curl -s $API/v1/snapshots/snap-a | jq -r .internal_name) | tr -d ' '")"
		expect_eq "and is read-only" r "$(t "sudo lvs --noheadings -o lv_attr $VG/\$(curl -s $API/v1/snapshots/snap-a | jq -r .internal_name) | tr -d ' ' | cut -c2")"
		expect_eq "the clone is active" active "$(t "sudo lvs --noheadings -o lv_active $VG/$a_internal | tr -d ' '")"
	fi

	# Writes under load. Each line in the journal is a file that fsync has finished for.
	i "setsid nohup bash -c 'cd /mnt/snap; n=0; while [ ! -f /tmp/snap.stop ]; do n=\$((n+1)); dd if=/dev/urandom of=w\$n bs=64k count=16 conv=fsync status=none || break; sync . ; md5sum w\$n >> /tmp/snap.journal; done; touch /tmp/snap.done' </dev/null >/dev/null 2>&1 &"
	for _ in $(seq 1 $((60 * SCALE))); do
		written=$(i 'wc -l < /tmp/snap.journal 2>/dev/null || echo 0')
		[ "${written:-0}" -ge 8 ] && break
		sleep 0.5
	done
	committed=$(i 'cat /tmp/snap.journal | wc -l')
	api "snapshot it while a host is writing" 201 POST /v1/snapshots '{"name":"snap-b","volume":"fs"}'
	i 'touch /tmp/snap.stop'
	for _ in $(seq 1 $((60 * SCALE))); do
		[ "$(i '[ -f /tmp/snap.done ] && echo yes')" = yes ] && break
		sleep 0.5
	done
	written=$(i 'wc -l < /tmp/snap.journal')
	if [ "$committed" -gt 0 ] && [ "$written" -gt "$committed" ]; then
		ok "writes were really running, before and after it ($committed, then $written files)"
	else
		bad "writes were really running, before and after it" "$committed, then $written files"
	fi
	api "clone the snapshot taken under load" 201 POST /v1/volumes '{"name":"clone-b","source":{"snapshot":"snap-b"}}'

	# Change the original, so that a clone still holding the old data shows they're independent.
	i 'cd /mnt/snap && head -c 8M /dev/urandom > base1 && touch only-in-original && sync -f .'
	out=$(i 'cd /mnt/snap && md5sum -c --quiet /tmp/snap.manifest 2>/dev/null | wc -l')
	expect_eq "the original has really changed" 1 "$out"

	# XFS refuses a second filesystem with the same UUID on a host without nouuid, as a clone has.
	api "map the first clone" 201 POST "/v1/subsystems/$sub_a/namespaces" '{"volume":"clone-a"}'
	out=$(connect "$sub_a") && ok "host connects to the first clone" || bad "host connects to the first clone" "$out"
	dev_a=$(ini_dev "$sub_a")
	if out=$(i "sudo mount -o nouuid $dev_a /mnt/snap-a" 2>&1); then ok "it mounts"; else bad "it mounts" "$out"; fi
	expect_eq "every file from before the snapshot is intact" "rc=0" "$(i 'cd /mnt/snap-a && md5sum -c --quiet /tmp/snap.manifest >/dev/null 2>&1; echo rc=$?')"
	expect_eq "nothing written after it is there" 0 "$(i 'ls /mnt/snap-a | grep -c -e "^w" -e only-in-original')"

	api "map the second clone" 201 POST "/v1/subsystems/$sub_b/namespaces" '{"volume":"clone-b"}'
	out=$(connect "$sub_b") && ok "host connects to the second clone" || bad "host connects to the second clone" "$out"
	dev_b=$(ini_dev "$sub_b")
	if out=$(i "sudo mount -o nouuid $dev_b /mnt/snap-b" 2>&1); then ok "it mounts, replaying its log"; else bad "it mounts, replaying its log" "$out"; fi
	expect_eq "the files from before it are intact" "rc=0" "$(i 'cd /mnt/snap-b && md5sum -c --quiet /tmp/snap.manifest >/dev/null 2>&1; echo rc=$?')"
	expect_eq "and so is every file finished before it was asked for ($committed)" "rc=0" \
		"$(i "cd /mnt/snap-b && head -n $committed /tmp/snap.journal | md5sum -c --quiet >/dev/null 2>&1; echo rc=\$?")"
	i 'cd /mnt/snap-b && touch only-in-clone && sync -f .'
	expect_eq "a file made on the clone isn't on the original" 0 "$(i 'ls /mnt/snap | grep -c only-in-clone')"
	expect_eq "nor one made on the original on the clone" 0 "$(i 'ls /mnt/snap-b | grep -c only-in-original')"

	i 'sudo umount /mnt/snap /mnt/snap-a /mnt/snap-b'
	for d in "$dev_a" "$dev_b"; do
		if out=$(i "sudo xfs_repair -n $d" 2>&1); then ok "the filesystem checks clean on $d"; else bad "the filesystem checks clean on $d" "$(echo "$out" | tail -5)"; fi
	done

	# Each survives the others going. The snapshots are all that's left of what was written.
	disconnect "$sub"
	disconnect "$sub_a"
	disconnect "$sub_b"
	api "unmap the original" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "unmap the first clone" 204 DELETE "/v1/subsystems/$sub_a/namespaces/1"
	api "delete the original" 204 DELETE /v1/volumes/fs
	api "delete the first clone" 204 DELETE /v1/volumes/clone-a
	api "clone the first snapshot again, with its volume gone" 201 POST /v1/volumes '{"name":"clone-c","source":{"snapshot":"snap-a"}}'
	api "delete the snapshots" 204 DELETE /v1/snapshots/snap-a
	api "delete the other" 204 DELETE /v1/snapshots/snap-b
	api "map the last clone" 201 POST "/v1/subsystems/$sub_a/namespaces" '{"volume":"clone-c"}'
	out=$(connect "$sub_a") && ok "host connects" || bad "host connects" "$out"
	dev_a=$(ini_dev "$sub_a")
	if out=$(i "sudo mount -o nouuid $dev_a /mnt/snap-a" 2>&1); then ok "it mounts"; else bad "it mounts" "$out"; fi
	expect_eq "with its files intact, and no snapshot or volume behind it" "rc=0" "$(i 'cd /mnt/snap-a && md5sum -c --quiet /tmp/snap.manifest >/dev/null 2>&1; echo rc=$?')"
	i 'sudo umount /mnt/snap-a'
	disconnect "$sub_a"

	api "unmap it" 204 DELETE "/v1/subsystems/$sub_a/namespaces/1"
	api "unmap the second clone" 204 DELETE "/v1/subsystems/$sub_b/namespaces/1"
	api "delete the last clone" 204 DELETE /v1/volumes/clone-c
	api "delete the second clone" 204 DELETE /v1/volumes/clone-b
	for s in "$sub" "$sub_a" "$sub_b"; do
		api "remove subsystem" 204 DELETE "/v1/subsystems/$s"
	done
	if [ "$kind" = reflink ]; then
		expect_eq "the pool's files are gone" 0 "$(t "sudo ls $pool_dir | grep -c img")"
	else
		expect_eq "the pool's volumes are gone" 0 "$(our_lvs)"
	fi
	api "forget the pool" 204 DELETE /v1/pools/snap
}

# Storage work runs at the same time, without the service's lock. Lots of it at once on real storage, as a
# client would send it, retrying a 503. Clones have to hold the right data, and nothing may be lost or left over.
concurrent_tests() { # BACKEND POOL-TYPE
	local backend=$1 kind=$2 sub=$NQN:$1-$2-many src=$NQN:$1-$2-many-src n=16 out sums pool_dir="" devs
	section "$backend, $kind: many changes at once"
	if [ "$kind" = reflink ]; then
		t "sudo podman exec nvweaver-vm mkdir -p /var/lib/nvweaver/many-pool"
		api "adopt a pool" 201 POST /v1/pools '{"name":"many","type":"reflink","path":"/var/lib/nvweaver/many-pool"}'
		pool_dir=$(t "sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}'")/many-pool
	else
		api "adopt a pool" 201 POST /v1/pools "{\"name\":\"many\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
	fi
	api "create the volume to clone" 201 POST /v1/volumes '{"name":"src","size_bytes":67108864}'
	api "subsystem for it" 201 POST /v1/subsystems "{\"nqn\":\"$src\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "map it" 201 POST "/v1/subsystems/$src/namespaces" '{"volume":"src"}'
	out=$(connect "$src") && ok "host connects" || bad "host connects" "$out"
	local dev
	dev=$(ini_dev "$src")
	i "sudo dd if=/dev/urandom of=$dev bs=1M count=16 oflag=direct status=none; sync"
	local want
	want=$(sum_of i "$dev")
	disconnect "$src"

	t "cat > /tmp/many.sh" <<EOF
API=$API
n=$n
sub=$sub
HOST=$HOST
$(cat <<'INNER'
# Sends a request, retrying while the service says it's busy. Prints only what went wrong.
req() { # METHOD PATH WANT [JSON]
	local i code
	for i in $(seq 1 1500); do
		if [ -n "${4:-}" ]; then
			code=$(curl -s -m 120 -o /dev/null -w '%{http_code}' -X "$1" -d "$4" "$API$2")
		else
			code=$(curl -s -m 120 -o /dev/null -w '%{http_code}' -X "$1" "$API$2")
		fi
		if [ "$code" = 503 ]; then
			echo busy >> /tmp/many.busy
			sleep 0.2
			continue
		fi
		[ "$code" = "$3" ] || echo "$1 $2: HTTP $code, want $3"
		return
	done
	echo "$1 $2: stayed busy"
}
each() { # FUNCTION: runs it for each number at once
	local j pids=()
	for j in $(seq 1 "$n"); do
		"$1" "$j" &
		pids+=($!)
	done
	wait "${pids[@]}" # Not a bare wait, which would also wait for the watcher, and that's waiting for this.
}
make_all() {
	req POST /v1/volumes 201 "{\"name\":\"c$1\",\"source\":{\"volume\":\"src\"}}"
	req POST /v1/snapshots 201 "{\"name\":\"s$1\",\"volume\":\"src\"}"
	req POST /v1/volumes 201 "{\"name\":\"v$1\",\"size_bytes\":8388608}"
	req PATCH "/v1/volumes/v$1" 200 '{"size_bytes":16777216}'
}
map_clone() { req POST "/v1/subsystems/$sub/namespaces" 201 "{\"nsid\":$1,\"volume\":\"c$1\"}"; }
unmap_clone() { req DELETE "/v1/subsystems/$sub/namespaces/$1" 204; }
remove_all() {
	req DELETE "/v1/volumes/c$1" 204
	req DELETE "/v1/snapshots/s$1" 204
	req DELETE "/v1/volumes/v$1" 204
}
same_name() {
	curl -s -m 120 -o /dev/null -w '%{http_code}\n' -X POST -d '{"name":"same","source":{"volume":"src"}}' "$API/v1/volumes"
}
# Looks for storage reported as a stray while the work is going on, which would be false.
watch_strays() {
	while [ -e /tmp/many.watch ]; do
		curl -s -m 120 "$API/v1/pools/many" | grep -o '[^"]*no volume refers[^"]*' >> /tmp/many.strays
		sleep 0.1
	done
}
case "$1" in
make)
	: > /tmp/many.busy; : > /tmp/many.strays; touch /tmp/many.watch
	watch_strays &
	w=$!
	each make_all
	rm -f /tmp/many.watch
	wait "$w"
	;;
map) each map_clone ;;
unmap) each unmap_clone ;;
remove)
	: > /tmp/many.strays; touch /tmp/many.watch
	watch_strays &
	w=$!
	each remove_all
	rm -f /tmp/many.watch
	wait "$w"
	;;
same)
	for j in 1 2 3 4 5 6 7 8; do same_name & done
	wait
	;;
esac
INNER
)
EOF
	out=$(t "bash /tmp/many.sh make" 2>&1)
	expect_eq "$n clones, $n snapshots, $n volumes and $n grows at once all succeed" "" "$out"
	expect_eq "no work in progress is reported as a stray" "" "$(t 'sort -u /tmp/many.strays | head -3')"
	echo "        (asked to retry $(t 'wc -l < /tmp/many.busy') times)"
	expect_eq "all the volumes are there" "$((2 * n + 1))" "$(t "curl -s $API/v1/volumes | jq '.items | length'")"
	expect_eq "and all the snapshots" "$n" "$(t "curl -s $API/v1/snapshots | jq '.items | length'")"
	expect_eq "every grow took effect" "$n" "$(t "curl -s $API/v1/volumes | jq '[.items[] | select(.name | startswith(\"v\")) | select(.size_bytes == 16777216)] | length'")"
	expect_eq "with a different identity each" "$((2 * n + 1))" "$(t "curl -s $API/v1/volumes | jq '[.items[].nguid] | unique | length'")"

	out=$(t "bash /tmp/many.sh same" | sort | uniq -c | awk '{print $2}' | tr '\n' ' ')
	case "$out" in
	*201*) ok "the same clone asked for 8 times at once is made ($out)" ;;
	*) bad "the same clone asked for 8 times at once is made" "$out" ;;
	esac
	expect_eq "exactly once" 1 "$(t "bash /tmp/many.sh same >/dev/null; curl -s $API/v1/volumes | jq '[.items[] | select(.name == \"same\")] | length'")"
	api "delete it" 204 DELETE /v1/volumes/same
	if [ "$kind" = reflink ]; then
		expect_eq "the pool holds exactly what the service lists" "$((3 * n + 1))" "$(t "sudo ls $pool_dir | grep -c img")"
	else
		expect_eq "the pool holds exactly what the service lists" "$((3 * n + 1))" "$(our_lvs)"
	fi

	api "subsystem for the clones" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	expect_eq "map every clone into it at once" "" "$(t "bash /tmp/many.sh map" 2>&1)"
	out=$(connect "$sub") && ok "host connects" || bad "host connects" "$out"
	for _ in $(seq 1 $((60 * SCALE))); do
		devs=$(i "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn)\" = $sub ] && ls -d \$s/nvme*n* | xargs -n1 basename; done | sed 's|^|/dev/|'")
		[ "$(echo "$devs" | grep -c nvme)" -ge "$n" ] && break
		sleep 0.25
	done
	expect_eq "the host sees all of them" "$n" "$(echo "$devs" | grep -c nvme)"
	sums=$(for d in $devs; do sum_of i "$d"; done | sort | uniq -c | awk '{print $1, $2}')
	expect_eq "and each holds the data of the volume it was cloned from" "$n $want" "$sums"
	disconnect "$sub"
	expect_eq "unmap them all at once" "" "$(t "bash /tmp/many.sh unmap" 2>&1)"
	api "remove that subsystem" 204 DELETE "/v1/subsystems/$sub"

	expect_eq "delete everything at once" "" "$(t "bash /tmp/many.sh remove" 2>&1)"
	expect_eq "nothing reported as a stray meanwhile" "" "$(t 'sort -u /tmp/many.strays | head -3')"
	api "unmap the source" 204 DELETE "/v1/subsystems/$src/namespaces/1"
	api "remove its subsystem" 204 DELETE "/v1/subsystems/$src"
	api "delete the source" 204 DELETE /v1/volumes/src
	if [ "$kind" = reflink ]; then
		expect_eq "the pool is empty" 0 "$(t "sudo ls $pool_dir | grep -c img")"
	else
		expect_eq "the pool is empty" 0 "$(our_lvs)"
	fi
	expect_eq "no change is left unfinished" 0 "$(t "sudo podman exec nvweaver-vm sh -c 'ls /var/lib/nvweaver/intents | wc -l'")"
	api "forget the pool" 204 DELETE /v1/pools/many
}

# Removing a host has to stop it there and then, even mid-write, so it can be fenced. What that does to other hosts
# matters as much. Host A is the other VM. Host B is the target VM's own kernel, with a volume shared with A, and one that isn't.
disconnect_tests() { # BACKEND
	local backend=$1 sub=$NQN:$1-fence other=$NQN:$1-fence-other b_host=$NQN:$1-host-b
	local dev_a dev_b dev_other out removed
	section "$backend: removing a host from a subsystem while it's writing"
	api "second host" 201 POST /v1/hosts "{\"nqn\":\"$b_host\"}"
	api "subsystem shared by both hosts" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\",\"$b_host\"],\"ports\":[1]}"
	api "with a namespace" 201 POST "/v1/subsystems/$sub/namespaces" "{\"file\":{\"name\":\"$1-fence.img\",\"size_bytes\":67108864}}"
	api "another subsystem on the same port, for the second host" 201 POST /v1/subsystems "{\"nqn\":\"$other\",\"allowed_hosts\":[\"$b_host\"],\"ports\":[1]}"
	api "with a namespace" 201 POST "/v1/subsystems/$other/namespaces" "{\"file\":{\"name\":\"$1-fence-other.img\",\"size_bytes\":67108864}}"
	out=$(connect "$sub") && ok "the first host connects" || bad "the first host connects" "$out"
	t "sudo modprobe nvme-tcp"
	for s in "$sub" "$other"; do
		out=$(t "sudo nvme connect -t tcp -a $TARGET_IP -s $PORT -n $s --hostnqn=$b_host >/dev/null" 2>&1) && ok "the second host connects" || bad "the second host connects" "$out"
	done
	dev_a=$(ini_dev "$sub")
	local find_dev='for _ in $(seq 1 40); do for s in /sys/class/nvme-subsystem/*; do [ "$(cat $s/subsysnqn 2>/dev/null)" = "$1" ] || continue; for n in $s/nvme*n*; do [ -b /dev/$(basename $n) ] && { echo /dev/$(basename $n); exit 0; }; done; done; sleep 0.25; done'
	dev_b=$(t "bash -c '$find_dev' _ $sub")
	dev_other=$(t "bash -c '$find_dev' _ $other")
	if [ -z "$dev_a" ] || [ -z "$dev_b" ] || [ -z "$dev_other" ]; then
		bad "the namespace appears on each host" "[$dev_a] [$dev_b] [$dev_other]"
		return
	fi
	# Each line is when a write started, how long it took, and how it ended.
	# nocreat, or once a host has lost its device, dd makes a file with its name in /dev, and appears to succeed.
	local writer='while :; do s=$(date +%s.%N); if dd if=/dev/zero of=$1 bs=4k count=1 oflag=direct,sync conv=nocreat status=none 2>/dev/null; then r=ok; else r=ERR; fi; e=$(date +%s.%N); echo "$s $(awk -v a="$s" -v b="$e" "BEGIN { printf \"%.3f\", b - a }") $r"; sleep 0.1; done'
	for pair in "i $dev_a /tmp/fence-a" "t $dev_b /tmp/fence-b" "t $dev_other /tmp/fence-other"; do
		set -- $pair
		$1 "cat > $3.sh <<'WRITER'
$writer
WRITER
sudo nohup setsid bash $3.sh $2 > $3.log 2>&1 < /dev/null &"
	done
	sleep 3
	expect_eq "all three are writing, with no errors" "0 0 0" "$(for f in i:a t:b t:other; do ${f%%:*} "grep -c ERR /tmp/fence-${f##*:}.log"; done | tr '\n' ' ' | sed 's/ $//')"

	removed=$(t 'date +%s.%N')
	api "remove the first host, while it's writing" 204 DELETE "/v1/subsystems/$sub/hosts/$HOST"
	sleep $((2 * SCALE))
	# Clocks differ between VMs, so A's writes are counted by its own clock.
	local a_since
	a_since=$(i 'date +%s.%N')
	# This write can't finish, so it isn't waited for. If it reached the disk, the other host would see it below.
	i "sudo nohup sh -c 'head -c 4096 /dev/urandom > /tmp/fence-mark; dd if=/tmp/fence-mark of=$dev_a bs=4k seek=4096 count=1 oflag=direct,sync conv=nocreat' >/dev/null 2>&1 < /dev/null &"
	sleep $((13 * SCALE))
	expect_eq "it has no write finish after that" 0 "$(i "awk -v s=$a_since '\$1 + \$2 >= s && \$3 == \"ok\"' /tmp/fence-a.log | wc -l")"
	expect_eq "and a write it tried didn't reach the disk" 0 "$(t "sudo dd if=$dev_b bs=4k skip=4096 count=1 iflag=direct status=none | tr -d '\\0' | wc -c")"
	# What the host's controller shows varies, as SPDK's hosts take until an I/O timeout to notice. The target's answer doesn't.
	if out=$(t "sudo nvme connect -t tcp -a $TARGET_IP -s $PORT -n $sub --hostnqn=$HOST 2>&1"); then
		bad "the target refuses that host's name, from anywhere" "$out"
		t "sudo nvme disconnect -n $sub >/dev/null 2>&1"
		bad "skipping the rest, as that also disconnected the other host"
		return
	else
		ok "the target refuses that host's name, from anywhere"
	fi

	expect_eq "the host sharing the subsystem has no write errors" 0 "$(t "grep -c ERR /tmp/fence-b.log")"
	expect_eq "and is connected" live "$(t "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn)\" = $sub ] && cat \$s/nvme[0-9]*/state; done | head -1")"
	expect_eq "and is writing again" 1 "$(t "awk -v s=\$(date +%s) '\$1 >= s - 2 && \$3 == \"ok\"' /tmp/fence-b.log | wc -l | awk '{print (\$1 > 0)}'")"
	echo "        (its longest stall was $(t "awk -v s=$removed '\$1 + \$2 >= s && \$2 > m { m = \$2 } END { printf \"%.2f\", m }' /tmp/fence-b.log") s)"
	expect_eq "the same host on another subsystem has no write errors" 0 "$(t "grep -c ERR /tmp/fence-other.log")"
	expect_eq "and no stall at all" 1 "$(t "awk -v s=$removed -v most=$((2 * SCALE)) '\$1 + \$2 >= s && \$2 > m { m = \$2 } END { print (m < most) }' /tmp/fence-other.log")"

	api "allow the first host again" 204 PUT "/v1/subsystems/$sub/hosts/$HOST"
	# A host told it isn't allowed may give up and remove its controller, and then has to connect again itself.
	if [ -z "$(i "cat /sys/class/nvme-subsystem/*/nvme[0-9]*/state 2>/dev/null | head -1")" ]; then
		echo "        (this host's controller was removed, so it connects again itself)"
		out=$(connect "$sub") && ok "the host can connect again" || bad "the host can connect again" "$out"
	fi
	dev_a=$(ini_dev "$sub")
	# Not waited for directly, as it may block until the host has recovered.
	# With sudo, as root made it. Left there, the previous backend's would pass this.
	i "sudo rm -f /tmp/fence-ok"
	expect_eq "(nothing left over to pass this check)" "" "$(i '[ -e /tmp/fence-ok ] && echo left')"
	i "sudo nohup sh -c 'dd if=/dev/zero of=$dev_a bs=4k seek=8192 count=1 oflag=direct,sync conv=nocreat && touch /tmp/fence-ok' >/dev/null 2>&1 < /dev/null &"
	for _ in $(seq 1 $((240 * SCALE))); do
		[ "$(i '[ -e /tmp/fence-ok ] && echo yes')" = yes ] && break
		sleep 0.5
	done
	expect_eq "and a write finishes" yes "$(i '[ -e /tmp/fence-ok ] && echo yes')"

	# They must really have stopped: device names get reused, and a later test's disk could be written to.
	for r in i t; do
		$r "sudo pkill -f '^bash /tmp/fence-'; sleep 0.5; sudo pkill -x dd; true"
		expect_eq "its writers have stopped" 0 "$($r "pgrep -fc '^bash /tmp/fence-'; true")"
	done
	disconnect "$sub"
	t "sudo nvme disconnect -n $sub >/dev/null 2>&1; sudo nvme disconnect -n $other >/dev/null 2>&1"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"
	api "remove the other" 204 DELETE "/v1/subsystems/$other"
	api "remove the second host" 204 DELETE "/v1/hosts/$b_host"
}

# The figure in the check has to match real SPDK: up to the limit has to work, and past it has to be refused clearly.
aio_limit_tests() {
	local sub=$NQN:aio have before
	section "spdk: running out of fs.aio-max-nr"
	before=$(t 'cat /proc/sys/fs/aio-max-nr')
	have=$(t "curl -s $API/v1/subsystems | jq '[.[].namespaces[]] | length'")
	api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"ports\":[1]}"
	t "sudo sysctl -q -w fs.aio-max-nr=$(((have + 2) * 128))"
	api "a namespace that fits" 201 POST "/v1/subsystems/$sub/namespaces" '{"nsid":1,"file":{"name":"aio1.img","size_bytes":1048576}}'
	expect_eq "and is served" true "$(t "jq .live_enabled /tmp/b")"
	api "the last that fits" 201 POST "/v1/subsystems/$sub/namespaces" '{"nsid":2,"file":{"name":"aio2.img","size_bytes":1048576}}'
	expect_eq "and is served" true "$(t "jq .live_enabled /tmp/b")"
	api "one too many is refused" 422 POST "/v1/subsystems/$sub/namespaces" '{"nsid":3,"file":{"name":"aio3.img","size_bytes":1048576}}'
	expect_has "saying what to do" "sysctl -w fs.aio-max-nr" "$(t 'cat /tmp/b')"
	t "sudo sysctl -q -w fs.aio-max-nr=$before"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"
}

# What's particular to LVM: its rules on sizes, what else is in the volume group, and crashing.
lvm_tests() { # BACKEND IMAGE
	local backend=$1 image=$2 sub=$NQN:$1-lvm dev out
	section "$backend, lvm-thin: what's particular to LVM"
	api "a thin pool that doesn't exist is refused" 422 POST /v1/pools "{\"name\":\"l\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"nosuch\"}"
	t "sudo lvcreate -q -y -L 8m -n not-a-pool $VG"
	api "nor can it adopt what isn't a thin pool" 422 POST /v1/pools "{\"name\":\"l\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"not-a-pool\"}"
	api "adopt the thin pool" 201 POST /v1/pools "{\"name\":\"l\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\",\"warning_percent\":99}"
	expect_eq "it reports real capacity" 6442450944 "$(t 'jq .capacity.total_bytes /tmp/b')"
	expect_eq "and metadata use" true "$(t 'jq ".capacity.metadata_used_percent > 0" /tmp/b')"
	api "a small volume" 201 POST /v1/volumes '{"name":"small","size_bytes":4096}'
	expect_eq "reports the size LVM really gave it" 4194304 "$(t 'jq .size_bytes /tmp/b')"
	expect_eq "which is what's on disk" 4194304 "$(t "sudo blockdev --getsize64 \$(jq -r .device_path /tmp/b)")"
	api "asking again with the original size finds it" 200 POST /v1/volumes '{"name":"small","size_bytes":4096}'
	api "the same name at another size is a conflict" 409 POST /v1/volumes '{"name":"small","size_bytes":8388608}'
	api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "map it" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"small"}'
	out=$(connect "$sub") && ok "host connects" || bad "host connects" "$out"
	dev=$(ini_dev "$sub")
	expect_eq "and sees that size" 4194304 "$(i "sudo blockdev --getsize64 $dev")"
	api "grow it while connected" 200 PATCH /v1/volumes/small '{"size_bytes":8388608}'
	sleep $((2 * SCALE))
	expect_eq "and the host sees the new size" 8388608 "$(i "sudo blockdev --getsize64 $dev")"
	api "a mapped volume can't be deleted" 409 DELETE /v1/volumes/small
	disconnect "$sub"
	api "unmap it" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"

	t "sudo lvcreate -q -y --thin -V 4m -n nvw-stray $VG/pool0; sudo lvcreate -q -y --thin -V 4m -n someone-elses $VG/pool0"
	api "pool status" 200 GET /v1/pools/l
	out=$(t 'cat /tmp/b')
	expect_has "reports a volume nothing refers to" nvw-stray "$out"
	expect_lacks "but ignores what isn't ours" someone-elses "$out"
	t "sudo lvremove -q -y $VG/nvw-stray"
	api "delete the volume" 204 DELETE /v1/volumes/small
	expect_eq "it's gone" 0 "$(our_lvs)"

	# Everything the service said it did must still be true after a crash, with nothing left over.
	local round in_progress=0 n
	t 'rm -f /tmp/kill.*'
	for round in 1 2 3; do
		t "(n=0; while :; do n=\$((n+1)); v=k$round-\$n
			[ \"\$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST $API/v1/volumes -d '{\"name\":\"'\$v'\",\"size_bytes\":4194304}')\" = 201 ] || break
			[ \"\$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST $API/v1/snapshots -d '{\"name\":\"s'\$v'\",\"volume\":\"'\$v'\"}')\" = 201 ] || break
			echo s\$v >> /tmp/kill.snapshots
			echo \$v >> /tmp/kill.volumes
		done; touch /tmp/kill.done$round) >/dev/null 2>&1 &"
		sleep $((3 * SCALE))
		t 'sudo podman kill -s KILL nvweaver-vm >/dev/null'
		for _ in $(seq 1 $((60 * SCALE))); do [ "$(t "[ -f /tmp/kill.done$round ] && echo yes")" = yes ] && break; sleep 0.5; done
		n=$(t "sudo ls \$(sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}')/intents | wc -l")
		in_progress=$((in_progress + n))
		start_service "$backend" "$image"
		wait_api || bad "service restarts after a crash" "$(t 'sudo podman logs nvweaver-vm 2>&1 | tail -5')"
	done
	echo "  (changes in progress at the moment of the crash: $in_progress)"
	out=$(t 'cat /tmp/kill.volumes /tmp/kill.snapshots 2>/dev/null | wc -l')
	if [ "$out" -gt 0 ]; then ok "the service was really doing work when killed ($out changes confirmed)"; else bad "the service was really doing work when killed"; fi
	expect_eq "after crashes, no intents are left" 0 "$(t "sudo ls \$(sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}')/intents | wc -l")"
	expect_eq "every volume it confirmed is there" "" "$(t "for v in \$(cat /tmp/kill.volumes 2>/dev/null); do [ \"\$(curl -s -o /dev/null -w '%{http_code}' $API/v1/volumes/\$v)\" = 200 ] || echo \$v; done")"
	expect_eq "every snapshot it confirmed is there" "" "$(t "for v in \$(cat /tmp/kill.snapshots 2>/dev/null); do [ \"\$(curl -s -o /dev/null -w '%{http_code}' $API/v1/snapshots/\$v)\" = 200 ] || echo \$v; done")"
	api "pool status after the crashes" 200 GET /v1/pools/l
	expect_eq "LVM holds exactly what the service knows of" "$(t 'jq ".volumes + .snapshots" /tmp/b')" "$(our_lvs)"
	expect_lacks "with nothing left that nothing refers to" "no volume refers" "$(t 'cat /tmp/b')"
	for n in $(t "curl -s '$API/v1/volumes?limit=1000' | jq -r '.items[].name'"); do t "curl -s -X DELETE $API/v1/volumes/$n"; done
	for n in $(t "curl -s '$API/v1/snapshots?limit=1000' | jq -r '.items[].name'"); do t "curl -s -X DELETE $API/v1/snapshots/$n"; done
	expect_eq "it all deletes" 0 "$(our_lvs)"
	api "forget the pool" 204 DELETE /v1/pools/l
	expect_eq "forgetting it changes nothing in LVM" "someone-elses" "$(t "sudo lvs --noheadings -o lv_name $VG | tr -d ' ' | grep -v -e pool0 -e not-a-pool | head -1")"
	t "sudo lvremove -q -y $VG/someone-elses $VG/not-a-pool"
}

# A thin pool can promise more than it has, so this really fills one.
full_pool_tests() { # BACKEND
	local sub=$NQN:$1-full dev out
	section "$1, lvm-thin: a thin pool that really fills up"
	t "sudo lvcreate -q -y --type thin-pool -L 128m -n small $VG"
	api "adopt a small thin pool" 201 POST /v1/pools "{\"name\":\"full\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"small\"}"
	api "a volume can promise more than the pool has" 201 POST /v1/volumes '{"name":"big","size_bytes":536870912}'
	api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "map it" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"big"}'
	out=$(connect "$sub" "-l 600") && ok "host connects" || bad "host connects" "$out"
	dev=$(ini_dev "$sub")
	i "sudo rm -f /tmp/full.done; head -c 256M /dev/urandom > /tmp/full; (sudo dd if=/tmp/full of=$dev bs=1M oflag=direct status=none; echo \$? > /tmp/full.done) </dev/null >/dev/null 2>&1 &"
	for _ in $(seq 1 $((120 * SCALE))); do
		[ "$(t "sudo lvs --noheadings -o lv_health_status $VG/small | tr -d ' '")" = out_of_data ] && break
		sleep 0.5
	done
	expect_eq "writing past its size fills the pool" out_of_data "$(t "sudo lvs --noheadings -o lv_health_status $VG/small | tr -d ' '")"
	api "the API still answers" 200 GET /v1/pools/full
	expect_has "and says what's happening" "out of space" "$(t 'cat /tmp/b')"
	expect_eq "and that it's full" 100 "$(t 'jq .capacity.used_percent /tmp/b')"
	api "a new volume is refused as out of room" 507 POST /v1/volumes '{"name":"more","size_bytes":4194304}'
	api "as is a snapshot" 507 POST /v1/snapshots '{"name":"more","volume":"big"}'
	expect_eq "and a refusal leaves no intent behind" 0 "$(t "sudo ls \$(sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}')/intents | wc -l")"
	for _ in $(seq 1 $((240 * SCALE))); do
		[ -n "$(i 'cat /tmp/full.done 2>/dev/null')" ] && break
		sleep 0.5
	done
	out=$(i 'cat /tmp/full.done 2>/dev/null')
	if [ -n "$out" ] && [ "$out" != 0 ]; then ok "the host's write fails, rather than silently losing data"; else bad "the host's write fails, rather than silently losing data" "dd said [$out]"; fi
	t "sudo lvextend -q -L +512m $VG/small" && ok "extend the pool" || bad "extend the pool"
	out=$(i "sudo dd if=/tmp/full of=$dev bs=1M oflag=direct status=none; echo \$?")
	expect_eq "the host can write again, with no reconnect" 0 "$out"
	expect_eq "and all of it reads back" "$(i "md5sum < /tmp/full | cut -d' ' -f1")" "$(i "sudo dd if=$dev bs=1M count=256 iflag=direct status=none | md5sum | cut -d' ' -f1")"
	api "the pool no longer warns" 200 GET /v1/pools/full
	expect_lacks "of being out of space" "out of space" "$(t 'cat /tmp/b')"
	disconnect "$sub"
	api "unmap it" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"
	api "delete the volume" 204 DELETE /v1/volumes/big
	api "forget the pool" 204 DELETE /v1/pools/full
	t "sudo lvremove -q -y $VG/small"
}

flushes_of() { t "awk '{print \$16}' /sys/block/$1/stat"; } # Flush requests a disk has completed.

# Everything in the target's RAM is lost at once, with no sync, while the host carries on. A file whose fsync
# finished before that must still be there, read back from the target rather than from the host's cache.
# QEMU's disks don't lose unflushed writes, as a real disk's cache can, so this also checks that a host's flush
# reaches the disk. Without that, a pass here would say nothing about a real power cut.
power_tests() { # BACKEND IMAGE POOL-TYPE
	local backend=$1 image=$2 kind=$3 sub=$NQN:$1-$3-power dev out disk before after idle acked
	section "$backend, $kind: losing power"
	if [ "$kind" = reflink ]; then
		t "sudo podman exec nvweaver-vm mkdir -p /var/lib/nvweaver/power-pool"
		api "adopt a pool" 201 POST /v1/pools '{"name":"power","type":"reflink","path":"/var/lib/nvweaver/power-pool"}'
		disk=$(t "lsblk -no PKNAME \$(df --output=source /var/lib/containers | tail -1 | sed 's/\[.*//')")
	else
		api "adopt a pool" 201 POST /v1/pools "{\"name\":\"power\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
		disk=$(t "basename \$(readlink -f $LVM_DISK)")
	fi
	api "a volume" 201 POST /v1/volumes '{"name":"pw","size_bytes":1073741824}'
	api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "map it" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"pw"}'
	out=$(connect "$sub" "-l 900") && ok "host connects" || bad "host connects" "$out"
	dev=$(ini_dev "$sub")
	i "sudo mkfs.xfs -q -f $dev && sudo mkdir -p /mnt/power && sudo mount $dev /mnt/power && sudo chmod 777 /mnt/power && rm -f /tmp/power.*"

	before=$(flushes_of "$disk")
	sleep $((2 * SCALE))
	after=$(flushes_of "$disk")
	idle=$((after - before))
	before=$after
	i 'cd /mnt/power; for n in $(seq 1 20); do dd if=/dev/urandom of=f$n bs=4k count=1 conv=fsync status=none; done'
	after=$(flushes_of "$disk")
	if [ $((after - before - idle)) -ge 10 ]; then
		ok "a host's fsync reaches the disk ($((after - before)) flushes for 20 fsyncs, $idle when idle)"
	else
		bad "a host's fsync reaches the disk" "$((after - before)) flushes for 20 fsyncs, $idle when idle"
	fi

	# The images and the rest of the system aren't what's being tested, and a freshly loaded image hasn't reached the disk.
	t 'sudo sync'
	# Each journal line is a file whose fsync has finished. It's on the host, so it survives the target.
	i "setsid nohup bash -c 'cd /mnt/power; n=0; while [ ! -f /tmp/power.stop ]; do n=\$((n+1)); dd if=/dev/urandom of=w\$n bs=64k count=16 conv=fsync status=none || break; sync . ; md5sum w\$n >> /tmp/power.journal; done; touch /tmp/power.done' </dev/null >/dev/null 2>&1 &"
	# Each line here is something the service said it did, which comes to this machine as it happens.
	acked=$(mktemp)
	t "n=0; while :; do n=\$((n+1)); [ \"\$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST $API/v1/volumes -d '{\"name\":\"p'\$n'\",\"size_bytes\":4194304}')\" = 201 ] || break; echo v p\$n; [ \"\$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST $API/v1/snapshots -d '{\"name\":\"ps'\$n'\",\"volume\":\"p'\$n'\"}')\" = 201 ] || break; echo s ps\$n; done" >"$acked" 2>/dev/null &
	local churn=$!
	for _ in $(seq 1 $((60 * SCALE))); do
		[ "$(i 'wc -l < /tmp/power.journal 2>/dev/null || echo 0')" -ge 8 ] && [ "$(wc -l <"$acked")" -ge 8 ] && break
		sleep 0.5
	done
	t 'sudo sh -c "echo 1 > /proc/sys/kernel/sysrq; echo b > /proc/sysrq-trigger"' 2>/dev/null &
	sleep $((3 * SCALE))
	local committed confirmed
	committed=$(i 'wc -l < /tmp/power.journal')
	kill "$churn" 2>/dev/null
	wait "$churn" 2>/dev/null
	confirmed=$(wc -l <"$acked")
	if [ "$committed" -gt 0 ] && [ "$confirmed" -gt 0 ]; then
		ok "both were really at work when the power went ($committed files, $confirmed changes)"
	else
		bad "both were really at work when the power went" "$committed files, $confirmed changes"
	fi
	sleep $((5 * SCALE))
	for _ in $(seq 1 $((90 * SCALE))); do t true 2>/dev/null && break; sleep 2; done
	t true 2>/dev/null && ok "the target comes back" || bad "the target comes back"
	expect_eq "it really rebooted" 0 "$(t "awk '{exit \$1 > 600}' /proc/uptime; echo \$?")"
	start_service "$backend" "$image"
	if wait_api; then
		ok "the service restores its configuration"
	else
		bad "the service restores its configuration" "$(t 'sudo podman logs nvweaver-vm 2>&1 | tail -8')"
		return 1 # What follows would only repeat this failure.
	fi
	expect_eq "no intents are left" 0 "$(t "sudo ls \$(sudo podman volume inspect nvweaver-vm-data --format '{{.Mountpoint}}')/intents | wc -l")"
	out=$(while read -r type name; do
		case "$type" in
		v) code=$(t "curl -s -o /dev/null -w '%{http_code}' $API/v1/volumes/$name") ;;
		s) code=$(t "curl -s -o /dev/null -w '%{http_code}' $API/v1/snapshots/$name") ;;
		*) continue ;;
		esac
		[ "$code" = 200 ] || echo "$name:$code"
	done <"$acked")
	rm -f "$acked"
	expect_eq "everything the service confirmed is there ($confirmed)" "" "$out"
	api "pool status" 200 GET /v1/pools/power
	expect_lacks "with nothing left that nothing refers to" "no volume refers" "$(t 'cat /tmp/b')"
	if [ "$kind" = lvm-thin ]; then
		expect_eq "LVM holds exactly what the service knows of" "$(t 'jq ".volumes + .snapshots" /tmp/b')" "$(our_lvs)"
	fi

	for _ in $(seq 1 $((120 * SCALE))); do
		[ "$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$sub' ] && cat \$c/state; done")" = live ] && break
		sleep 1
	done
	expect_eq "the host reconnects by itself" live "$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$sub' ] && cat \$c/state; done")"
	i 'touch /tmp/power.stop'
	for _ in $(seq 1 $((120 * SCALE))); do [ "$(i '[ -f /tmp/power.done ] && echo yes')" = yes ] && break; sleep 0.5; done
	expect_eq "and its writer carries on to the end" yes "$(i '[ -f /tmp/power.done ] && echo yes')"
	# Unmounting drops the host's cache, so these reads have to come from the target.
	i 'sudo umount /mnt/power' && ok "unmount it" || bad "unmount it"
	if out=$(i "sudo xfs_repair -n $dev" 2>&1); then ok "the filesystem checks clean"; else bad "the filesystem checks clean" "$(echo "$out" | tail -5)"; fi
	i "sudo mount $dev /mnt/power" && ok "mount it again" || bad "mount it again"
	expect_eq "every file whose fsync finished before the power went is intact ($committed)" rc=0 \
		"$(i "cd /mnt/power && head -n $committed /tmp/power.journal | md5sum -c --quiet >/dev/null 2>&1; echo rc=\$?")"
	i 'sudo umount /mnt/power'
	disconnect "$sub"

	api "unmap it" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"
	for out in $(t "curl -s '$API/v1/volumes?limit=1000' | jq -r '.items[].name'"); do t "curl -s -X DELETE $API/v1/volumes/$out"; done
	for out in $(t "curl -s '$API/v1/snapshots?limit=1000' | jq -r '.items[].name'"); do t "curl -s -X DELETE $API/v1/snapshots/$out"; done
	api "forget the pool" 204 DELETE /v1/pools/power
	if [ "$kind" = lvm-thin ]; then expect_eq "nothing is left in LVM" 0 "$(our_lvs)"; fi
}

# After a reboot nothing is active but what LVM activates, and the target has lost its configuration.
lvm_reboot_tests() { # BACKEND IMAGE
	local sub=$NQN:$1-lvm-reboot dev sum out
	section "$1, lvm-thin: a real reboot of the target"
	api "adopt the thin pool" 201 POST /v1/pools "{\"name\":\"r\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
	api "a volume" 201 POST /v1/volumes '{"name":"rv","size_bytes":67108864}'
	api "subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$sub\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "map it" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"rv"}'
	out=$(connect "$sub") && ok "host connects" || bad "host connects" "$out"
	dev=$(ini_dev "$sub")
	sum=$(i "md5sum < /tmp/p | cut -d' ' -f1")
	i "sudo dd if=/tmp/p of=$dev bs=1M oflag=direct status=none"
	api "snapshot it" 201 POST /v1/snapshots '{"name":"rs","volume":"rv"}'
	disconnect "$sub"
	reboot_target && ok "VM reboots" || bad "VM reboots"
	start_service "$1" "$2"
	wait_api && ok "service restores its configuration" || bad "service restores its configuration" "$(t 'sudo podman logs nvweaver-vm 2>&1 | tail -8')"
	out=$(connect "$sub") && ok "host connects again" || bad "host connects again" "$out"
	dev=$(ini_dev "$sub")
	expect_eq "the volume's data survived" "$sum" "$(sum_of i "$dev")"
	disconnect "$sub"
	api "the snapshot is still there" 200 GET /v1/snapshots/rs
	expect_eq "and still inactive" "" "$(t "sudo lvs --noheadings -o lv_active $VG/\$(jq -r .internal_name /tmp/b) | tr -d ' '")"
	api "clone it after the reboot" 201 POST /v1/volumes '{"name":"rc","source":{"snapshot":"rs"}}'
	api "unmap the volume" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "map the clone" 201 POST "/v1/subsystems/$sub/namespaces" '{"volume":"rc"}'
	out=$(connect "$sub") && ok "host connects to the clone" || bad "host connects to the clone" "$out"
	dev=$(ini_dev "$sub")
	expect_eq "and it has the snapshot's data" "$sum" "$(sum_of i "$dev")"
	disconnect "$sub"
	api "unmap the clone" 204 DELETE "/v1/subsystems/$sub/namespaces/1"
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$sub"
	api "delete the volume" 204 DELETE /v1/volumes/rv
	api "delete the clone" 204 DELETE /v1/volumes/rc
	api "delete the snapshot" 204 DELETE /v1/snapshots/rs
	api "forget the pool" 204 DELETE /v1/pools/r
	expect_eq "nothing is left in LVM" 0 "$(our_lvs)"
}

backend_cleanup() {
	t 'sudo podman stop -t 25 nvweaver-vm >/dev/null; sudo podman rm -f nvweaver-vm >/dev/null; sudo podman volume rm -f nvweaver-vm-data >/dev/null'
}

# POWER_ONLY="BACKEND POOL-TYPE ROUNDS" repeats just the power-loss test, as a rare failure needs many rounds.
if [ -n "${POWER_ONLY:-}" ]; then
	read -r only_backend only_kind only_rounds <<<"$POWER_ONLY"
	only_image=localhost/nvweaver:dev
	[ "$only_backend" = spdk ] && only_image=localhost/nvweaver:spdk
	start_service "$only_backend" "$only_image"
	wait_api || exit 1
	api "port on the link address" 201 POST /v1/ports "{\"id\":1,\"address\":\"$TARGET_IP\",\"service\":$PORT}"
	api "host" 201 POST /v1/hosts "{\"nqn\":\"$HOST\"}"
	for _ in $(seq 1 "$only_rounds"); do
		power_tests "$only_backend" "$only_image" "$only_kind" || break
	done
	[ "$fail" = 0 ]
	exit
fi

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
lvm_reboot_tests nvmet localhost/nvweaver:dev
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
LSUB=$NQN:service-lvm
api "adopt the thin pool, with no container involved" 201 POST /v1/pools "{\"name\":\"svc\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
api "a volume" 201 POST /v1/volumes '{"name":"sv","size_bytes":67108864}'
api "a snapshot" 201 POST /v1/snapshots '{"name":"ss","volume":"sv"}'
api "a clone" 201 POST /v1/volumes '{"name":"sc","source":{"snapshot":"ss"}}'
api "subsystem for it" 201 POST /v1/subsystems "{\"nqn\":\"$LSUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
api "map the clone" 201 POST "/v1/subsystems/$LSUB/namespaces" '{"volume":"sc"}'
out=$(connect "$LSUB" "-l 300 -c 5") && ok "host connects to it" || bad "host connects to it" "$out"
LDEV=$(ini_dev "$LSUB")
i "sudo dd if=/tmp/p of=$LDEV bs=1M oflag=direct status=none"
expect_eq "data round trip on an LVM volume" "$SUM" "$(sum_of i "$LDEV")"
# LVM carries on without these, so nothing else here would notice that the sandbox had stopped them.
expect_eq "the sandbox lets LVM back up its metadata" yes "$(t "sudo grep -q \$(curl -s $API/v1/volumes/sc | jq -r .internal_name) /etc/lvm/backup/$VG && echo yes || echo no")"
expect_lacks "and lock itself in memory" "mlock failed" "$(t "curl -s -X POST $API/v1/volumes -d '{\"name\":\"sm\",\"size_bytes\":4194304}' >/dev/null; sudo journalctl -u nvweaver --no-pager -n 200")"
api "delete that volume" 204 DELETE /v1/volumes/sm

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
for _ in $(seq 1 $((60 * SCALE))); do
	LIVE=$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$SSUB' ] && cat \$c/state; done")
	[ "$LIVE" = live ] && break
	sleep 2
done
expect_eq "host reconnected by itself" live "$LIVE"
SDEV=$(ini_dev "$SSUB")
expect_eq "data survived the reboot" "$SUM" "$(sum_of i "$SDEV")"
LIVE=
for _ in $(seq 1 $((60 * SCALE))); do
	LIVE=$(i "for c in /sys/class/nvme/*; do [ \"\$(cat \$c/subsysnqn 2>/dev/null)\" = '$LSUB' ] && cat \$c/state; done")
	[ "$LIVE" = live ] && break
	sleep 2
done
expect_eq "host reconnected to the LVM volume" live "$LIVE"
LDEV=$(ini_dev "$LSUB")
expect_eq "its data survived the reboot" "$SUM" "$(sum_of i "$LDEV")"
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
for _ in $(seq 1 $((60 * SCALE))); do
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
