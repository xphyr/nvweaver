#!/usr/bin/env bash
# A disposable Kubernetes lab for the CSI driver: k3s on three VMs, and nvweaver on a fourth,
# run with QEMU as an ordinary user, from the base image that lab.sh builds. Installing is fine
# inside these VMs, and nothing is installed on the machine running them.
#
#   nvweaver  10.98.0.1   the target: an emulated NVMe disk and a virtio disk for LVM
#   server    10.98.0.10  k3s server, which runs pods too
#   agent1    10.98.0.11  k3s agent
#   agent2    10.98.0.12  k3s agent
#
# The VMs share a private link through k3s-switch.py, and reach the internet, for images,
# through QEMU's user network. It has its own directory and ports, so it can run beside
# lab.sh's VMs, if there's memory for both.
#
# Commands:
#   up                  boot fresh VMs, install k3s and wait for every node to be ready
#   down                stop the VMs and delete their disks
#   status              show what is running, and the nodes
#   check               prove the lab works: a pod on every node, and NVMe/TCP from every node
#   ssh VM [cmd]        run a command in a VM (nvweaver, server, agent1, agent2)
#   kubectl [args]      run kubectl on the server, with stdin passed through
#   load VM|nodes IMAGE copy a local Podman image into a VM, or into every k3s node
#   sync VM             copy this source tree to ~/nvweaver in a VM
#
# Settings: K3S_VERSION (see below), LAB_DIR (~/nvweaver-vms), K3S_CPUS (2), K3S_MEM_MB (2048 for agents),
# K3S_SERVER_MEM_MB (3072), K3S_NVW_MEM_MB (3072), IMAGE for check (localhost/nvweaver:dev).
# LAB_ARCH=aarch64 runs Arm VMs instead, from lab.sh's Arm base image; emulated on x86, slowly.
set -u

# K3S_VERSION picks another Kubernetes, from the checksums pinned here: the oldest is the oldest the driver supports.
K3S_VERSION=${K3S_VERSION:-v1.37.2+k3s1}
case "$K3S_VERSION" in
v1.37.2+k3s1)
	K3S_SHA256_x86_64=265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda
	K3S_SHA256_aarch64=a779ccf1e5e2e33a62f5a29c2f1c0bbf99431e100579d4c10f0aa0742a6aa84b
	;;
v1.31.14+k3s1)
	K3S_SHA256_x86_64=660c48d525e5fa2c2ddd889a25ce2c18c961e3046cc083cbde8557a8ff1ddb46
	K3S_SHA256_aarch64=ce16d95de12a85ddf2e72359401dd50dae396cc7725e26f09e2bbb78dae4e555
	;;
*)
	echo "k3s-lab: no checksum is pinned for k3s $K3S_VERSION: add one to k3s-lab.sh, from the release's sha256sum files" >&2
	exit 1
	;;
esac
PAUSE_IMAGE=registry.k8s.io/pause:3.10
LAB_ARCH=${LAB_ARCH:-x86_64}

case "$LAB_ARCH" in
x86_64)
	LAB_DIR=${LAB_DIR:-$HOME/nvweaver-vms}
	QEMU=(qemu-system-x86_64)
	MACHINE=q35
	K3S_ASSET=k3s
	K3S_SHA256=$K3S_SHA256_x86_64
	;;
aarch64)
	LAB_DIR=${LAB_DIR:-$HOME/nvweaver-vms-aarch64}
	QEMU=(qemu-system-aarch64 -bios "${LAB_FIRMWARE:-/usr/share/edk2/aarch64/QEMU_EFI.fd}")
	MACHINE=virt
	K3S_ASSET=k3s-arm64
	K3S_SHA256=$K3S_SHA256_aarch64
	;;
*)
	echo "k3s-lab: LAB_ARCH must be x86_64 or aarch64" >&2
	exit 2
	;;
esac
if [ "$(uname -m)" = "$LAB_ARCH" ]; then
	ACCEL=kvm
	LAB_CPU=${LAB_CPU:-host}
	LAB_TIMEOUT_SCALE=${LAB_TIMEOUT_SCALE:-1}
