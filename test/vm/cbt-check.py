#!/usr/bin/env python3
"""Changed block tracking: does the storage ever under-report? Run as root in a disposable VM, by cbt-check.sh.

A backup built on a delta that misses one changed byte is silently corrupt, and so is every backup after it. So before
nvweaver reports changed or allocated ranges, this checks the methods it would use, against the data itself:

- LVM thin: thin_delta and thin_dump, on a reserved metadata snapshot of a live pool, at several chunk sizes.
- Reflink: FIEMAP on two reflinked snapshot files, on XFS, where a different physical extent, or a hole on one side only,
  is a change.

A volume goes through rounds of changes (unaligned bytes, writes across chunk boundaries, random small writes, TRIM,
writes into never-written space, identical rewrites, nothing at all, and a full overwrite), with a read-only snapshot after
each, as nvweaver takes them. For pairs of snapshots, every byte that differs between them must be inside a reported
changed range, and every non-zero byte of a snapshot inside its reported allocated ranges. Reporting more is allowed, and
its ratio is printed. Only loop devices on files in a scratch directory are used.
"""

import bisect
import fcntl
import os
import random
import struct
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET

MIB = 1 << 20
SIZE = 256 * MIB
CHECK_BLOCK = 64 * 1024
failures = 0


def run(*args, **kw):
    return subprocess.run(args, check=True, capture_output=True, text=True, **kw).stdout


def report(ok, what, detail=""):
    global failures
    if not ok:
        failures += 1
    print(f"{'ok  ' if ok else 'FAIL'}  {what}" + (f": {detail}" if detail else ""), flush=True)


# Ranges are sorted, merged lists of (start, end) in bytes.
def merge(ranges):
    out = []
    for s, e in sorted(r for r in ranges if r[1] > r[0]):
        if out and s <= out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], e))
        else:
            out.append((s, e))
    return out


def covered(ranges, starts, s, e):
    """Whether [s, e) lies inside the merged ranges."""
    i = bisect.bisect_right(starts, s) - 1
    return i >= 0 and ranges[i][0] <= s and e <= ranges[i][1]


def uncovered_bytes(ranges, data_a, data_b, base, starts):
    """Bytes in this block that differ (or, with data_b None, are non-zero) and aren't covered."""
    n = 0
    for i in range(len(data_a)):
        differs = data_a[i] != data_b[i] if data_b is not None else data_a[i] != 0
        if differs and not covered(ranges, starts, base + i, base + i + 1):
            n += 1
    return n


def first_byte(path_a, path_b):
    """The first byte that differs between two, or with path_b None, that isn't zero."""
    with open(path_a, "rb", buffering=0) as a, open(path_b or "/dev/zero", "rb", buffering=0) as b:
        for off in range(0, SIZE, CHECK_BLOCK):
            x, y = a.read(CHECK_BLOCK), b.read(CHECK_BLOCK)
            if x != y:
                return off + next(i for i in range(len(x)) if x[i] != y[i])
    return None


def control(what, path_a, path_b, ranges, check):
    """A check that can't fail proves nothing: without the range that covers the first changed byte, it must fail."""
    first = first_byte(path_a, path_b)
    if first is None:
        return
    ranges = merge(ranges)
    without = [r for r in ranges if not r[0] <= first < r[1]]
    missed = check(what, path_a, path_b, without, quiet=True) if path_b else check(what, path_a, without, quiet=True)
    report(missed > 0, f"{what}: and fails without the range holding byte {first}", f"{missed} bytes unreported then")


