import os
import hashlib
import shutil
import sqlite3
import threading
import time
import tkinter as tk
from tkinter import filedialog, messagebox, simpledialog
from send2trash import send2trash
import zipfile, tarfile, py7zr, rarfile

# -------------------- config --------------------

EXCLUDE_DIRS = {
    ".git", ".svn", ".hg", "node_modules",
    "__pycache__", ".venv", "venv",
}

ARCHIVE_EXTS = (".zip", ".tar", ".7z", ".rar")
CACHE_DB = "hash_cache.db"

# -------------------- helpers --------------------

def walk_filtered(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in EXCLUDE_DIRS]
        yield dirpath, dirnames, filenames

def auto_select_keep(paths, rule):
    if rule == "newest":
        return max(paths, key=os.path.getmtime)
    if rule == "oldest":
        return min(paths, key=os.path.getmtime)
    if rule == "shortest_path":
        return min(paths, key=len)
    return paths[0]

# -------------------- config object --------------------

class ScanConfig:
    def __init__(self, rule, ask, dry_run, expand_archives):
        self.rule = rule
        self.ask = ask
        self.dry_run = dry_run
        self.expand_archives = expand_archives

# -------------------- hash cache --------------------

class HashCache:
    def __init__(self, db_path=CACHE_DB):
        self.conn = sqlite3.connect(db_path)
        self._init_db()

    def _init_db(self):
        cur = self.conn.cursor()
        cur.execute("""
            CREATE TABLE IF NOT EXISTS file_hashes (
                path TEXT PRIMARY KEY,
                size INTEGER,
                mtime REAL,
                hash TEXT
            )
        """)
        self.conn.commit()

    def hash_file(self, path):
        size = os.path.getsize(path)
        mtime = os.path.getmtime(path)

        cur = self.conn.cursor()
        cur.execute("SELECT hash, size, mtime FROM file_hashes WHERE path=?", (path,))
        row = cur.fetchone()

        if row and row[1] == size and row[2] == mtime:
            return row[0]

        h = hashlib.md5()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(8192), b""):
                h.update(chunk)

        digest = h.hexdigest()
        cur.execute(
            "REPLACE INTO file_hashes VALUES (?, ?, ?, ?)",
            (path, size, mtime, digest),
        )
        self.conn.commit()
        return digest

    def close(self):
        self.conn.close()

# -------------------- dedup engine --------------------

class Deduplicator:
    def __init__(self, cache, config, status_cb=None):
        self.cache = cache
        self.config = config
        self.status_cb = status_cb or (lambda x: None)

    def _status(self, text):
        self.status_cb(text)

    def expand_archives(self, root, max_depth=5):
            processed_paths = set()
            current_depth = 0
            
            while current_depth < max_depth:
                archives_to_unpack = []
                for dirpath, _, filenames in walk_filtered(root):
                    for name in filenames:
                        path = os.path.join(dirpath, name)
                        if path.lower().endswith(ARCHIVE_EXTS) and path not in processed_paths:
                            archives_to_unpack.append(path)
                
                if not archives_to_unpack:
                    break
                    
                for path in archives_to_unpack:
                    processed_paths.add(path)
                    dest = os.path.join(os.path.dirname(path), os.path.splitext(os.path.basename(path))[0])
                    os.makedirs(dest, exist_ok=True)
                    self._unpack(path, dest)
                
                current_depth += 1
            
            if current_depth == max_depth:
                self._status("Reached maximum archive depth. Stopping extraction.")

    def _unpack(self, path, dest):
        ext = os.path.splitext(path)[1].lower()
        try:
            if ext == ".zip":
                with zipfile.ZipFile(path) as z: z.extractall(dest)
            elif ext == ".tar":
                with tarfile.open(path) as t: t.extractall(dest)
            elif ext == ".7z":
                with py7zr.SevenZipFile(path) as z: z.extractall(dest)
            elif ext == ".rar":
                with rarfile.RarFile(path) as r: r.extractall(dest)
            send2trash(path)
        except Exception:
            pass

    def find_duplicate_folders(self, root):
        seen = {}
        duplicates = []

        for dirpath, dirnames, _ in walk_filtered(root):
            for d in dirnames:
                full = os.path.join(dirpath, d)
                sig = self._folder_signature(full)
                if sig in seen:
                    duplicates.append([seen[sig], full])
                else:
                    seen[sig] = full
        return duplicates

    def _folder_signature(self, folder):
        total_size = 0
        count = 0
        structure = []

        for root, _, files in os.walk(folder):
            for f in files:
                path = os.path.join(root, f)
                try:
                    size = os.path.getsize(path)
                except OSError:
                    continue
                total_size += size
                count += 1
                structure.append(os.path.relpath(path, folder))

        structure.sort()
        return (os.path.basename(folder), count, total_size, tuple(structure))

    def find_duplicate_files(self, root):
        by_size = {}

        for dirpath, _, filenames in walk_filtered(root):
            for name in filenames:
                path = os.path.join(dirpath, name)
                try:
                    size = os.path.getsize(path)
                except OSError:
                    continue
                by_size.setdefault(size, []).append(path)

        duplicates = []
        for files in by_size.values():
            if len(files) < 2:
                continue
            hashes = {}
            for f in files:
                h = self.cache.hash_file(f)
                hashes.setdefault(h, []).append(f)
            duplicates.extend(g for g in hashes.values() if len(g) > 1)

        return duplicates

    def resolve(self, groups, dest, ask_fn, is_folder=False):
        for group in groups:
            if self.config.ask:
                keep = ask_fn(group, is_folder)
                if not keep:
                    continue
            else:
                keep = auto_select_keep(group, self.config.rule)

            for p in group:
                if p == keep:
                    continue
                if self.config.dry_run:
                    print("[DRY RUN]", p)
                    continue
                
                # IMPROVED LOGIC: Handle name collisions in Destination
                base_name = os.path.basename(p)
                final_dst = os.path.join(dest, base_name)
                
                counter = 1
                name, ext = os.path.splitext(base_name)
                while os.path.exists(final_dst):
                    final_dst = os.path.join(dest, f"{name}_{counter}{ext}")
                    counter += 1

                try:
                    shutil.move(p, final_dst)
                except Exception as e:
                    try:
                        send2trash(p)
                    except Exception as trash_err:
                        print(f"[SKIP] Could not move or trash {p}: {trash_err}")
                        continue