else
	ACCEL=tcg,thread=multi
	LAB_CPU=${LAB_CPU:-max}
	LAB_TIMEOUT_SCALE=${LAB_TIMEOUT_SCALE:-10}
fi
K3S_CPUS=${K3S_CPUS:-2}
K3S_MEM_MB=${K3S_MEM_MB:-2048}
K3S_SERVER_MEM_MB=${K3S_SERVER_MEM_MB:-3072}
K3S_NVW_MEM_MB=${K3S_NVW_MEM_MB:-3072}
IMAGE=${IMAGE:-localhost/nvweaver:dev}
GOLDEN=$LAB_DIR/golden.qcow2
KEY=$LAB_DIR/id_ed25519
CACHE=$LAB_DIR/k3s-cache
RUN=$LAB_DIR/k3s
SWITCH=$RUN/switch.sock
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
VMS=(nvweaver server agent1 agent2)
NODES=(server agent1 agent2)
SERVER_IP=10.98.0.10
NVW_IP=10.98.0.1
API_FORWARD=16443

SSH_OPTS=(-i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=5 -o LogLevel=ERROR
	-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null)

die() { echo "k3s-lab: $*" >&2; exit 1; }

vm_index() {
	case "$1" in
	nvweaver) echo 1 ;;
	server) echo 10 ;;
	agent1) echo 11 ;;
	agent2) echo 12 ;;
	*) die "VM must be one of ${VMS[*]}, not '$1'" ;;
	esac
}

ssh_port() { local i; i=$(vm_index "$1") || exit 1; echo $((2240 + i)); }

vm_ssh() { # VM [command...]
	local port
	port=$(ssh_port "$1") || exit 1
	shift
	ssh "${SSH_OPTS[@]}" -p "$port" lab@127.0.0.1 "$@"
}

vm_scp() { # VM SOURCE DEST
	scp -q "${SSH_OPTS[@]}" -P "$(ssh_port "$1")" "$2" "lab@127.0.0.1:$3"
}

running() { # PIDFILE: true if it names a process started for this lab
	[ -f "$1" ] && grep -qs "$RUN" "/proc/$(cat "$1")/cmdline"
}

make_seed() { # OUTPUT HOSTNAME USER-DATA-FILE NETWORK-CONFIG-FILE
	local dir
	dir=$(mktemp -d "$RUN/seed.XXXXXX") || exit 1
	printf 'instance-id: %s-%s\nlocal-hostname: %s\n' "$2" "$(date +%s)" "$2" >"$dir/meta-data"
	cp "$3" "$dir/user-data"
	cp "$4" "$dir/network-config"
	genisoimage -quiet -output "$1" -volid cidata -joliet -rock "$dir/user-data" "$dir/meta-data" "$dir/network-config" ||
		die "cannot build seed"
	rm -rf "$dir"
}

# As in lab.sh: each VM makes its own hostnqn, as cloned images commonly share one.
vm_user_data() { # OUTPUT
	cat >"$1" <<EOF
#cloud-config
disable_root: true
users:
  - name: lab
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "$KEY.pub")
runcmd:
  - mkdir -p /etc/nvme
  - nvme gen-hostnqn > /etc/nvme/hostnqn
  - uuidgen > /etc/nvme/hostid
  - systemctl restart sshd
EOF
}

vm_network() { # OUTPUT INDEX
	cat >"$1" <<EOF
version: 2
ethernets:
  mgmt:
    match:
      macaddress: "52:54:00:98:01:$(printf %02x "$2")"
    set-name: mgmt0
    dhcp4: true
  lab:
    match:
      macaddress: "52:54:00:98:00:$(printf %02x "$2")"
    set-name: lab0
    dhcp4: false
    addresses:
      - 10.98.0.$2/24
EOF
}

