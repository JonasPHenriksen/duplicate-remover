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
import tkinter.scrolledtext as st
from tkinter import ttk
from typing import Any

EXCLUDE_DIRS = {
    ".git", ".svn", ".hg", "node_modules",
    "__pycache__", ".venv", "venv",
    "proc", "dev", "sys", "run", "var"
}

ARCHIVE_EXTS = (".zip", ".tar", ".7z", ".rar")
CACHE_DB = "hash_cache.db"

def walk_filtered(root):
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        dirnames[:] = [d for d in dirnames if d not in EXCLUDE_DIRS]
        
        valid_files = []
        for f in filenames:
            path = os.path.join(dirpath, f)
            
            if not os.path.isfile(path) or os.path.islink(path):
                continue
                
            valid_files.append(f)
            
        yield dirpath, dirnames, valid_files

def auto_select_keep(paths, rule):
    if rule == "newest":
        return max(paths, key=os.path.getmtime)
    if rule == "oldest":
        return min(paths, key=os.path.getmtime)
    if rule == "shortest_path":
        return min(paths, key=len)
    return paths[0]

class ScanConfig:
    def __init__(self, rule, ask, dry_run, expand_archives):
        self.rule = rule
        self.ask = ask
        self.dry_run = dry_run
        self.expand_archives = expand_archives

class FileInfo:
    def __init__(self, path, size, hash):
        self.path = path
        self.size = size
        self.hash = hash

class FolderInfo:
    def __init__(self, path):
        self.path = path
        self.files = []       
        self.subfolders = []  
        self.signature = None

class Mapper:
    def __init__(self, cache, status_cb=None):
        self.cache = cache
        self.status_cb = status_cb or (lambda x: None)

        self.files_by_hash = {}
        self.folders_by_signature = {}
        self.path_to_folder = {}
        self.all_folders = []

    def map_tree(self, root):
        self._build_folder_tree(root)
        self._index_files()
        self._index_folders()

    def _build_folder_tree(self, root):
        for dirpath, dirnames, filenames in walk_filtered(root):
            
            self.status_cb(f"Scanning: {os.path.basename(dirpath)}...")
            
            folder = FolderInfo(dirpath)
            self.path_to_folder[dirpath] = folder
            self.all_folders.append(folder)

            for name in filenames:
                path = os.path.join(dirpath, name)
                try:
                    size = os.path.getsize(path)
                    hash = self.cache.hash_file(path)
                    folder.files.append(FileInfo(path, size, hash))
                except OSError:
                    continue

        for folder in self.all_folders:
            parent = os.path.dirname(folder.path)
            if parent in self.path_to_folder:
                self.path_to_folder[parent].subfolders.append(folder)
    
    def _index_files(self):
        for folder in self.all_folders:
            for f in folder.files:
                self.files_by_hash.setdefault(f.hash, []).append(f)
    
    def _folder_signature(self, folder):
        entries = []

        for root, _, files in os.walk(folder.path):
            for name in files:
                path = os.path.join(root, name)
                try:
                    size = os.path.getsize(path)
                    h = self.cache.hash_file(path)
                    rel = os.path.relpath(path, folder.path)
                    entries.append((rel, h, size))
                except OSError:
                    continue

        entries.sort()
        total_size = sum(e[2] for e in entries)

        return (len(entries), total_size, tuple(entries))
    
    def _index_folders(self):
        sorted_folders = sorted(
            self.all_folders, 
            key=lambda f: f.path.count(os.sep), 
            reverse=True
        )

        for folder in sorted_folders:
            folder.signature = self._calculate_in_memory_signature(folder)
            
            self.folders_by_signature.setdefault(folder.signature, []).append(folder)

    def _calculate_in_memory_signature(self, folder):

        all_entries = []
        
        def collect_files(current_folder):
            for f in current_folder.files:
                rel = os.path.relpath(f.path, folder.path)
                all_entries.append((rel, f.hash, f.size))
            
            for sub in current_folder.subfolders:
                collect_files(sub)

        collect_files(folder)

        all_entries.sort()
        
        total_size = sum(e[2] for e in all_entries)
        file_count = len(all_entries)
        
        return (file_count, total_size, tuple(all_entries))
        
