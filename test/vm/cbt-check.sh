#!/usr/bin/env bash
# Changed block tracking's correctness test: whether thin_delta, thin_dump and FIEMAP ever under-report what changed
# between two snapshots, or what a snapshot holds. Runs cbt-check.py as root in a fresh lab VM, on loop devices, and
# takes the VMs down after. Needs no images. See "Changed block tracking" in docs/testing.md.
set -u
LAB=$(dirname "$0")/lab.sh

"$LAB" down >/dev/null 2>&1
"$LAB" up >/dev/null || exit 1
trap '"$LAB" down >/dev/null 2>&1' EXIT
"$LAB" sync tgt >/dev/null || exit 1
"$LAB" ssh tgt 'rpm -q device-mapper-persistent-data xfsprogs lvm2; uname -r'
"$LAB" ssh tgt 'sudo python3 nvweaver/test/vm/cbt-check.py'