start_vm() { # NAME MEMORY [extra QEMU args...]
	local name=$1 mem=$2 i port fwd
	shift 2
	i=$(vm_index "$name") || exit 1
	port=$(ssh_port "$name")
	fwd="hostfwd=tcp:127.0.0.1:$port-:22"
	[ "$name" = server ] && fwd="$fwd,hostfwd=tcp:127.0.0.1:$API_FORWARD-:6443"
	qemu-img create -q -f qcow2 -b "$GOLDEN" -F qcow2 "$RUN/$name.qcow2" 30G || exit 1
	vm_user_data "$RUN/$name-user-data"
	vm_network "$RUN/$name-network" "$i"
	make_seed "$RUN/$name-seed.iso" "$name" "$RUN/$name-user-data" "$RUN/$name-network"
	"${QEMU[@]}" -name "nvweaver-k3s-$name" -machine "$MACHINE" -accel "$ACCEL" -cpu "$LAB_CPU" \
		-smp "$K3S_CPUS" -m "$mem" -display none -daemonize \
		-pidfile "$RUN/$name.pid" -serial "file:$RUN/$name-console.log" \
		-drive "file=$RUN/$name.qcow2,if=none,id=root,format=qcow2,cache=unsafe" \
		-device "virtio-blk-pci,drive=root,bootindex=0" \
		-drive "file=$RUN/$name-seed.iso,if=virtio,format=raw,readonly=on" \
		-device virtio-rng-pci \
		-netdev "user,id=mgmt,$fwd" \
		-device "virtio-net-pci,netdev=mgmt,mac=52:54:00:98:01:$(printf %02x "$i")" \
		-netdev "stream,id=lab,addr.type=unix,addr.path=$SWITCH,server=off" \
		-device "virtio-net-pci,netdev=lab,mac=52:54:00:98:00:$(printf %02x "$i")" \
		"$@" || die "cannot start $name"
}

wait_ssh() { # VM
	local _
	for _ in $(seq 1 $((90 * LAB_TIMEOUT_SCALE))); do
		timeout 20 ssh "${SSH_OPTS[@]}" -p "$(ssh_port "$1")" lab@127.0.0.1 'test -s /etc/nvme/hostnqn' 2>/dev/null && return 0
		sleep 2
	done
	die "$1 did not come up; see $RUN/$1-console.log"
}

fetch_k3s() {
	local bin=$CACHE/$K3S_ASSET-$K3S_VERSION got
	mkdir -p "$CACHE" || exit 1
	if [ ! -f "$bin" ]; then
		echo "Downloading k3s $K3S_VERSION" >&2
		curl -fsSL -o "$bin.part" "https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION//+/%2B}/$K3S_ASSET" ||
			die "cannot download k3s"
		got=$(sha256sum "$bin.part" | cut -d' ' -f1)
		[ "$got" = "$K3S_SHA256" ] || die "k3s checksum mismatch: want $K3S_SHA256, got $got"
		mv "$bin.part" "$bin"
	fi
	echo "$bin"
}

# k3s's own install script would do this; writing the unit keeps every flag in view.
install_k3s() { # VM BINARY ROLE-ARGS
	local vm=$1 bin=$2 args=$3 ip
	ip=10.98.0.$(vm_index "$vm")
	vm_scp "$vm" "$bin" /tmp/k3s || die "cannot copy k3s to $vm"
	vm_ssh "$vm" "sudo bash -s" <<EOF || die "cannot install k3s on $vm"
set -e
install -m 0755 /tmp/k3s /usr/local/bin/k3s
rm -f /tmp/k3s
restorecon /usr/local/bin/k3s 2>/dev/null || true
ln -sf k3s /usr/local/bin/kubectl
ln -sf k3s /usr/local/bin/crictl
ln -sf k3s /usr/local/bin/ctr
printf 'nvme-tcp\n' > /etc/modules-load.d/nvweaver-lab.conf
modprobe nvme-tcp
if systemctl is-active -q firewalld; then systemctl disable --now firewalld; fi
cat > /etc/systemd/system/k3s.service <<'UNIT'
[Unit]
Description=k3s, for the nvweaver lab
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s $args --node-ip $ip --flannel-iface lab0
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now k3s
EOF
}

kctl() { vm_ssh server "sudo k3s kubectl $*"; }

