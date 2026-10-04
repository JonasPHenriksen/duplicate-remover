#!/usr/bin/env python3
"""
photo_cleanup.py - quarantine duplicate photos/videos and non-media files.

Folder structure is never changed. Nothing is deleted. Nothing is renamed.
Everything removed is moved into <folder>/_Quarantine/ under the exact same
relative path it came from, so you can review it and put it back.

Usage:
    python3 photo_cleanup.py /path/to/photos            # look only, change nothing
    python3 photo_cleanup.py /path/to/photos --apply    # move files to _Quarantine
    python3 photo_cleanup.py /path/to/photos --undo     # put everything back

Needs nothing but Python 3.8+.
"""

import hashlib
import json
import os
import shutil
import sys
from collections import defaultdict
from pathlib import Path

# ---------------------------------------------------------------------------
# Settings - edit these if you need to
# ---------------------------------------------------------------------------

# Anything whose extension is not in here is treated as "not a photo or video". Includes pdf as it can also be used as a photo.
MEDIA_EXT = {
    # photos
    "jpg", "jpeg", "jpe", "jfif", "png", "gif", "bmp", "dib", "tif", "tiff", "webp",
    "heic", "heif", "hif", "avif", "jxl", "jp2", "j2k", "jpf", "jpx", "tga", "pcx",
    "ppm", "pgm", "pbm", "pnm", "psd", "psb", "xcf", "svg", "pdf",
    # camera raw
    "dng", "cr2", "cr3", "crw", "nef", "nrw", "arw", "srf", "sr2", "raf", "orf",
    "rw2", "raw", "pef", "ptx", "srw", "x3f", "erf", "mef", "mos", "iiq", "3fr",
    "fff", "kdc", "dcr", "k25", "mrw", "rwl", "rwz", "gpr", "ari", "cap", "eip",
    # videos
    "mp4", "m4v", "mov", "qt", "avi", "mkv", "webm", "wmv", "asf", "flv", "f4v",
    "mpg", "mpeg", "mpe", "m1v", "m2v", "mts", "m2ts", "ts", "mod", "tod", "vob",
    "3gp", "3g2", "mxf", "ogv", "ogm", "rm", "rmvb", "divx", "dv", "mjpg", "mjpeg",
    "insv", "lrv", "360", "braw", "r3d", "amv", "svi",
}

QUARANTINE = "_Quarantine"
DUP_DIR = "duplicates"        # _Quarantine/duplicates/<original path>
OTHER_DIR = "not-media"       # _Quarantine/not-media/<original path>

# Folders that look like a phone dump or a backup rather than a curated album.
# When the same photo exists in one of these and also elsewhere, the copy in the
# curated folder is the one that stays.
DISPOSABLE_PARTS = {
    "backup", "backups", "dcim", "camera", "camera roll", "camera uploads",
    "unsorted", "usorteret", "downloads", "download", "temp", "tmp", "new folder",
    "recovered", "recovery", "restored", "old phone", "gammel telefon", "import",
    "imported", "copy", "copies", "kopi", "til sortering", "diverse", "misc",
    "whatsapp", "whatsapp images", "whatsapp video", "telegram", "screenshots",
    "100andro", "100apple", "100canon", "100nikon", "100media", "100msdcf", "101apple",
}

# Filename markers that suggest a copy rather than the original.
COPY_MARKERS = ("copy", "kopi", "copia", "duplicate", "dublet", "(1)", "(2)", "(3)",
                "(4)", "(5)", " - kopi", "-copy", "_copy")

READ_CHUNK = 1 << 20          # 1 MB


# ---------------------------------------------------------------------------

def is_media(path):
    return path.suffix.lower().lstrip(".") in MEDIA_EXT


def quick_fingerprint(path, size):
    """Hash of the first and last 64 KB. Cheap way to rule files out."""
    h = hashlib.blake2b(digest_size=16)
    h.update(str(size).encode())
    with open(path, "rb") as fh:
        h.update(fh.read(65536))
        if size > 131072:
            fh.seek(-65536, os.SEEK_END)
            h.update(fh.read(65536))
    return h.hexdigest()


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        while True:
            block = fh.read(READ_CHUNK)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def scan(root):
    """Walk the folder once. Returns (media, other, skipped_count)."""
    media, other = [], []
    skipped = 0
    quarantine = root / QUARANTINE
    for dirpath, dirnames, filenames in os.walk(root):
        here = Path(dirpath)
        if here == quarantine or quarantine in here.parents:
            dirnames[:] = []
            continue
        dirnames.sort()
        for name in sorted(filenames):
            p = here / name
            if p.is_symlink():          # never follow or move links
                skipped += 1
                continue
            try:
                st = p.stat()
            except OSError:
                skipped += 1
                continue
            if not p.is_file():
                continue
            entry = (p, st.st_size, st.st_dev, st.st_ino, st.st_mtime)
            if is_media(p) and st.st_size > 0:
                media.append(entry)
            else:
                other.append(entry)
    return media, other, skipped


