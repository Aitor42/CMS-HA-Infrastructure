#!/usr/bin/env bash
# casper_initrd_tcp.sh — Force casper's NFS mount over TCP.
#
# Background: the PXE clients boot the live-server casper environment with
# netboot=nfs, and casper mounts the NFS root via BusyBox `nfsmount -o nolock
# -o ro`, which defaults to NFSv3/UDP. Three concurrent clients streaming the
# ~2.5 GB squashfs over one NFS export swamp UDP, and the live environment
# dies in `SQASHFS error`/`I/O error, dev loop0` storms so ssh.service never
# starts and install-batches times out waiting for SSH.
#
# Fix: casper ignores NFS options from the kernel command line (NFSOPTS is
# dead code in casper 25.04.1); the only lever is the mount line inside
# /scripts/casper of the initrd. The Ubuntu 24.04 initrd is a concatenation of
# cpio segments (uncompressed microcode/kernel/rootfs plus a final zstd
# segment). We keep the file byte-for-byte and APPEND one more cpio segment
# containing a patched /scripts/casper that issues the mount with `-o ro,tcp`.
# A later cpio entry with the same path overrides the earlier one, and Cobbler
# hands this exact file to PXE through hard links in its tftpboot tree.
#
# Idempotent: a marker file plus a content check skip the work when already
# applied. Exits non-zero (aborts the phase) on any integrity problem:
# a corrupt mirror or a changed casper layout would otherwise produce PXE
# installs that fail 15 minutes later inside install-batches.

set -euo pipefail

MIRROR_DIR="${1:-/var/www/cobbler/distro_mirror/ubuntu-24.04}"
CASPER_DIR="$MIRROR_DIR/casper"
INITRD="$CASPER_DIR/initrd"
MARKER="$CASPER_DIR/.tcp-patched"

[ -f "$INITRD" ] || { echo "[ERROR] initrd not found: $INITRD" >&2; exit 1; }

if [ -f "$MARKER" ] && grep -q "ro,tcp" "$INITRD" 2>/dev/null; then
    echo "  casper initrd already patched for NFS/TCP — nothing to do"
    exit 0
fi

# ── 1. Distro mirror integrity gate ───────────────────────────────────
# A partially-copied tree (interrupted rsync mid-install) boots PXE clients
# that die in squashfs errors after (not before) their 15-minute SSH window.
# casper/initrd is exempt: this helper patches it, so its md5 legitimately
# diverges from the pristine ISO after this script has run once.
if [ -f "$MIRROR_DIR/md5sum.txt" ]; then
    echo "  Verifying distro mirror integrity (md5sum -c)..."
    set +e
    md5out="$(cd "$MIRROR_DIR" && md5sum -c md5sum.txt 2>&1)"
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        # Only FAILED / missing entries matter (casper/initrd is exempt: this
        # helper patched it, so it legitimately diverges from the ISO).
        real_failures=$(printf '%s\n' "$md5out" | grep -E 'FAILED|No such file|does not exist' | grep -v 'casper/initrd' || true)
        if [ -n "$real_failures" ]; then
            printf '%s\n' "$real_failures" >&2
            echo "[ERROR] distro mirror corrupt/incomplete: re-export the ISO" >&2
            echo "        (mount + rsync) or verify the ISO checksum." >&2
            exit 1
        fi
    fi
else
    echo "  [WARN] $MIRROR_DIR/md5sum.txt missing: skipping integrity gate"
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
cp "$INITRD" "$WORK_DIR/orig.initrd"

# ── 2. Extract the shipped /scripts/casper ────────────────────────────
python3 - "$WORK_DIR" <<'PYEOF'
import os, subprocess, sys

work, initrd = sys.argv[1], os.path.join(sys.argv[1], "orig.initrd")
data = open(initrd, "rb").read()

def valid_header(buf, off):
    """Validate a candidate newc header structurally: names are short and
    NUL-terminated, sizes are bounded and the mode is octal. This keeps
    magic-scan false positives (raw file payload bytes that just happen to
    contain '070701') out."""
    if off + 110 > len(buf) or buf[off:off + 6] != b"070701":
        return False
    f = buf[off:off + 110]
    try:
        size = int(f[54:62], 16)
        nsize = int(f[94:102], 16)
    except ValueError:
        return False
    if not (1 <= nsize <= 4096) or size >= len(buf):
        return False
    nstart = off + 110
    nend = nstart + nsize
    if buf[nend - 1:nend] != b"\x00" or b"\x00" in buf[nstart:nend - 1]:
        return False
    name = buf[nstart:nend - 1]
    if any(b < 32 or b > 126 for b in name):
        return False
    return True


