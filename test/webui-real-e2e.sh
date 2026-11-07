#!/usr/bin/env bash
# Drives the web interface in a real browser against both real targets, in VMs: a real LVM pool, over real HTTPS, signing in with a password, and a real
# host connecting with the command the page shows. test/webui-e2e.sh covers the interface in depth against a fake target, which this doesn't repeat.
# Needs no root, as test/vm/vm-e2e.sh doesn't, and the images built. Local Podman images are copied into the VMs.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
LAB=$ROOT/test/vm/lab.sh
ENGINE=${ENGINE:-podman}
PLAYWRIGHT_VERSION=1.50.0
BROWSER_IMAGE=mcr.microsoft.com/playwright:v$PLAYWRIGHT_VERSION-noble
CONTAINER=nvweaver-webui-real
TUNNEL=${TUNNEL:-18440}
USERNAME="admin"
PASSWORD='a test password'
VG=nvwlab
LVM_DISK=/dev/disk/by-id/virtio-NVWEAVERLVM
TARGET_IP=10.99.0.1
KEEP=${1:-} # a directory for screenshots of any step that fails
WORK=$(mktemp -d)
TUNNEL_PID=
pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
t() { "$LAB" ssh tgt "$1" </dev/null; }
i() { "$LAB" ssh ini "$1" </dev/null; }
cleanup() {
	[ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null
	$ENGINE rm -f "$CONTAINER" >/dev/null 2>&1
	[ -n "$KEEP" ] && cp "$WORK"/failed-*.png "$KEEP/" 2>/dev/null
	rm -rf "$WORK"
	[ -n "${KEEP_VMS:-}" ] || "$LAB" down >/dev/null 2>&1
}
trap cleanup EXIT

# One step in the browser, adding its checks to the totals, and leaving its KEY=VALUE lines in $WORK/out.
step() { # NAME ARGS...
	local name=$1
	shift
	$ENGINE exec "$CONTAINER" node /work/real.mjs "$name" "https://127.0.0.1:$TUNNEL" "$@" >"$WORK/out" 2>&1
	grep -E '^  (ok|FAIL)|^        ' "$WORK/out"
	pass=$((pass + $(grep -c '^  ok' "$WORK/out")))
	fail=$((fail + $(grep -c '^  FAIL' "$WORK/out")))
	grep -qE '^  (ok|FAIL)' "$WORK/out" || { bad "step $name ran" "$(tail -5 "$WORK/out" | cut -c1-200)"; }
}
value() { sed -n "s/^$1=//p" "$WORK/out"; }

# The namespace with this ID in a subsystem, as the host sees it.
ns_dev() { # NQN NSID
	i "for _ in \$(seq 1 40); do for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = '$1' ] || continue; for n in \$s/nvme*n*; do [ \"\$(cat \$n/nsid 2>/dev/null)\" = '$2' ] && { echo /dev/\$(basename \$n); exit 0; }; done; done; sleep 0.5; done"
}

"$LAB" down >/dev/null 2>&1
"$LAB" up >/dev/null || exit 1
for image in dev spdk; do "$LAB" load tgt "localhost/nvweaver:$image" >/dev/null || exit 1; done
i 'sudo modprobe nvme-tcp'
HOST=$(i 'cat /etc/nvme/hostnqn')
t "sudo pvcreate -q -y --metadatasize 16m $LVM_DISK && sudo vgcreate -q $VG $LVM_DISK && sudo lvcreate -q -y --type thin-pool -L 6G --poolmetadatasize 256m -n pool0 $VG" >/dev/null 2>&1 || bad "make a thin pool"
t "sudo mkdir -p /etc/nvweaver/tls; cd /etc/nvweaver/tls; sudo openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=nvweaver-real -addext subjectAltName=IP:127.0.0.1 -keyout tls.key -out tls.crt 2>/dev/null"
LINE=$(printf '%s\n' "$PASSWORD" | "$LAB" ssh tgt 'sudo podman run --rm -i --pull=never --network none --entrypoint /usr/local/bin/nvweaver localhost/nvweaver:dev passwd '"$USERNAME")
case "$LINE" in "$USERNAME:\$2"*) ok "nvweaver passwd makes the users file" ;; *) bad "nvweaver passwd makes the users file" "$LINE" ;; esac
printf '%s\n' "$LINE" | "$LAB" ssh tgt 'sudo sh -c "umask 077; cat > /etc/nvweaver/tls/users"'

# The browser reaches the target through a tunnel, as only the initiator is on the VMs' private link.
"$LAB" forward tgt "$TUNNEL" 8420 </dev/null &
TUNNEL_PID=$!
cp "$ROOT/test/webui/real.mjs" "$WORK/"
chmod 777 "$WORK"
$ENGINE rm -f "$CONTAINER" >/dev/null 2>&1
$ENGINE run -d --name "$CONTAINER" --network host -v "$WORK:/work:Z" -w /work "$BROWSER_IMAGE" sleep infinity >/dev/null || exit 1
$ENGINE exec "$CONTAINER" sh -c "npm install --silent --no-audit --no-fund --prefix /work playwright@$PLAYWRIGHT_VERSION >/dev/null 2>&1" || { bad "install Playwright"; exit 1; }