# -------------------- UI --------------------

class CleanupApp:
    def __init__(self, root):
        self.root = root
        root.title("Universal Deduplicator")
        root.geometry("420x300")

        self.ask_var = tk.BooleanVar()
        self.expand_var = tk.BooleanVar(value=True)
        self.dry_var = tk.BooleanVar()
        self.rule_var = tk.StringVar(value="newest")

        tk.Checkbutton(root, text="Expand archives", variable=self.expand_var).pack(anchor="w")
        tk.Checkbutton(root, text="Ask before removing", variable=self.ask_var).pack(anchor="w")
        tk.Checkbutton(root, text="Dry run", variable=self.dry_var).pack(anchor="w")

        tk.Label(root, text="Auto-keep rule").pack(anchor="w")
        tk.OptionMenu(root, self.rule_var, "newest", "oldest", "shortest_path").pack(anchor="w")

        tk.Button(root, text="Start cleanup", command=self.start).pack(pady=10)

        self.status = tk.Label(root, text="Idle", anchor="w")
        self.status.pack(fill="x", pady=5)

    def update_status(self, text):
        self.root.after(0, lambda: self.status.config(text=text))

    def ask_user(self, paths, is_folder):
        title = "Duplicate folders" if is_folder else "Duplicate files"
        text = "\n".join(f"{i+1}: {p}" for i, p in enumerate(paths))
        choice = simpledialog.askstring(
            title,
            f"{title} found:\n\n{text}\n\nEnter number to keep, or 0 to skip:"
        )
        if not choice or choice == "0":
            return None
        try:
            idx = int(choice) - 1
            if 0 <= idx < len(paths):
                return paths[idx]
        except ValueError:
            pass
        return None

    def start(self):
        threading.Thread(target=self.run).start()

    def run(self):
        src = filedialog.askdirectory(title="Select source folder")
        dst = filedialog.askdirectory(title="Select destination folder")
        if not src or not dst:
            return

        config = ScanConfig(
            rule=self.rule_var.get(),
            ask=self.ask_var.get(),
            dry_run=self.dry_var.get(),
            expand_archives=self.expand_var.get(),
        )

        cache = HashCache()
        dedup = Deduplicator(cache, config, self.update_status)

        start = time.time()

        if config.expand_archives:
            self.update_status("Expanding archives...")
            dedup.expand_archives(src)

        self.update_status("Finding duplicate folders...")
        folders = dedup.find_duplicate_folders(src)
        dedup.resolve(folders, dst, self.ask_user, is_folder=True)

        self.update_status("Finding duplicate files...")
        files = dedup.find_duplicate_files(src)
        dedup.resolve(files, dst, self.ask_user, is_folder=False)

        cache.close()

        elapsed = time.time() - start
        self.update_status(f"Done in {elapsed:.1f}s")
        messagebox.showinfo("Finished", "Cleanup completed")

# -------------------- run --------------------

if __name__ == "__main__":
    root = tk.Tk()
    app = CleanupApp(root)
    root.mainloop()