def walk_segment(buf, off):
    """Parse ONE uncompressed newc cpio segment from a validated header.
    Returns (entries, end) with end right after the TRAILER entry (padded),
    or (entries, None) on malformed input. GNU cpio may emit extra NUL
    padding between entries; it is tolerated here (the kernel tolerates it,
    otherwise this initrd would not boot at all)."""
    entries = []
    while off + 110 <= len(buf) and buf[off:off + 6] == b"070701":
        f = buf[off:off + 110]
        size = int(f[54:62], 16)
        nsize = int(f[94:102], 16)
        name = buf[off + 110:off + 110 + nsize - 1].decode("utf-8", "replace")
        doff = (off + 110 + nsize + 3) & ~3
        entries.append((name, doff, size))
        off = doff + ((size + 3) & ~3)
        limit = min(off + 8192, len(buf))
        while off < limit and buf[off:off + 1] == b"\x00" and \
                buf[off:off + 6] != b"070701":
            off += 1
        if name == "TRAILER!!!":
            return entries, off
    return entries, None


def extract_target(buf, tag, tail_start):
    """Walk every uncompressed segment (headers validated one by one) and
    return the target file, or None. tail_start is updated to just past the
    last parsed header/trailer before non-cpio data."""
    out = {}
    pos = 0
    while pos + 110 <= len(buf):
        if not valid_header(buf, pos):
            nxt = buf.find(b"070701", pos + 1)
            if nxt == -1:
                break
            pos = nxt
            if not valid_header(buf, pos):
                continue
        entries, end = walk_segment(buf, pos)
        if end is None:
            break
        for name, doff, size in entries:
            if name.lstrip("./") == tag:
                out[tag] = buf[doff:doff + size]
        pos = end
        while pos < len(buf) and buf[pos:pos + 1] == b"\x00":
            pos += 1
        if pos == end:
            # no NUL padding in between: a new header may start exactly at
            # `end`; break only when that is not the case
            if not valid_header(buf, pos):
                break
    tail_start[0] = pos
    return out.get(tag)


tail_start = [0]

# 1) try the uncompressed segment stream first
script = extract_target(data, "scripts/casper", tail_start)
if script is None:
    # 2) 24.04 ships the rootfs as a final zstd segment. Look for it in a
    #    bounded window after the last parsed segment: deeper candidates sit
    #    inside compressed data and every decompression attempt on them is
    #    wasted (or very slow for large slices).
    zi = data.find(b"\x28\xb5/\xfd", tail_start[0],
                   tail_start[0] + 64 * 1024)
    if zi == -1:
        sys.stderr.write("[ERROR] scripts/casper not in initrd and no "
                          "zstd tail segment within 64 KiB of the last "
                          "cpio segment\n")
        sys.exit(1)
    dec = subprocess.run(["zstd", "-dc"], input=data[zi:],
                         capture_output=True).stdout
    if len(dec) < 1024 * 1024:
        sys.stderr.write("[ERROR] zstd tail segment did not decompress a "
                          "rootfs-sized payload\n")
        sys.exit(1)
    script = extract_target(dec, "scripts/casper", tail_start)
    if script is None:
        sys.stderr.write("[ERROR] scripts/casper not found in zstd segment\n")
        sys.exit(1)

seg = os.path.join(work, "seg")
os.makedirs(os.path.join(seg, "scripts"), exist_ok=True)
open(os.path.join(seg, "scripts", "casper"), "wb").write(script)
print("  extracted /scripts/casper (%d bytes)" % len(script))
PYEOF

if ! grep -q "nfsmount" "$WORK_DIR/seg/scripts/casper"; then
    echo "[ERROR] nfsmount call not found in scripts/casper: casper" >&2
    echo "        layout changed, re-derive this patch." >&2
    exit 1
fi

# ── 3. Patch the mount line to TCP ─────────────────────────────────────
sed -i 's/nfsmount -o nolock -o ro$/nfsmount -o nolock -o ro,tcp/' \
    "$WORK_DIR/seg/scripts/casper"
sed -i 's/nfsmount -o nolock -o ro /nfsmount -o nolock -o ro,tcp /' \
    "$WORK_DIR/seg/scripts/casper"
grep -q "ro,tcp" "$WORK_DIR/seg/scripts/casper" || {
    echo "[ERROR] casper mount patch failed" >&2; exit 1; }

# ── 4. Append the override segment and mark ────────────────────────────
( cd "$WORK_DIR/seg" && find scripts | cpio -o -H newc --quiet ) > "$WORK_DIR/extra.cpio"
cp "$INITRD" "$CASPER_DIR/initrd.no-tcp.orig"
cat "$WORK_DIR/extra.cpio" >> "$INITRD"
touch "$MARKER"
echo "  [OK] casper initrd pushes NFS over TCP (ro,tcp);"
echo "       original at $CASPER_DIR/initrd.no-tcp.orig"
