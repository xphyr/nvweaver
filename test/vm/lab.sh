#!/usr/bin/env bash
# Disposable test VMs, run with QEMU as an ordinary user. No root is needed on the
# machine running them, and tests get real root inside VMs with their own kernels.
#
#   target (tgt)     10.99.0.1  emulated NVMe, SATA and virtio disks
#   initiator (ini)  10.99.0.2  connected to the target by a private link only
#
# Commands:
#   base                 download Fedora and build the provisioned base image, once
#   up                   boot fresh copies of the base image
#   down                 stop the VMs and delete their disks
#   status               show what is running
#   ssh tgt|ini [cmd]    run a command in a VM, as a user with passwordless sudo
#   sync tgt|ini         copy this source tree to ~/nvweaver in a VM
#   load tgt|ini IMAGE   copy a local Podman image into a VM's root image store
#
# Settings: LAB_DIR (~/nvweaver-vms), LAB_CPUS (4), LAB_MEM_MB (4096).
#
# LAB_ARCH=aarch64 runs Arm VMs instead, in ~/nvweaver-vms-aarch64. On an x86 machine they are
# emulated, which needs qemu-system-aarch64 and edk2-aarch64, and is many times slower. LAB_CPU
# picks the CPU model (default max), such as cortex-a72 to stand in for older Arm hardware.
# LAB_TIMEOUT_SCALE multiplies how long to wait for VMs (10 when emulating, otherwise 1).
set -u

FEDORA_VERSION=44
FEDORA_BUILD=44-1.7
LAB_ARCH=${LAB_ARCH:-x86_64}
BASE_NAME=Fedora-Cloud-Base-Generic-$FEDORA_BUILD.$LAB_ARCH.qcow2
FEDORA_URL=https://download.fedoraproject.org/pub/fedora/linux/releases/$FEDORA_VERSION/Cloud/$LAB_ARCH/images
CHECKSUM_NAME=Fedora-Cloud-$FEDORA_BUILD-$LAB_ARCH-CHECKSUM

case "$LAB_ARCH" in
x86_64)
	LAB_DIR=${LAB_DIR:-$HOME/nvweaver-vms}
	QEMU=(qemu-system-x86_64)
	MACHINE=q35
	# The q35 machine has an AHCI controller built in.
	SATA_ARGS=(-device "ide-hd,drive=sata0,bus=ide.0,serial=NVWEAVERSATA")
	;;
aarch64)
	LAB_DIR=${LAB_DIR:-$HOME/nvweaver-vms-aarch64}
	QEMU=(qemu-system-aarch64 -bios "${LAB_FIRMWARE:-/usr/share/edk2/aarch64/QEMU_EFI.fd}")
	MACHINE=virt
	SATA_ARGS=(-device "ich9-ahci,id=ahci" -device "ide-hd,drive=sata0,bus=ahci.0,serial=NVWEAVERSATA")
	;;
*)
	echo "lab: LAB_ARCH must be x86_64 or aarch64" >&2
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
LAB_CPUS=${LAB_CPUS:-4}
LAB_MEM_MB=${LAB_MEM_MB:-4096}
GOLDEN=$LAB_DIR/golden.qcow2
KEY=$LAB_DIR/id_ed25519
RUN=$LAB_DIR/run
ROOT=$(cd "$(dirname "$0")/../.." && pwd)

# IdentitiesOnly: an SSH agent with many keys would otherwise use up the VM's login attempts.
SSH_OPTS=(-i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=5 -o LogLevel=ERROR
	-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null)

die() { echo "lab: $*" >&2; exit 1; }

ssh_port() {
	case "$1" in
	tgt) echo 2221 ;;
	ini) echo 2222 ;;
	*) die "VM must be tgt or ini, not '$1'" ;;
	esac
}

vm_ssh() { # VM [command...]
	local port
	port=$(ssh_port "$1") || exit 1
	shift
	ssh "${SSH_OPTS[@]}" -p "$port" lab@127.0.0.1 "$@"
}