def check_delta(what, path_a, path_b, ranges, quiet=False):
    """Every byte that differs between the two must be in ranges."""
    ranges = merge(ranges)
    starts = [r[0] for r in ranges]
    diff = missed = 0
    with open(path_a, "rb", buffering=0) as a, open(path_b, "rb", buffering=0) as b:
        for off in range(0, SIZE, CHECK_BLOCK):
            x, y = a.read(CHECK_BLOCK), b.read(CHECK_BLOCK)
            if x == y:
                continue
            for sub in range(0, CHECK_BLOCK, 4096):
                xs, ys = x[sub:sub + 4096], y[sub:sub + 4096]
                if xs == ys:
                    continue
                diff += sum(1 for i in range(4096) if xs[i] != ys[i])
                if not covered(ranges, starts, off + sub, off + sub + 4096):
                    missed += uncovered_bytes(ranges, xs, ys, off + sub, starts)
    if quiet:
        return missed
    reported = sum(e - s for s, e in ranges)
    report(missed == 0, what, f"{diff} bytes differ, {missed} of them unreported; {reported // 1024} KiB reported")
    if diff:
        control(what, path_a, path_b, ranges, check_delta)
    return missed


def check_allocated(what, path, ranges, quiet=False):
    """Every non-zero byte must be in ranges."""
    ranges = merge(ranges)
    starts = [r[0] for r in ranges]
    nonzero = missed = 0
    zero = bytes(4096)
    with open(path, "rb", buffering=0) as f:
        for off in range(0, SIZE, CHECK_BLOCK):
            x = f.read(CHECK_BLOCK)
            for sub in range(0, CHECK_BLOCK, 4096):
                xs = x[sub:sub + 4096]
                if xs == zero:
                    continue
                nonzero += 4096 - xs.count(0)
                if not covered(ranges, starts, off + sub, off + sub + 4096):
                    missed += uncovered_bytes(ranges, xs, None, off + sub, starts)
    if quiet:
        return missed
    reported = sum(e - s for s, e in ranges)
    report(missed == 0, what, f"{nonzero} non-zero bytes, {missed} of them unreported; {reported // 1024} KiB reported")
    if nonzero:
        control(what, path, None, ranges, check_allocated)
    return missed


# The rounds of changes. Each takes a writable path and the chunk size, and leaves its changes on stable storage.
def pwrite(path, off, data):
    fd = os.open(path, os.O_WRONLY)
    try:
        os.pwrite(fd, data, off)
        os.fsync(fd)
    finally:
        os.close(fd)


def trim(path, off, length, is_file):
    if is_file:
        run("fallocate", "--punch-hole", "--keep-size", "--offset", str(off), "--length", str(length), path)
    else:
        run("blkdiscard", "--offset", str(off), "--length", str(length), path)


