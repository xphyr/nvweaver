#!/usr/bin/env bash
# Upgrading from the previous release, on each backend: its published image makes one of every kind of object, a host writes
# to the volumes, then this version's image starts on the same data. Everything must still be there and served, the data the
# same, and what's new must work on it. Run before each release, as docs/releasing.md says.
#
# Uses the lab VMs (lab.sh), started and deleted here. Build this version's images first ("make images"). PREV is the
# previous release (default 0.5.0), whose images are pulled from ghcr.io. KEEP_VMS=1 leaves the VMs up.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
LAB=$HERE/lab.sh
PREV=${PREV:-0.5.0}
API=http://127.0.0.1:8420
TARGET_IP=10.99.0.1
SUB=nqn.2026-11.net.xphyr.upgrade:s1
LVM_DISK=/dev/disk/by-id/virtio-NVWEAVERLVM
VG=nvwup
pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$2], got [$3]"; fi; }
t() { "$LAB" ssh tgt "$1" </dev/null; }
i() { "$LAB" ssh ini "$1" </dev/null; }
api() { # NAME WANT METHOD PATH [JSON]
	local code
	code=$(t "curl -s -m 60 -o /tmp/b -w '%{http_code}' -X $3 ${5:+-d '$5'} $API$4")
	if [ "$code" = "$2" ]; then ok "$1"; else bad "$1" "HTTP $code, want $2: $(t 'head -c 300 /tmp/b')"; fi
}
start() { # BACKEND IMAGE
	t "sudo podman rm -f nvweaver-up >/dev/null 2>&1; sudo podman run -d --pull=never --name nvweaver-up --privileged --network host --pid=host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-up-data:/var/lib/nvweaver -e NVWEAVER_BACKEND=$1 $2 >/dev/null"
	for _ in $(seq 1 120); do
		[ "$(t "curl -s -m 2 -o /dev/null -w '%{http_code}' $API/healthz")" = 200 ] && return 0
		[ "$(t "sudo podman inspect -f '{{.State.Status}}' nvweaver-up")" = running ] || break
		sleep 1
	done
	bad "$2 starts" "$(t 'sudo podman logs nvweaver-up 2>&1 | tail -5')"
	return 1
}
# What must survive an upgrade, of everything nvweaver keeps: names, identities, paths, sizes and links, but not states,
# usage or warnings, which are read live.
snapshot_state() {
	t "for p in ports hosts subsystems pools volumes snapshots; do curl -s $API/v1/\$p; echo; done | jq -cS '
		if type == \"array\" then map(del(.state, .capacity, .warnings, .connections, .has_dhchap_key, .has_dhchap_ctrl_key))
		elif .items then .items | map(del(.state, .capacity, .warnings, .usage, .used_bytes))
		else . end'"
}
# The host's device for a volume, by its NGUID.
dev_of() {
	local nguid
	nguid=$(t "curl -s $API/v1/volumes/$1 | jq -r .nguid | tr -d -")
	i "for b in /sys/block/nvme*n*; do case \$b in *c[0-9]*n*) continue ;; esac; [ \"\$(tr -d - < \$b/nguid 2>/dev/null)\" = $nguid ] && echo /dev/\${b##*/}; done"
}

cleanup() {
	i "sudo nvme disconnect-all >/dev/null 2>&1" 2>/dev/null
	t "sudo podman rm -f nvweaver-up >/dev/null 2>&1; sudo podman volume rm nvweaver-up-data >/dev/null 2>&1; sudo vgremove -q -y -f $VG >/dev/null 2>&1; sudo wipefs -q -a $LVM_DISK" 2>/dev/null
}
finish() {
	cleanup
	[ -n "${KEEP_VMS:-}" ] || "$LAB" down >/dev/null 2>&1
}

"$LAB" down >/dev/null 2>&1
"$LAB" up >/dev/null || { echo "VMs did not start"; exit 1; }
trap finish EXIT
for image in "ghcr.io/xphyr/nvweaver:$PREV" "ghcr.io/xphyr/nvweaver:$PREV-spdk"; do
	podman image exists "$image" || podman pull -q "$image" >/dev/null || { echo "cannot pull $image"; exit 1; }
	"$LAB" load tgt "$image" >/dev/null || { echo "cannot load $image"; exit 1; }
done
for image in localhost/nvweaver:dev localhost/nvweaver:spdk; do
	"$LAB" load tgt "$image" >/dev/null || { echo "cannot load $image: build it first"; exit 1; }
done
HOST=$(i 'cat /etc/nvme/hostnqn')
i 'sudo modprobe nvme-tcp'

