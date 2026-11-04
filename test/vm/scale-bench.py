#!/usr/bin/env python3
"""Measures the API as configuration grows, by provisioning volumes as a CSI driver would.

SHAPE=node (default) has one subsystem per node, as CSI drivers for enterprise arrays do, and
adds each volume to one of them. It also measures moving a volume between nodes, which is what
rescheduling a pod does. SHAPE=volume gives each volume its own subsystem.

Runs inside a VM, next to a running service, and prints results as tab-separated lines. It
stops at the time budget or the first error: how far it got is part of the result.
"""
import http.client
import json
import os
import statistics
import subprocess
import sys
import time

API_PORT = 8420
NQN = "nqn.2026-11.net.xphyr.scale"
HOSTS = 100
SHAPE = os.environ.get("SHAPE", "node")
CHECKPOINTS = [100, 500, 1000, 2000, 3000, 4000, 5000]
BUDGET = float(os.environ.get("BUDGET_SECONDS", "600"))
CONTAINER = os.environ.get("CONTAINER", "nvweaver-scale")
TCP_PORT = 14500
EVERY_HOST = f"{NQN}:every"

conn = http.client.HTTPConnection("127.0.0.1", API_PORT, timeout=300)


def call(method, path, body=None):
    """Returns the status, the parsed body and the milliseconds taken."""
    data = json.dumps(body) if body is not None else None
    start = time.perf_counter()
    conn.request(method, path, data)
    resp = conn.getresponse()
    raw = resp.read()
    elapsed = (time.perf_counter() - start) * 1000
    return resp.status, raw, elapsed


def must(want, method, path, body=None):
    status, raw, elapsed = call(method, path, body)
    if status != want:
        raise RuntimeError(f"{method} {path}: HTTP {status}: {raw[:300].decode(errors='replace')}")
    return elapsed


def shell(command):
    return subprocess.run(command, shell=True, capture_output=True, text=True).stdout.strip()


def rss_mb_sudo(pid):
    out = shell(f"sudo awk '/VmRSS/{{print $2}}' /proc/{pid}/status")
    return int(out) // 1024 if out else 0


def usage():
    """Returns control plane memory, target memory and state file size, in MB."""
    pid = shell(f"sudo podman inspect {CONTAINER} --format '{{{{.State.Pid}}}}'")
    child = shell(f"sudo cat /proc/{pid}/task/{pid}/children").split()
    state = shell(f"sudo podman exec {CONTAINER} stat -c %s /var/lib/nvweaver/state.json") or "0"
    return rss_mb_sudo(pid), (rss_mb_sudo(child[0]) if child else 0), int(state) / 1048576


def provision(i):
    """Creates one volume, returning the time for each of its five API calls."""
    sub = f"{NQN}:v{i}"
    return [
        must(201, "POST", "/v1/subsystems", {"nqn": sub}),
        must(201, "POST", f"/v1/subsystems/{sub}/namespaces",
             {"nsid": 1, "file": {"name": f"v{i}.img", "size_bytes": 1048576}}),
        must(204, "PUT", f"/v1/subsystems/{sub}/hosts/{NQN}:h{i % HOSTS}"),
        must(204, "PUT", f"/v1/subsystems/{sub}/hosts/{EVERY_HOST}"),
        must(204, "PUT", f"/v1/subsystems/{sub}/ports/1"),
    ]


def node_sub(n):
    return f"{NQN}:node{n}"


placed = {}  # Volume number to its node and NSID, for moving it later.


def provision_on_node(i):
    """Creates one volume in its node's subsystem: one API call."""
    node = i % HOSTS
    status, raw, elapsed = call("POST", f"/v1/subsystems/{node_sub(node)}/namespaces",
                                {"file": {"name": f"v{i}.img", "size_bytes": 1048576}})
    if status != 201:
        raise RuntimeError(f"namespace for volume {i}: HTTP {status}: {raw[:300].decode(errors='replace')}")
    placed[i] = (node, json.loads(raw)["nsid"])
    return [elapsed]


def move_volume(i):
    """Moves a volume to the next node, returning the milliseconds it took."""
    node, nsid = placed[i]
    new_node = (node + 1) % HOSTS
    took = must(204, "DELETE", f"/v1/subsystems/{node_sub(node)}/namespaces/{nsid}")
    status, raw, elapsed = call("POST", f"/v1/subsystems/{node_sub(new_node)}/namespaces",
                                {"device_path": f"/var/lib/nvweaver/volumes/v{i}.img"})
    if status != 201:
        raise RuntimeError(f"moving volume {i}: HTTP {status}: {raw[:300].decode(errors='replace')}")
    placed[i] = (new_node, json.loads(raw)["nsid"])
    return took + elapsed


def main():
    print("volumes\tprovision_p50_ms\tprovision_p95_ms\tmove_p50_ms\tlist_ms\tlist_mb\tservice_mb\ttarget_mb\tstate_mb\telapsed_s")
    sys.stdout.flush()
    must(201, "POST", "/v1/ports", {"id": 1, "address": "127.0.0.1", "service": TCP_PORT})
    for h in range(HOSTS):
        must(201, "POST", "/v1/hosts", {"nqn": f"{NQN}:h{h}"})
        if SHAPE == "node":
            must(201, "POST", "/v1/subsystems",
                 {"nqn": node_sub(h), "allowed_hosts": [f"{NQN}:h{h}"], "ports": [1]})
    must(201, "POST", "/v1/hosts", {"nqn": EVERY_HOST})
    make = provision_on_node if SHAPE == "node" else provision

    start = time.time()
    created = 0
    stopped = "reached the largest size"
    try:
        for checkpoint in CHECKPOINTS:
            samples = []
            while created < checkpoint:
                samples.append(sum(make(created)))
                created += 1
                if time.time() - start > BUDGET:
                    stopped = f"time budget of {BUDGET:.0f}s"
                    break
            moves = [move_volume(i) for i in range(10)] if SHAPE == "node" else [0]
            status, raw, list_ms = call("GET", "/v1/subsystems")
            service, tgt, state = usage()
            print(f"{created}\t{statistics.median(samples):.0f}\t"
                  f"{statistics.quantiles(samples, n=20)[-1] if len(samples) > 1 else samples[0]:.0f}\t"
                  f"{statistics.median(moves):.0f}\t{list_ms:.0f}\t{len(raw) / 1048576:.1f}\t{service}\t{tgt}\t{state:.2f}\t{time.time() - start:.0f}")
            sys.stdout.flush()
            if stopped != "reached the largest size":
                break
    except (RuntimeError, OSError) as err:
        stopped = f"error at {created} volumes: {err}"
    print(f"# stopped: {stopped}")
    print(f"# volumes created: {created}")
    with open("/tmp/scale-created", "w") as f:
        f.write(str(created))


if __name__ == "__main__":
    main()