cmd_up() {
	[ -f "$GOLDEN" ] || die "no base image; run: $(dirname "$0")/lab.sh base"
	local vm
	for vm in "${VMS[@]}"; do
		running "$RUN/$vm.pid" && die "already running; run: $0 down"
	done
	running "$RUN/switch.pid" && die "the switch is still running; run: $0 down"
	local bin
	bin=$(fetch_k3s) || exit 1
	rm -rf "$RUN"
	mkdir -p "$RUN" || exit 1
	chmod 700 "$RUN"

	setsid python3 "$HERE/k3s-switch.py" "$SWITCH" >"$RUN/switch.log" 2>&1 </dev/null &
	echo $! >"$RUN/switch.pid"
	for _ in $(seq 1 50); do [ -S "$SWITCH" ] && break; sleep 0.1; done
	[ -S "$SWITCH" ] || die "the switch did not start; see $RUN/switch.log"

	qemu-img create -q -f qcow2 "$RUN/nvme0.qcow2" 8G || exit 1
	qemu-img create -q -f qcow2 "$RUN/lvm0.qcow2" 20G || exit 1
	start_vm nvweaver "$K3S_NVW_MEM_MB" \
		-drive "file=$RUN/nvme0.qcow2,if=none,id=nvme0,format=qcow2,cache=unsafe" \
		-device "nvme,serial=NVWEAVERK3S01,drive=nvme0" \
		-drive "file=$RUN/lvm0.qcow2,if=none,id=lvm0,format=qcow2,cache=unsafe" \
		-device "virtio-blk-pci,drive=lvm0,serial=NVWEAVERK3SLVM"
	start_vm server "$K3S_SERVER_MEM_MB"
	start_vm agent1 "$K3S_MEM_MB"
	start_vm agent2 "$K3S_MEM_MB"
	for vm in "${VMS[@]}"; do wait_ssh "$vm"; done

	local token
	token=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	echo "Installing k3s $K3S_VERSION"
	install_k3s server "$bin" "server --token $token --disable traefik --disable servicelb --write-kubeconfig-mode 0600 --tls-san 127.0.0.1"
	for vm in agent1 agent2; do
		install_k3s "$vm" "$bin" "agent --server https://$SERVER_IP:6443 --token $token"
	done

	echo "Waiting for the nodes"
	local ready=0
	for _ in $(seq 1 $((120 * LAB_TIMEOUT_SCALE))); do
		ready=$(kctl get nodes --no-headers 2>/dev/null | awk '$2 == "Ready"' | wc -l)
		[ "$ready" -eq 3 ] && break
		sleep 5
	done
	[ "$ready" -eq 3 ] || { kctl get nodes; die "only $ready of 3 nodes are ready"; }

	vm_ssh server 'sudo cat /etc/rancher/k3s/k3s.yaml' |
		sed "s#https://127.0.0.1:6443#https://127.0.0.1:$API_FORWARD#" >"$RUN/kubeconfig"
	chmod 600 "$RUN/kubeconfig"
	cmd_status
	echo "kubeconfig: $RUN/kubeconfig, on this machine's loopback only"
}

cmd_down() {
	local vm pid
	for vm in "${VMS[@]}"; do
		running "$RUN/$vm.pid" || continue
		timeout 10 ssh "${SSH_OPTS[@]}" -p "$(ssh_port "$vm")" lab@127.0.0.1 'sudo systemctl poweroff' 2>/dev/null
	done
	for vm in "${VMS[@]}"; do
		running "$RUN/$vm.pid" || continue
		pid=$(cat "$RUN/$vm.pid")
		for _ in $(seq 1 30); do
			running "$RUN/$vm.pid" || break
			sleep 1
		done
		running "$RUN/$vm.pid" && kill "$pid"
	done
	running "$RUN/switch.pid" && kill "$(cat "$RUN/switch.pid")"
	sleep 1
	# Only ever this one directory, which this script created.
	[ -n "$RUN" ] && [ "$RUN" = "$LAB_DIR/k3s" ] && rm -rf "$RUN"
	echo "VMs stopped and their disks deleted"
}

cmd_status() {
	local vm
	for vm in "${VMS[@]}"; do
		if running "$RUN/$vm.pid"; then
			echo "$vm: running; $(vm_ssh "$vm" 'echo "kernel $(uname -r), $(cat /etc/nvme/hostnqn)"' 2>/dev/null)"
		else
			echo "$vm: stopped"
		fi
	done
	running "$RUN/server.pid" && kctl get nodes -o wide 2>/dev/null
	return 0
}

