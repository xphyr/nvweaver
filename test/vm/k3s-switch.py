#!/usr/bin/env python3
"""An Ethernet switch for QEMU VMs, on a Unix socket, run as an ordinary user.

QEMU's stream netdev links only two VMs, and multicast would need a loopback that allows it,
or would leak onto the machine's network. This joins any number: each VM connects with
-netdev stream,addr.type=unix,addr.path=SOCKET,server=off. Frames on the stream are each
preceded by their length, 4 bytes big-endian, as QEMU sends them.

It learns which VM has which MAC address, and floods the rest. It's for test labs: one
thread per VM, and nothing is filtered.
"""
import os
import socket
import struct
import sys
import threading

lock = threading.Lock()
ports = {}  # connection: lock for writing to it
where = {}  # MAC address: connection


def read_exactly(conn, n):
    buf = bytearray()
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return bytes(buf)


def send(conn, data):
    with lock:
        wlock = ports.get(conn)
    if wlock is None:
        return
    try:
        with wlock:
            conn.sendall(data)
    except OSError:
        pass


def serve(conn):
    try:
        while True:
            head = read_exactly(conn, 4)
            if head is None:
                break
            (size,) = struct.unpack(">I", head)
            frame = read_exactly(conn, size)
            if frame is None or size < 14:
                break
            dst, src = frame[0:6], frame[6:12]
            with lock:
                where[src] = conn
                out = where.get(dst) if not dst[0] & 1 else None
                targets = [out] if out is not None and out is not conn else [c for c in ports if c is not conn]
            for t in targets:
                send(t, head + frame)
    except OSError:
        pass
    finally:
        with lock:
            ports.pop(conn, None)
            for mac in [m for m, c in where.items() if c is conn]:
                del where[mac]
        conn.close()


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: k3s-switch.py SOCKET")
    path = sys.argv[1]
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    os.chmod(path, 0o600)
    server.listen()
    while True:
        conn, _ = server.accept()
        with lock:
            ports[conn] = threading.Lock()
        threading.Thread(target=serve, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main()