class HashCache:
    def __init__(self, db_path=CACHE_DB, status_cb=None):
        self.status_cb = status_cb or (lambda x: None)
        self.conn = sqlite3.connect(db_path, check_same_thread=False)
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

        self.status_cb(f"Hashing: {os.path.basename(path)}...")

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

class Deduplicator:
    def __init__(self, cache, config, status_cb=None):
        self.cache = cache
        self.config = config
        self.status_cb = status_cb or (lambda x: None)
        self.resolved_paths = set()

    def _status(self, text):
        self.status_cb(text)

    def _mark_resolved(self, folder):
        for root, _, files in os.walk(folder.path):
            self.resolved_paths.add(root)
            for f in files:
                self.resolved_paths.add(os.path.join(root, f))

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
                    
                    base_dest = os.path.join(os.path.dirname(path), os.path.splitext(os.path.basename(path))[0])
                    dest = base_dest
                    counter = 1
                    
                    while os.path.exists(dest):
                        dest = f"{base_dest}_{counter}"
                        counter += 1
                    
                    os.makedirs(dest, exist_ok=True)
                    self._unpack(path, dest)
                    self._status(f"Unpacked: {os.path.basename(path)}")
                
                current_depth += 1

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

    def resolve_folders_top_down(self, mapper, ask_fn, dest):
        folders = sorted(
            mapper.all_folders,
            key=lambda f: f.path.count(os.sep)
        )

        for folder in folders:
            if folder.path in self.resolved_paths:
                continue

            group = mapper.folders_by_signature.get(folder.signature, [])
            if len(group) < 2:
                continue

            keep = ask_fn([f.path for f in group], is_folder=True)
            if not keep:
                continue

            for f in group:
                self.resolved_paths.add(f.path)
                if f.path != keep:
                    self._mark_resolved(f)
                    self._remove_path(f.path, dest)

    def resolve_loose_files(self, mapper, ask_fn, dest):
        for hash, files in mapper.files_by_hash.items():
            loose = [
                f for f in files
                if f.path not in self.resolved_paths
            ]

            if len(loose) < 2:
                continue

            keep = ask_fn([f.path for f in loose], is_folder=False)
            if not keep:
                continue

            for f in loose:
                if f.path != keep:
                    self._remove_path(f.path, dest)
    
    def _remove_path(self, path, dest):
        
        if self.config.dry_run:
            print("[DRY RUN]", path)
            return
        
        action = "[DRY RUN]" if self.config.dry_run else "[MOVING]"

        self.status_cb(f"{action} {os.path.basename(path)}")
        
        base = os.path.basename(path)
        target = os.path.join(dest, base)

        counter = 1
        name, ext = os.path.splitext(base)
        while os.path.exists(target):
            target = os.path.join(dest, f"{name}_{counter}{ext}")
            counter += 1

        try:
            shutil.move(path, target)
        except Exception as e:
            self.status_cb(f"[ERROR] Could not move {path}: {e}")
            send2trash(path)