running() { # PIDFILE: true if it names a QEMU process started by this script
	[ -f "$1" ] && grep -qs "$LAB_DIR" "/proc/$(cat "$1")/cmdline"
}

make_seed() { # OUTPUT HOSTNAME USER-DATA-FILE [NETWORK-CONFIG-FILE]
	local dir files
	dir=$(mktemp -d "$LAB_DIR/seed.XXXXXX") || exit 1
	printf 'instance-id: %s-%s\nlocal-hostname: %s\n' "$2" "$(date +%s)" "$2" >"$dir/meta-data"
	cp "$3" "$dir/user-data"
	files=("$dir/user-data" "$dir/meta-data")
	if [ -n "${4:-}" ]; then
		cp "$4" "$dir/network-config"
		files+=("$dir/network-config")
	fi
	genisoimage -quiet -output "$1" -volid cidata -joliet -rock "${files[@]}" || die "cannot build seed"
	rm -rf "$dir"
}

cmd_base() {
	mkdir -p "$LAB_DIR" || exit 1
	chmod 700 "$LAB_DIR"
	# Copy-on-write underneath qcow2 fragments badly. Harmless if unsupported.
	chattr +C "$LAB_DIR" 2>/dev/null
	[ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C nvweaver-lab -f "$KEY" || exit 1

	local cloud=$LAB_DIR/$BASE_NAME
	if [ ! -f "$cloud" ]; then
		echo "Downloading $BASE_NAME"
		curl -fsSL -o "$LAB_DIR/$CHECKSUM_NAME" "$FEDORA_URL/$CHECKSUM_NAME" || die "cannot download checksums"
		curl -fsSL -o "$cloud.part" "$FEDORA_URL/$BASE_NAME" || die "cannot download image"
		local want got
		want=$(awk -v f="($BASE_NAME)" '$1 == "SHA256" && $2 == f { print $4 }' "$LAB_DIR/$CHECKSUM_NAME")
		got=$(sha256sum "$cloud.part" | cut -d' ' -f1)
		[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch: want '$want', got '$got'"
		mv "$cloud.part" "$cloud"
		echo "Checksum verified"
	fi

	local building=$LAB_DIR/golden.building.qcow2
	rm -f "$building"
	qemu-img create -q -f qcow2 -b "$cloud" -F qcow2 "$building" 30G || exit 1

	# kernel-modules and -extra must match the running kernel, hence upgrading the
	# kernel with them. Identity is removed at the end, so that clones don't share a
	# machine ID or, as cloned hosts commonly do, a hostnqn. Fedora waits only 45 seconds for
	# disks, which an emulated Cortex-A72 misses, ending in emergency mode.
	cat >"$LAB_DIR/base-user-data" <<EOF
#cloud-config
disable_root: true
users:
  - name: lab
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "$KEY.pub")
runcmd:
  - mkdir -p /etc/systemd/system.conf.d
  - printf '[Manager]\nDefaultDeviceTimeoutSec=600\n' > /etc/systemd/system.conf.d/90-nvweaver-lab.conf
  - dnf -y upgrade --refresh
  - dnf -y install kernel-modules kernel-modules-extra podman nvme-cli fio make jq curl rsync lvm2 xfsprogs tar
  - rm -f /etc/nvme/hostnqn /etc/nvme/hostid
  - cloud-init clean --logs --seed
  - truncate -s 0 /etc/machine-id
  - rm -f /etc/ssh/ssh_host_*
  - fstrim -av
  - sync
  - poweroff
EOF
	make_seed "$LAB_DIR/base-seed.iso" base "$LAB_DIR/base-user-data"
	echo "Provisioning the base image; this upgrades Fedora and takes several minutes"
	"${QEMU[@]}" -name nvweaver-lab-base -machine "$MACHINE" -accel "$ACCEL" -cpu "$LAB_CPU" \
		-smp "$LAB_CPUS" -m "$LAB_MEM_MB" -display none -daemonize \
		-pidfile "$LAB_DIR/base.pid" -serial "file:$LAB_DIR/base-console.log" \
		-drive "file=$building,if=virtio,format=qcow2" \
		-drive "file=$LAB_DIR/base-seed.iso,if=virtio,format=raw,readonly=on" \
		-device virtio-rng-pci -nic user,model=virtio-net-pci || die "cannot start QEMU"
	for _ in $(seq 1 $((240 * LAB_TIMEOUT_SCALE))); do
		running "$LAB_DIR/base.pid" || break
		sleep 5
	done
	if running "$LAB_DIR/base.pid"; then
		kill "$(cat "$LAB_DIR/base.pid")"
		tail -20 "$LAB_DIR/base-console.log"
		die "provisioning did not finish in $((20 * LAB_TIMEOUT_SCALE)) minutes"
	fi
	rm -f "$LAB_DIR/base.pid"
	# Flatten, so that the base image is one self-contained file.
	qemu-img convert -O qcow2 "$building" "$GOLDEN.new" || exit 1
	mv "$GOLDEN.new" "$GOLDEN"
	rm -f "$building" "$LAB_DIR/base-seed.iso" "$LAB_DIR/base-user-data"
	du -h "$GOLDEN"
	echo "Base image ready"
}

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

vm_network() { # OUTPUT MAC ADDRESS
	cat >"$1" <<EOF
version: 2
ethernets:
  mgmt:
    match:
      macaddress: "52:54:00:99:01:${2##*:}"
    set-name: mgmt0
    dhcp4: true
  lab:
    match:
      macaddress: "$2"
    set-name: lab0
    dhcp4: false
    addresses:
      - $3/24
EOF
}

start_vm() { # NAME SSH-PORT MAC ADDRESS LINK-OPTS [extra QEMU args...]
	local name=$1 port=$2 mac=$3 addr=$4 link=$5
	shift 5
	local disk=$RUN/$name.qcow2
	qemu-img create -q -f qcow2 -b "$GOLDEN" -F qcow2 "$disk" 30G || exit 1
	vm_user_data "$RUN/$name-user-data"
	vm_network "$RUN/$name-network" "$mac" "$addr"
	make_seed "$RUN/$name-seed.iso" "$name" "$RUN/$name-user-data" "$RUN/$name-network"
	"${QEMU[@]}" -name "nvweaver-lab-$name" -machine "$MACHINE" -accel "$ACCEL" -cpu "$LAB_CPU" \
		-smp "$LAB_CPUS" -m "$LAB_MEM_MB" -display none -daemonize \
		-pidfile "$RUN/$name.pid" -serial "file:$RUN/$name-console.log" \
		-drive "file=$disk,if=none,id=root,format=qcow2,cache=unsafe" \
		-device "virtio-blk-pci,drive=root,bootindex=0" \
		-drive "file=$RUN/$name-seed.iso,if=virtio,format=raw,readonly=on" \
		-device virtio-rng-pci \
		-netdev "user,id=mgmt,hostfwd=tcp:127.0.0.1:$port-:22" \
		-device "virtio-net-pci,netdev=mgmt,mac=52:54:00:99:01:${mac##*:}" \
		-netdev "stream,id=lab,addr.type=unix,addr.path=$RUN/link.sock,$link" \
		-device "virtio-net-pci,netdev=lab,mac=$mac" \
		"$@" || die "cannot start $name"
}

# A scratch disk for a test to use: QEMU-backed, so losing it costs nothing.
scratch() { # FILE SIZE
	qemu-img create -q -f qcow2 "$1" "$2" || exit 1
}

cmd_up() {
	[ -f "$GOLDEN" ] || die "no base image; run: $0 base"
	running "$RUN/tgt.pid" || running "$RUN/ini.pid" && die "already running; run: $0 down"
	rm -rf "$RUN"
	mkdir -p "$RUN" || exit 1
	local d
	for d in nvme0 nvme1 sata0 virtio0; do scratch "$RUN/$d.qcow2" 8G; done
	# The target's socket exists once QEMU has started, and rebooting a guest doesn't restart
	# QEMU, so the initiator's side has no need to reconnect.
	start_vm tgt 2221 52:54:00:99:00:01 10.99.0.1 server=on \
		-drive "file=$RUN/nvme0.qcow2,if=none,id=nvme0,format=qcow2,cache=unsafe" \
		-device "nvme,serial=NVWEAVER0001,drive=nvme0" \
		-drive "file=$RUN/nvme1.qcow2,if=none,id=nvme1,format=qcow2,cache=unsafe" \
		-device "nvme,serial=NVWEAVER0002,drive=nvme1" \
		-drive "file=$RUN/sata0.qcow2,if=none,id=sata0,format=qcow2,cache=unsafe" \
		"${SATA_ARGS[@]}" \
		-drive "file=$RUN/virtio0.qcow2,if=none,id=scratchv,format=qcow2,cache=unsafe" \
		-device "virtio-blk-pci,drive=scratchv,serial=NVWEAVERVIRTIO"
	start_vm ini 2222 52:54:00:99:00:02 10.99.0.2 server=off
	local vm
	for vm in tgt ini; do
		for _ in $(seq 1 $((90 * LAB_TIMEOUT_SCALE))); do
			timeout 20 ssh "${SSH_OPTS[@]}" -p "$(ssh_port "$vm")" lab@127.0.0.1 'test -s /etc/nvme/hostnqn' 2>/dev/null && break
			sleep 2
		done
		timeout 20 ssh "${SSH_OPTS[@]}" -p "$(ssh_port "$vm")" lab@127.0.0.1 'test -s /etc/nvme/hostnqn' 2>/dev/null ||
			die "$vm did not come up; see $RUN/$vm-console.log"
	done
	cmd_status
}

cmd_down() {
	local vm pid
	for vm in tgt ini; do
		running "$RUN/$vm.pid" || continue
		pid=$(cat "$RUN/$vm.pid")
		timeout 10 ssh "${SSH_OPTS[@]}" -p "$(ssh_port "$vm")" lab@127.0.0.1 'sudo systemctl poweroff' 2>/dev/null
		for _ in $(seq 1 30); do
			running "$RUN/$vm.pid" || break
			sleep 1
		done
		running "$RUN/$vm.pid" && kill "$pid"
	done
	sleep 1
	# Only ever this one directory, which this script created.
	[ -n "$RUN" ] && [ "$RUN" = "$LAB_DIR/run" ] && rm -rf "$RUN"
	echo "VMs stopped and their disks deleted"
}

cmd_status() {
	local vm
	for vm in tgt ini; do
		if running "$RUN/$vm.pid"; then
			echo "$vm: running; $(vm_ssh "$vm" 'echo "$(hostname), kernel $(uname -r), $(cat /etc/nvme/hostnqn)"' 2>/dev/null)"
		else
			echo "$vm: stopped"
		fi
	done
}

cmd_sync() { # VM
	local port
	port=$(ssh_port "$1") || exit 1
	rsync -a --delete --exclude .git --exclude CLAUDE.local.md --exclude /nvweaverd \
		--exclude /nvweaver.service -e "ssh ${SSH_OPTS[*]} -p $port" "$ROOT/" lab@127.0.0.1:nvweaver/
}

cmd_load() { # VM IMAGE
	podman save "$2" | vm_ssh "$1" 'sudo podman load -q'
}

case "${1:-}" in
base) cmd_base ;;
up) cmd_up ;;
down) cmd_down ;;
status) cmd_status ;;
ssh)
	[ $# -ge 2 ] || die "usage: $0 ssh tgt|ini [command]"
	shift
	vm_ssh "$@"
	;;
sync)
	[ $# -eq 2 ] || die "usage: $0 sync tgt|ini"
	cmd_sync "$2"
	;;
load)
	[ $# -eq 3 ] || die "usage: $0 load tgt|ini IMAGE"
	cmd_load "$2" "$3"
	;;
*)
	sed -n '2,/^set -u/p' "$0" | sed -n 's/^# \{0,1\}//p'
	exit 2
	;;
esac