for backend in nvmet spdk; do
	old=ghcr.io/xphyr/nvweaver:$PREV new=localhost/nvweaver:dev
	[ "$backend" = spdk ] && old=ghcr.io/xphyr/nvweaver:$PREV-spdk new=localhost/nvweaver:spdk
	printf '\n%s: from %s to this version\n' "$backend" "$PREV"
	cleanup
	t "sudo pvcreate -q -y $LVM_DISK && sudo vgcreate -q $VG $LVM_DISK && sudo lvcreate -q -y --type thin-pool -L 4G -n pool0 $VG" >/dev/null ||
		{ bad "an LVM thin pool for the test"; continue; }
	start "$backend" "$old" || continue
	expect_eq "$PREV says it's $PREV" "$PREV" "$(t "curl -s $API/v1/system | jq -r .version")"
	t "sudo podman exec nvweaver-up mkdir -p /var/lib/nvweaver/pool"
	api "a port" 201 POST /v1/ports "{\"id\":1,\"address\":\"$TARGET_IP\",\"service\":4420}"
	api "a host" 201 POST /v1/hosts "{\"nqn\":\"$HOST\"}"
	api "a subsystem" 201 POST /v1/subsystems "{\"nqn\":\"$SUB\",\"allowed_hosts\":[\"$HOST\"],\"ports\":[1]}"
	api "a reflink pool" 201 POST /v1/pools '{"name":"r","type":"reflink","path":"/var/lib/nvweaver/pool"}'
	api "an LVM pool" 201 POST /v1/pools "{\"name\":\"l\",\"type\":\"lvm-thin\",\"volume_group\":\"$VG\",\"thin_pool\":\"pool0\"}"
	api "a reflink volume" 201 POST /v1/volumes '{"name":"rv","pool":"r","size_bytes":67108864,"labels":{"app":"up"}}'
	api "an LVM volume" 201 POST /v1/volumes '{"name":"lv","pool":"l","size_bytes":67108864}'
	api "a snapshot of each" 201 POST /v1/snapshots '{"name":"rs","volume":"rv"}'
	api "and the other" 201 POST /v1/snapshots '{"name":"ls","volume":"lv"}'
	api "a clone" 201 POST /v1/volumes '{"name":"rc","source":{"snapshot":"rs"}}'
	api "a file namespace" 201 POST "/v1/subsystems/$SUB/namespaces" '{"nsid":1,"file":{"name":"f.img","size_bytes":16777216}}'
	api "both volumes exported" 201 POST "/v1/subsystems/$SUB/namespaces" '{"nsid":2,"volume":"rv"}'
	api "in the subsystem" 201 POST "/v1/subsystems/$SUB/namespaces" '{"nsid":3,"volume":"lv"}'
	i "sudo nvme connect -t tcp -a $TARGET_IP -s 4420 -n $SUB -l -1 >/dev/null" && ok "the host connects" || bad "the host connects"
	sleep 2
	dr=$(dev_of rv) dl=$(dev_of lv)
	i "head -c 16M /dev/urandom > /tmp/up.pat; sudo dd if=/tmp/up.pat of=$dr bs=1M oflag=direct status=none; sudo dd if=/tmp/up.pat of=$dl bs=1M oflag=direct status=none"
	want=$(i "md5sum < /tmp/up.pat | cut -c1-32")
	before=$(snapshot_state)
	# Not just something: an error both times would compare equal too.
	recorded=yes
	for name in rv lv rc rs ls; do
		grep -qF "\"name\":\"$name\"" <<<"$before" || recorded=no
	done
	grep -qF "\"nqn\":\"$SUB\"" <<<"$before" || recorded=no
	expect_eq "what $PREV has, recorded: every volume, snapshot and the subsystem" yes "$recorded"

	t "sudo podman stop -t 30 nvweaver-up >/dev/null"
	start "$backend" "$new" || continue
	expect_eq "this version starts on $PREV's data" 0 "$(t "sudo podman logs nvweaver-up 2>&1 | grep -c 'level=ERROR'")"
	after=$(snapshot_state)
	if [ "$before" = "$after" ]; then
		ok "every port, host, subsystem, pool, volume and snapshot is as $PREV left it"
	else
		bad "every object is as $PREV left it" "$(diff <(echo "$before" | tr ',' '\n') <(echo "$after" | tr ',' '\n') | head -10)"
	fi
	expect_eq "with no warnings" "" "$(t "curl -s $API/v1/system | jq -r '.warnings | join(\" \")'")"
	for v in rv lv; do
		got=
		for _ in $(seq 1 30); do
			d=$(dev_of $v)
			[ -n "$d" ] && got=$(i "sudo dd if=$d bs=1M count=16 iflag=direct status=none | md5sum | cut -c1-32") && [ "$got" = "$want" ] && break
			sleep 2
		done
		expect_eq "the host reads $v's data back the same, through the upgrade" "$want" "$got"
	done
	# What's new, on the old data.
	api "a host gets an owner and labels" 200 PATCH "/v1/hosts/$HOST" '{"owner":"lab","labels":{"csi-node":"n1"}}'
	api "a snapshot group of both pools' volumes, while exported" 201 POST /v1/snapshot-groups '{"name":"g","volumes":["rv","lv"]}'
	api "and it goes" 204 DELETE /v1/snapshot-groups/g

	i "sudo nvme disconnect -n $SUB >/dev/null 2>&1"
	api "unexport the file, and delete it" 204 DELETE "/v1/subsystems/$SUB/namespaces/1?delete_file=true"
	for n in 2 3; do api "unexport volume $n" 204 DELETE "/v1/subsystems/$SUB/namespaces/$n"; done
	for s in rs ls; do api "delete snapshot $s" 204 DELETE "/v1/snapshots/$s"; done
	for v in rc rv lv; do api "delete volume $v" 204 DELETE "/v1/volumes/$v"; done
	api "remove the subsystem" 204 DELETE "/v1/subsystems/$SUB"
	expect_eq "LVM has nothing of nvweaver's left" 0 "$(t "sudo lvs --noheadings -o lv_name $VG | grep -c nvw-")"
done
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