cmd_check() {
	local fails=0 vm nqn hosts dev h
	local -A hostnqn
	ok() { echo "ok    $1"; }
	bad() { echo "FAIL  $1"; [ -n "${2:-}" ] && echo "$2" | sed 's/^/      /'; fails=$((fails + 1)); }

	echo "== pods on every node"
	for vm in "${NODES[@]}"; do
		kctl delete pod "lab-check-$vm" --ignore-not-found --wait=true >/dev/null 2>&1
		kctl run "lab-check-$vm" --image="$PAUSE_IMAGE" --restart=Never \
			--overrides="'{\"spec\":{\"nodeName\":\"$vm\"}}'" >/dev/null 2>&1
	done
	for vm in "${NODES[@]}"; do
		if kctl wait --for=condition=Ready "pod/lab-check-$vm" --timeout=$((180 * LAB_TIMEOUT_SCALE))s >/dev/null 2>&1; then
			ok "a pod runs on $vm"
		else
			bad "a pod runs on $vm" "$(kctl describe pod "lab-check-$vm" 2>&1 | tail -8)"
		fi
		kctl delete pod "lab-check-$vm" --wait=false >/dev/null 2>&1
	done
	dev=$(kctl get pods -n kube-system --no-headers 2>/dev/null | awk '$3 != "Running" && $3 != "Completed"')
	[ -z "$dev" ] && ok "every system pod is running" || bad "every system pod is running" "$dev"

	echo "== NVMe/TCP from every node to nvweaver, image $IMAGE"
	cmd_load nvweaver "$IMAGE" >/dev/null 2>&1 && ok "load the image" || bad "load the image"
	vm_ssh nvweaver "sudo podman rm -f nvweaver-k3s-check >/dev/null 2>&1; sudo podman run -d --pull=never --name nvweaver-k3s-check \
		--privileged --network host --pid=host -v /lib/modules:/lib/modules:ro -v /dev:/dev \
		-v nvweaver-k3s-check:/var/lib/nvweaver -e NVWEAVER_BACKEND=nvmet $IMAGE >/dev/null" || bad "start nvweaver"
	api() { # METHOD PATH [BODY]: prints the status code
		vm_ssh nvweaver "curl -s -m 20 -o /dev/null -w '%{http_code}' -X $1 ${3:+-d '$3'} http://127.0.0.1:8420$2"
	}
	for _ in $(seq 1 60); do [ "$(api GET /healthz)" = 200 ] && break; sleep 1; done
	[ "$(api GET /healthz)" = 200 ] && ok "nvweaver starts" || bad "nvweaver starts" "$(vm_ssh nvweaver 'sudo podman logs nvweaver-k3s-check 2>&1 | tail -5')"
	nqn=nqn.2026-11.net.xphyr.nvweaver:k3s-check
	hosts=""
	for vm in "${NODES[@]}"; do
		hostnqn[$vm]=$(vm_ssh "$vm" 'cat /etc/nvme/hostnqn')
		hosts="$hosts${hosts:+,}\"${hostnqn[$vm]}\""
	done
	[ "$(echo "$hosts" | tr , '\n' | sort -u | wc -l)" = 3 ] && ok "every node has its own hostnqn" || bad "every node has its own hostnqn" "$hosts"
	[ "$(api POST /v1/ports "{\"id\":1,\"address\":\"$NVW_IP\",\"service\":4420}")" = 201 ] && ok "a port on the link" || bad "a port on the link"
	for vm in "${NODES[@]}"; do api POST /v1/hosts "{\"nqn\":\"${hostnqn[$vm]}\"}" >/dev/null; done
	[ "$(api POST /v1/subsystems "{\"nqn\":\"$nqn\",\"allowed_hosts\":[$hosts],\"ports\":[1]}")" = 201 ] &&
		ok "a subsystem for the three nodes" || bad "a subsystem for the three nodes"
	[ "$(api POST "/v1/subsystems/$nqn/namespaces" '{"nsid":1,"file":{"name":"k3s-check.img","size_bytes":67108864}}')" = 201 ] &&
		ok "a namespace on a file" || bad "a namespace on a file"
	for vm in "${NODES[@]}"; do
		if vm_ssh "$vm" "sudo nvme connect -t tcp -a $NVW_IP -s 4420 -n $nqn >/dev/null"; then
			dev=""
			for _ in $(seq 1 20); do
				dev=$(vm_ssh "$vm" "for s in /sys/class/nvme-subsystem/*; do [ \"\$(cat \$s/subsysnqn 2>/dev/null)\" = $nqn ] && ls \$s | grep -m1 -E '^nvme[0-9]+n1\$'; done" | head -1)
				[ -n "$dev" ] && break
				sleep 0.5
			done
			if [ -n "$dev" ] && [ "$(vm_ssh "$vm" "sudo blockdev --getsize64 /dev/$dev")" = 67108864 ]; then
				ok "$vm connects over the link and sees 64 MiB"
			else
				bad "$vm connects over the link and sees 64 MiB" "$(vm_ssh "$vm" 'sudo nvme list 2>&1')"
			fi
			vm_ssh "$vm" "sudo nvme disconnect -n $nqn >/dev/null"
		else
			bad "$vm connects over the link"
		fi
	done
	api DELETE "/v1/subsystems/$nqn" >/dev/null
	api DELETE /v1/ports/1 >/dev/null
	for h in "${hostnqn[@]}"; do api DELETE "/v1/hosts/$h" >/dev/null; done
	vm_ssh nvweaver 'sudo podman rm -f nvweaver-k3s-check >/dev/null; sudo podman volume rm nvweaver-k3s-check >/dev/null'
	for vm in "${NODES[@]}"; do
		[ -z "$(vm_ssh "$vm" 'ls /sys/class/nvme 2>/dev/null')" ] && ok "$vm has no controllers left" ||
			bad "$vm has no controllers left" "$(vm_ssh "$vm" 'sudo nvme list-subsys 2>&1')"
	done
	[ -z "$(vm_ssh nvweaver 'sudo find /sys/kernel/config/nvmet/subsystems /sys/kernel/config/nvmet/ports /sys/kernel/config/nvmet/hosts -mindepth 1 -maxdepth 1')" ] &&
		ok "nvweaver left nothing in configfs" ||
		bad "nvweaver left nothing in configfs" "$(vm_ssh nvweaver 'ls -R /sys/kernel/config/nvmet 2>&1 | head')"
	echo "$fails failed"
	[ "$fails" -eq 0 ]
}

cmd_load() { # VM|nodes IMAGE
	local vm
	if [ "$1" = nodes ]; then
		for vm in "${NODES[@]}"; do
			podman save "$2" | vm_ssh "$vm" 'sudo k3s ctr images import -' || return 1
		done
	elif [ "$1" = nvweaver ]; then
		podman save "$2" | vm_ssh nvweaver 'sudo podman load -q'
	else
		vm_index "$1" >/dev/null || exit 1
		podman save "$2" | vm_ssh "$1" 'sudo k3s ctr images import -'
	fi
}

cmd_sync() { # VM
	local port
	port=$(ssh_port "$1") || exit 1
	rsync -a --delete --exclude .git --exclude CLAUDE.local.md --exclude CLAUDE.review.md --exclude '*.priv' --exclude /nvweaverd \
		--exclude /nvweaver --exclude /nvweaver.service --exclude '/.webui-demo-*' -e "ssh ${SSH_OPTS[*]} -p $port" "$ROOT/" lab@127.0.0.1:nvweaver/
}

case "${1:-}" in
up) cmd_up ;;
down) cmd_down ;;
status) cmd_status ;;
check) cmd_check ;;
ssh)
	[ $# -ge 2 ] || die "usage: $0 ssh VM [command]"
	shift
	vm_ssh "$@"
	;;
kubectl)
	shift
	# SSH joins its arguments into one line for the remote shell, so each is quoted, or JSON and sh -c scripts would be split.
	vm_ssh server "sudo k3s kubectl $(printf '%q ' "$@")"
	;;
load)
	[ $# -eq 3 ] || die "usage: $0 load VM|nodes IMAGE"
	cmd_load "$2" "$3"
	;;
sync)
	[ $# -eq 2 ] || die "usage: $0 sync VM"
	cmd_sync "$2"
	;;
*)
	sed -n '2,/^set -u/p' "$0" | sed -n 's/^# \{0,1\}//p'
	exit 2
	;;
esac