def rounds(chunk):
    rng = random.Random(chunk)

    def first_half(p, f):
        pwrite(p, 0, rng.randbytes(SIZE // 2))  # The rest is never written.

    def unaligned(p, f):
        for off in (0, chunk - 1, chunk, SIZE // 2 - 1, SIZE - 1):
            pwrite(p, off, b"\xa5")
        pwrite(p, 3 * chunk - 2048, rng.randbytes(4096))  # Across a chunk boundary.
        pwrite(p, 5 * chunk + 777, rng.randbytes(MIB + 123))  # Unaligned at both ends, over several chunks.
        pwrite(p, SIZE // 2 - 100, rng.randbytes(200))  # From written space into never-written.

    def scattered(p, f):
        fd = os.open(p, os.O_WRONLY)
        try:
            for _ in range(1000):
                os.pwrite(fd, rng.randbytes(512), rng.randrange(SIZE // 512) * 512)
            os.fsync(fd)
        finally:
            os.close(fd)

    def trims(p, f):
        trim(p, 8 * chunk, 4 * chunk, f)  # Whole chunks.
        trim(p, 20 * chunk + 4096, chunk + 512, f)  # Parts of chunks, which may stay mapped.
        trim(p, SIZE // 2 + 16 * MIB, 4 * MIB, f)  # Never written, so no change.

    def same_and_zeros(p, f):
        with open(p, "rb") as r:
            head = r.read(2 * MIB)
        pwrite(p, 0, head)  # The same data again: a new mapping, but no change in content.
        pwrite(p, 3 * SIZE // 4, bytes(MIB))  # Zeros into never-written space.

    def nothing(p, f):
        pass

    def everything(p, f):
        for off in range(0, SIZE, 16 * MIB):
            pwrite(p, off, rng.randbytes(16 * MIB))

    return [first_half, unaligned, scattered, trims, same_and_zeros, nothing, everything]


# Pairs of snapshots to compare: consecutive ones, and some further apart, as a backup may skip snapshots.
def pairs(n):
    out = [(i, i + 1) for i in range(n - 1)]
    out += [(0, n - 1), (1, 4), (2, 5)]
    return out


# LVM thin.
def lvm_case(dir, chunk, zero):
    vg = "cbtcheck"
    name = f"LVM, {chunk // 1024} KiB chunks, zeroing {'on' if zero else 'off'}"
    print(f"== {name}", flush=True)
    img = os.path.join(dir, "lvm.img")
    with open(img, "wb") as f:
        f.truncate(4 * SIZE)
    loop = run("losetup", "--find", "--show", img).strip()
    try:
        run("pvcreate", "-q", "--devices", loop, loop)
        run("vgcreate", "-q", "--devices", loop, vg, loop)
        lvm = ["--devices", loop]
        run("lvcreate", "-q", *lvm, "--type", "thin-pool", "-L", f"{3 * SIZE // MIB}m", "-c", f"{chunk // 1024}k",
            "--poolmetadatasize", "16m", "-Z", "y" if zero else "n", "-n", "pool0", vg)
        run("lvcreate", "-q", *lvm, "--thin", "-V", f"{SIZE // MIB}m", "-n", "vol", f"{vg}/pool0")
        vol = f"/dev/{vg}/vol"
        snaps = []
        for i, change in enumerate(rounds(chunk)):
            change(vol, False)
            # As nvweaver takes a snapshot: read-only, and not activated until needed.
            run("lvcreate", "-q", *lvm, "-s", "-pr", "-ky", "-n", f"s{i}", f"{vg}/vol")
            run("lvchange", "-q", *lvm, "-ay", "-K", f"{vg}/s{i}")
            snaps.append(f"/dev/{vg}/s{i}")
        ids = [int(run("lvs", *lvm, "--noheadings", "-o", "thin_id", f"{vg}/s{i}").strip()) for i in range(len(snaps))]
        pool = f"{vg}-pool0-tpool"
        tmeta = f"/dev/mapper/{vg}-pool0_tmeta"
        # Read the mappings from a metadata snapshot, as the pool is live. It holds what was committed when reserved.
        run("dmsetup", "message", pool, "0", "reserve_metadata_snap")
        try:
            for a, b in pairs(len(snaps)):
                xml = run("thin_delta", "-m", "--snap1", str(ids[a]), "--snap2", str(ids[b]), tmeta)
                check_delta(f"{name}: s{a} to s{b}", snaps[a], snaps[b], thin_delta_ranges(xml))
            for i in (0, 3, len(snaps) - 1):
                xml = run("thin_dump", "-m", "--dev-id", str(ids[i]), tmeta)
                check_allocated(f"{name}: allocated in s{i}", snaps[i], thin_dump_ranges(xml))
        finally:
            run("dmsetup", "message", pool, "0", "release_metadata_snap")
    finally:
        subprocess.run(["vgremove", "-q", "-f", "--devices", loop, vg], capture_output=True)
        subprocess.run(["pvremove", "-q", "--devices", loop, loop], capture_output=True)
        subprocess.run(["losetup", "-d", loop])
        os.unlink(img)


def thin_delta_ranges(xml):
    """thin_delta's ranges are in the pool's data blocks, given in sectors by the superblock."""
    root = ET.fromstring(xml)
    block = int(root.get("data_block_size")) * 512
    out = []
    for e in root.iter():
        if e.tag in ("different", "left_only", "right_only"):
            b, n = int(e.get("begin")), int(e.get("length"))
            out.append((b * block, (b + n) * block))
    return out


def thin_dump_ranges(xml):
    root = ET.fromstring(xml)
    block = int(root.get("data_block_size")) * 512
    out = []
    for e in root.iter():
        if e.tag == "range_mapping":
            b, n = int(e.get("origin_begin")), int(e.get("length"))
            out.append((b * block, (b + n) * block))
        elif e.tag == "single_mapping":
            b = int(e.get("origin_block"))
            out.append((b * block, (b + 1) * block))
    return out


# Reflink, by FIEMAP.
FS_IOC_FIEMAP = 0xC020660B
FIEMAP_FLAG_SYNC = 0x1
FIEMAP_EXTENT_LAST = 0x1
FIEMAP_EXTENT_UNWRITTEN = 0x800


def fiemap(path):
    """A file's written extents, as (logical, physical, length): unwritten extents read as zeros, like holes."""
    out, start = [], 0
    fd = os.open(path, os.O_RDONLY)
    try:
        while True:
            count = 256
            buf = bytearray(struct.pack("=QQLLLL", start, (1 << 64) - 1 - start, FIEMAP_FLAG_SYNC, 0, count, 0))
            buf += bytes(56 * count)
            fcntl.ioctl(fd, FS_IOC_FIEMAP, buf)
            mapped = struct.unpack_from("=L", buf, 20)[0]
            last = False
            for i in range(mapped):
                logical, physical, length, _, _, flags = struct.unpack_from("=QQQQQL", buf, 32 + 56 * i)
                if not flags & FIEMAP_EXTENT_UNWRITTEN:
                    out.append((logical, physical, length))
                start = logical + length
                last = bool(flags & FIEMAP_EXTENT_LAST)
            if mapped == 0 or last:
                return out
    finally:
        os.close(fd)


def fiemap_delta(a, b):
    """Where two files' mappings differ: a different physical place, or written on one side only."""
    def at(extents, pos):
        for logical, physical, length in extents:
            if logical <= pos < logical + length:
                return physical + (pos - logical), logical + length
        nxt = min((l for l, _, _ in extents if l > pos), default=SIZE)
        return None, nxt

    ea, eb = fiemap(a), fiemap(b)
    out, pos = [], 0
    while pos < SIZE:
        pa, enda = at(ea, pos)
        pb, endb = at(eb, pos)
        end = min(enda, endb, SIZE)
        if pa != pb:
            out.append((pos, end))
        pos = end
    return out


def reflink_case(dir):
    name = "Reflink on XFS"
    print(f"== {name}", flush=True)
    img = os.path.join(dir, "xfs.img")
    mnt = os.path.join(dir, "mnt")
    with open(img, "wb") as f:
        f.truncate(12 * SIZE)
    os.mkdir(mnt)
    run("mkfs.xfs", "-q", "-m", "reflink=1", img)
    run("mount", "-o", "loop", img, mnt)
    try:
        vol = os.path.join(mnt, "vol.img")
        with open(vol, "wb") as f:
            f.truncate(SIZE)
        snaps = []
        for i, change in enumerate(rounds(64 * 1024)):
            change(vol, True)
            snap = os.path.join(mnt, f"s{i}.img")
            run("cp", "--reflink=always", vol, snap)  # FICLONE, as nvweaver's snapshots.
            snaps.append(snap)
        for a, b in pairs(len(snaps)):
            check_delta(f"{name}: s{a} to s{b}", snaps[a], snaps[b], fiemap_delta(snaps[a], snaps[b]))
        for i in (0, 3, len(snaps) - 1):
            check_allocated(f"{name}: allocated in s{i}", snaps[i], [(l, l + n) for l, _, n in fiemap(snaps[i])])
    finally:
        run("umount", mnt)
        os.unlink(img)


def main():
    if os.geteuid() != 0:
        sys.exit("cbt-check.py needs root, in a disposable VM")
    with tempfile.TemporaryDirectory(prefix="cbt-check-", dir="/var/tmp") as dir:
        for chunk, zero in ((64 * 1024, True), (64 * 1024, False), (128 * 1024, True), (256 * 1024, True), (MIB, True)):
            lvm_case(dir, chunk, zero)
        reflink_case(dir)
    print(f"{failures} failed")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
