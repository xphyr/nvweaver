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
	lvm_tests "$backend" "$image"
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