for backend in nvmet spdk; do
	image=localhost/nvweaver:dev
	[ "$backend" = spdk ] && image=localhost/nvweaver:spdk
	printf '\n%s: the interface, on the real target\n' "$backend"
	rm -f "$WORK/state.json"
	t "sudo podman rm -f nvweaver-real >/dev/null 2>&1; sudo podman volume rm -f nvweaver-real-data >/dev/null 2>&1; sudo podman run -d --pull=never --name nvweaver-real --privileged --network host --pid=host \
		-v /lib/modules:/lib/modules:ro -v /dev:/dev -v nvweaver-real-data:/var/lib/nvweaver -v /etc/nvweaver/tls:/etc/nvweaver/tls:ro,Z \
		-e NVWEAVER_BACKEND=$backend -e NVWEAVER_USERS_FILE=/etc/nvweaver/tls/users -e NVWEAVER_TLS_CERT=/etc/nvweaver/tls/tls.crt -e NVWEAVER_TLS_KEY=/etc/nvweaver/tls/tls.key $image >/dev/null"
	for _ in $(seq 1 120); do
		[ "$(curl -sk -m 2 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$TUNNEL/healthz")" = 200 ] && break
		sleep 0.5
	done
	[ "$(curl -sk -m 2 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$TUNNEL/healthz")" = 200 ] && ok "the service answers over HTTPS, through the tunnel" || { bad "the service answers over HTTPS" "$(t 'sudo podman logs --tail 8 nvweaver-real' 2>&1 | cut -c1-200)"; continue; }
	[ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$TUNNEL/healthz")" = 400 ] && ok "plain HTTP is refused" || bad "plain HTTP is refused"

	step signin "$USERNAME" "$PASSWORD" "$backend"
	step setup "$TARGET_IP" "$HOST"
	step export "$HOST"
	CONNECT=$(value CONNECT)
	NQN=$(value NQN)

	out=$(i "sudo $CONNECT 2>&1") && ok "the host connects, using the command the page showed" || { bad "the host connects, using the command the page showed" "$CONNECT: $out"; continue; }
	dev=$(ns_dev "$NQN" 1)
	[ -n "$dev" ] && ok "and sees the volume" || { bad "and sees the volume"; continue; }
	expect=$(i "sudo dd if=/dev/urandom of=/tmp/real bs=1M count=16 status=none; sudo md5sum </tmp/real | cut -d' ' -f1")
	i "sudo dd if=/tmp/real of=$dev bs=1M oflag=direct conv=nocreat,notrunc status=none" && ok "it writes real data" || bad "it writes real data"
	got=$(i "sudo dd if=$dev bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")
	[ "$got" = "$expect" ] && ok "which reads back" || bad "which reads back" "$got, not $expect"
	onDisk=$(t "sudo dd if=\$(sudo lvs --noheadings -o lv_path $VG | tr -d ' ' | grep nvw- | head -1) bs=1M count=16 status=none | md5sum | cut -d' ' -f1")
	[ "$onDisk" = "$expect" ] && ok "and is really in the LVM volume" || bad "and is really in the LVM volume" "$onDisk, not $expect"
	step usage
	step connected "10.99.0.2" "$HOST"

	step clone
	cdev=$(ns_dev "$NQN" 2)
	[ -n "$cdev" ] && ok "the host sees the clone, with no reconnect" || bad "the host sees the clone, with no reconnect"
	got=$(i "sudo dd if=$cdev bs=1M count=16 iflag=direct status=none | md5sum | cut -d' ' -f1")
	[ "$got" = "$expect" ] && ok "with the snapshot's data" || bad "with the snapshot's data" "$got, not $expect"

	# So that finding none at the end means something.
	lvs_now() { t "sudo lvs --noheadings -o lv_name $VG | tr -d ' ' | grep -c '^nvw-' || true"; }
	before=$(lvs_now)
	[ "$before" = 3 ] && ok "LVM has the volume, its snapshot and its clone" || bad "LVM has the volume, its snapshot and its clone" "$before"

	step deny "$HOST"
	if i "sudo timeout -s KILL 12 dd if=/tmp/real of=$dev bs=1M count=1 seek=100 oflag=direct conv=nocreat,notrunc status=none" 2>/dev/null; then
		bad "removing it in the interface really cuts the host off" "a write still finished"
	else
		ok "removing it in the interface really cuts the host off"
	fi
	i "sudo nvme disconnect-all >/dev/null 2>&1; sudo pkill -9 -x dd; true"
	step disconnected

	step cleanup "$HOST"
	left=$(lvs_now)
	[ "$left" = 0 ] && ok "nothing is left in LVM" || bad "nothing is left in LVM" "$left left"
	t "sudo podman stop -t 25 nvweaver-real >/dev/null; sudo podman rm -f nvweaver-real >/dev/null; sudo podman volume rm -f nvweaver-real-data >/dev/null"
done

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