def keeper_rank(entry):
    """Lower sorts first, so the first item of a sorted group is the keeper."""
    path = entry[0]
    parts = [part.lower() for part in path.parts[:-1]]
    in_dump = any(part in DISPOSABLE_PARTS for part in parts)
    name = path.stem.lower()
    looks_copied = any(marker in name for marker in COPY_MARKERS)
    return (in_dump,              # curated folder beats a phone dump / backup
            looks_copied,         # "IMG_1234" beats "IMG_1234 (1)"
            -len(path.parts),     # a named album is usually deeper than the root
            entry[4],             # older file first: likely the original
            str(path).lower())    # stable, so runs are repeatable


def why_kept(keeper, loser):
    k, l = keeper_rank(keeper), keeper_rank(loser)
    if k[0] != l[0]:
        return "other copy sits in a backup/DCIM folder"
    if k[1] != l[1]:
        return "other copy is named like a copy"
    if k[2] != l[2]:
        return "this copy is in a more specific folder"
    if k[3] != l[3]:
        return "this copy is older"
    return "identical in every way, first by name"


def find_duplicates(media):
    """Group byte-identical files. Returns list of (keeper, [duplicates])."""
    by_size = defaultdict(list)
    for entry in media:
        by_size[entry[1]].append(entry)

    same_size = [g for g in by_size.values() if len(g) > 1]
    total = sum(len(g) for g in same_size)
    print(f"  {total} files share a size with another file; fingerprinting those")

    by_quick = defaultdict(list)
    for group in same_size:
        for entry in group:
            try:
                fp = quick_fingerprint(entry[0], entry[1])
            except OSError as e:
                print(f"  ! cannot read {entry[0]}: {e}", file=sys.stderr)
                continue
            by_quick[(entry[1], fp)].append(entry)

    by_hash = defaultdict(list)
    candidates = [g for g in by_quick.values() if len(g) > 1]
    total = sum(len(g) for g in candidates)
    done = 0
    for group in candidates:
        for entry in group:
            try:
                by_hash[sha256(entry[0])].append(entry)
            except OSError as e:
                print(f"  ! cannot read {entry[0]}: {e}", file=sys.stderr)
            done += 1
            if done % 500 == 0:
                print(f"  hashed {done}/{total}")

    results = []
    for group in by_hash.values():
        if len(group) < 2:
            continue
        # Hardlinks to one another are already a single file on disk: leave alone.
        seen_inodes = set()
        unique = []
        for entry in sorted(group, key=keeper_rank):
            key = (entry[2], entry[3])
            if key in seen_inodes:
                continue
            seen_inodes.add(key)
            unique.append(entry)
        if len(unique) > 1:
            results.append((unique[0], unique[1:]))
    return results


def plan_moves(root, duplicates, other):
    """Return list of dicts describing every intended move."""
    moves = []
    for keeper, losers in duplicates:
        for entry in losers:
            rel = entry[0].relative_to(root)
            moves.append({
                "from": str(entry[0]),
                "to": str(root / QUARANTINE / DUP_DIR / rel),
                "why": "duplicate of " + str(keeper[0].relative_to(root)),
                "kind": "duplicate",
                "size": entry[1],
            })
    for entry in other:
        rel = entry[0].relative_to(root)
        moves.append({
            "from": str(entry[0]),
            "to": str(root / QUARANTINE / OTHER_DIR / rel),
            "why": "empty file" if entry[1] == 0 else "not a photo or video",
            "kind": "not-media",
            "size": entry[1],
        })
    return moves


def write_reports(root, moves, duplicates, other, skipped, applied):
    out = root / QUARANTINE
    out.mkdir(exist_ok=True)

    with open(out / "duplicates.csv", "w", encoding="utf-8", newline="") as fh:
        fh.write("group,role,path,size,reason\n")
        for i, (keeper, losers) in enumerate(duplicates, 1):
            fh.write(csv_row([i, "KEPT", str(keeper[0].relative_to(root)),
                              keeper[1], why_kept(keeper, losers[0])]))
            for entry in losers:
                fh.write(csv_row([i, "quarantined", str(entry[0].relative_to(root)),
                                  entry[1], "byte-for-byte identical"]))

    with open(out / "not-media.csv", "w", encoding="utf-8", newline="") as fh:
        fh.write("path,size,extension\n")
        for entry in other:
            ext = entry[0].suffix.lower().lstrip(".") or "(none)"
            fh.write(csv_row([str(entry[0].relative_to(root)), entry[1], ext]))

    dup_files = sum(len(l) for _, l in duplicates)
    dup_bytes = sum(e[1] for _, losers in duplicates for e in losers)
    other_bytes = sum(e[1] for e in other)
    by_ext = defaultdict(int)
    for entry in other:
        by_ext[entry[0].suffix.lower().lstrip(".") or "(no extension)"] += 1

    lines = [
        "photo_cleanup summary",
        "=" * 60,
        f"folder            : {root}",
        f"mode              : {'APPLIED' if applied else 'preview only, nothing moved'}",
        "",
        f"duplicate groups  : {len(duplicates)}",
        f"duplicate files   : {dup_files}  ({human(dup_bytes)})",
        f"non-media files   : {len(other)}  ({human(other_bytes)})",
        f"total to move     : {len(moves)}  ({human(dup_bytes + other_bytes)})",
        f"skipped (links)   : {skipped}",
        "",
        "non-media by type:",
    ]
    for ext, count in sorted(by_ext.items(), key=lambda kv: -kv[1]):
        lines.append(f"   {ext:<14} {count}")
    lines += [
        "",
        "Detail: duplicates.csv, not-media.csv",
        "",
        "Everything moved keeps its original relative path under _Quarantine/,",
        "so you can drag a whole subfolder back, or restore everything with:",
        f"   python3 {Path(sys.argv[0]).name} {shell_quote(str(root))} --undo",
    ]
    (out / "summary.txt").write_text("\n".join(lines) + "\n", encoding="utf-8")
    return "\n".join(lines)