class CleanupApp:
    def __init__(self, root):
        self.root = root
        root.title("Universal Deduplicator")
        root.geometry("600x500")

        opt_frame = tk.Frame(root)
        opt_frame.pack(fill="x", padx=10, pady=5)

        self.ask_var = tk.BooleanVar(value=True)
        self.expand_var = tk.BooleanVar(value=True)
        self.dry_var = tk.BooleanVar(value=False)
        self.rule_var = tk.StringVar(value="newest")

        tk.Checkbutton(opt_frame, text="Expand archives", variable=self.expand_var).grid(row=0, column=0, sticky="w")
        tk.Checkbutton(opt_frame, text="Ask before removing", variable=self.ask_var).grid(row=1, column=0, sticky="w")
        tk.Checkbutton(opt_frame, text="Dry run (Log only)", variable=self.dry_var).grid(row=0, column=1, sticky="w")

        tk.Label(opt_frame, text="Keep:").grid(row=1, column=1, sticky="e")
        tk.OptionMenu(opt_frame, self.rule_var, "newest", "oldest", "shortest_path").grid(row=1, column=2, sticky="w")

        self.progress = ttk.Progressbar(root, orient="horizontal", length=100, mode="determinate")
        self.progress.pack(fill="x", padx=10, pady=5)

        self.status = tk.Label(root, text="Ready", fg="blue")
        self.status.pack(padx=10)

        tk.Label(root, text="Activity Log:").pack(anchor="w", padx=10)
        self.log_area = st.ScrolledText(root, height=15, font=("Consolas", 9))
        self.log_area.pack(fill="both", expand=True, padx=10, pady=5)

        self.is_running = False
        self.btn_run = tk.Button(root, text="Start Cleanup", command=self.run, bg="#e1e1e1")
        self.btn_run.pack(pady=10)

    def log(self, message):
        """Thread-safe logging to the UI."""
        def append():
            self.log_area.insert(tk.END, f"{message}\n")
            self.log_area.see(tk.END) 
        self.root.after(0, append)

    def update_status(self, text, progress_val=None):
        def update():
            self.status.config(text=text)
            if progress_val is not None:
                self.progress["value"] = progress_val
        self.root.after(0, update)

    def ask_user(self, paths, is_folder):

        result_container: list[Any] = [None] 

        def show_dialog():
            title = "Duplicate Found"
            msg = "Pick one to KEEP (Enter number, or 0 to skip):\n\n"
            msg += "\n".join(f"[{i+1}] {p}" for i, p in enumerate(paths))
            res = simpledialog.askstring(title, msg, parent=self.root)
            result_container[0] = res if res is not None else "0"

        self.root.after(0, show_dialog)
        while result_container[0] is None:
            time.sleep(0.1)
        
        choice = result_container[0]
        if choice == "0": return None
        try:
            idx = int(choice) - 1
            return paths[idx] if 0 <= idx < len(paths) else None
        except: return None

    def run(self):
        if self.is_running: return

        src = filedialog.askdirectory(title="Select source folder")
        dst = filedialog.askdirectory(title="Select destination folder")
        if not src or not dst: return

        self.is_running = True
        self.btn_run.config(state=tk.DISABLED)
        self.log_area.delete('1.0', tk.END) 

        def task():
            cache = None
            try:
                config = ScanConfig(self.rule_var.get(), self.ask_var.get(), self.dry_var.get(), self.expand_var.get())
                
                self.log("--- Starting Session ---")
                self.log(f"Source: {src}")
                
                cache = HashCache(status_cb=self.log)
                dedup = Deduplicator(cache, config, self.log) 

                if config.expand_archives:
                    self.update_status("Expanding archives...", 10)
                    dedup.expand_archives(src)
                
                self.update_status("Mapping files and hashing...", 30)
                mapper = Mapper(cache, self.log)
                mapper.map_tree(src)

                self.update_status("Comparing folder signatures...", 60)
                mapper._index_folders() 

                self.log(">>> Resolving Duplicates...")
                dedup.resolve_folders_top_down(mapper, self.ask_user, dst)
                dedup.resolve_loose_files(mapper, self.ask_user, dst)

                self.update_status("Done!", 100)
                self.log("--- FINISHED ---")
                messagebox.showinfo("Done", "Cleanup completed successfully.")

            except Exception as e:
                self.log(f"ERROR: {str(e)}")
            finally:
                if cache is not None:
                    cache.close()
                self.is_running = False
                self.root.after(0, lambda: self.btn_run.config(state=tk.NORMAL))

        threading.Thread(target=task, daemon=True).start()

if __name__ == "__main__":
    root = tk.Tk()
    app = CleanupApp(root)
    root.mainloop()