def csv_row(values):
    cells = []
    for v in values:
        s = str(v)
        if any(c in s for c in ',"\n'):
            s = '"' + s.replace('"', '""') + '"'
        cells.append(s)
    return ",".join(cells) + "\n"


def shell_quote(s):
    return "'" + s.replace("'", "'\\''") + "'" if " " in s else s


def human(n):
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if abs(n) < 1024 or unit == "TB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n /= 1024.0


def do_moves(root, moves):
    journal = root / QUARANTINE / "journal.jsonl"
    journal.parent.mkdir(exist_ok=True)
    moved = failed = 0
    with open(journal, "a", encoding="utf-8") as jf:
        for move in moves:
            src, dst = Path(move["from"]), Path(move["to"])
            try:
                dst.parent.mkdir(parents=True, exist_ok=True)
                if dst.exists():
                    dst = uniquify(dst)
                shutil.move(str(src), str(dst))
            except Exception as e:
                print(f"  ! could not move {src}: {e}", file=sys.stderr)
                failed += 1
                continue
            jf.write(json.dumps({"from": str(src), "to": str(dst)}) + "\n")
            moved += 1
            if moved % 500 == 0:
                print(f"  moved {moved}/{len(moves)}")
    return moved, failed


def uniquify(path):
    i = 2
    while True:
        candidate = path.with_name(f"{path.stem}~{i}{path.suffix}")
        if not candidate.exists():
            return candidate
        i += 1


def undo(root):
    journal = root / QUARANTINE / "journal.jsonl"
    if not journal.exists():
        print(f"Nothing to undo: {journal} not found.")
        return 1
    entries = []
    with open(journal, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                try:
                    entries.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
    restored = missing = blocked = 0
    for e in reversed(entries):
        src, dst = Path(e["from"]), Path(e["to"])
        if not dst.exists():
            missing += 1
            continue
        if src.exists():
            print(f"  ! {src} exists again, leaving {dst} in quarantine", file=sys.stderr)
            blocked += 1
            continue
        try:
            src.parent.mkdir(parents=True, exist_ok=True)
            shutil.move(str(dst), str(src))
            restored += 1
        except Exception as ex:
            print(f"  ! could not restore {dst}: {ex}", file=sys.stderr)
            blocked += 1
    # clean up the now-empty quarantine skeleton, keeping the reports
    for dirpath, _dirnames, _filenames in os.walk(root / QUARANTINE, topdown=False):
        try:
            if not os.listdir(dirpath):      # re-check: children may just have been removed
                os.rmdir(dirpath)
        except OSError:
            pass
    if restored:
        journal.unlink(missing_ok=True)
    print(f"\nrestored {restored}, already gone {missing}, left alone {blocked}")
    if blocked or missing:
        print("Check _Quarantine/ for anything that could not be put back.")
    return 0


def main(argv):
    args = [a for a in argv[1:]]
    apply_it = "--apply" in args
    undo_it = "--undo" in args
    args = [a for a in args if not a.startswith("--")]
    if len(args) != 1 or (apply_it and undo_it):
        print(__doc__.strip())
        return 2

    root = Path(args[0]).expanduser().resolve()
    if not root.is_dir():
        print(f"Not a folder: {root}", file=sys.stderr)
        return 2

    if undo_it:
        return undo(root)

    print(f"Scanning {root}")
    media, other, skipped = scan(root)
    print(f"  {len(media)} photos/videos, {len(other)} other files")
    if skipped:
        print(f"  {skipped} symlinks or unreadable entries skipped")

    print("Looking for duplicates")
    duplicates = find_duplicates(media)
    moves = plan_moves(root, duplicates, other)

    if apply_it and moves:
        print(f"Moving {len(moves)} files to {QUARANTINE}/")
        moved, failed = do_moves(root, moves)
        print(f"  moved {moved}" + (f", {failed} failed" if failed else ""))

    summary = write_reports(root, moves, duplicates, other, skipped, apply_it and moves)
    print()
    print(summary)
    if not apply_it:
        print()
        print("Nothing was moved. Re-run with --apply when the lists look right.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except KeyboardInterrupt:
        print("\ninterrupted", file=sys.stderr)
        sys.exit(130)
