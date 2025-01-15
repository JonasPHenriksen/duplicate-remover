import os
import hashlib
import shutil
import py7zr
import rarfile
import zipfile
import tarfile
import time
import tkinter as tk
import threading
from tkinter import filedialog, messagebox
from send2trash import send2trash
import json

def is_archive(file_path):
    return (
        file_path.lower().endswith(('.rar', '.7z', '.zip', '.tar'))    
    )

def unpack_archive(file_path, extract_to):
    try:

        file_path = file_path.strip()  
        file_path = os.path.abspath(file_path)  
        extract_to = os.path.abspath(extract_to)  

        if zipfile.is_zipfile(file_path):
            with zipfile.ZipFile(file_path, 'r') as archive:
                archive.extractall(extract_to)
        elif tarfile.is_tarfile(file_path):
            with tarfile.open(file_path, 'r') as archive:
                archive.extractall(extract_to)
        elif file_path.lower().endswith('.7z'):
            with py7zr.SevenZipFile(file_path, mode='r') as archive:
                archive.extractall(extract_to)
        elif file_path.lower().endswith('.rar'):
            with rarfile.RarFile(file_path, 'r') as archive:
                archive.extractall(extract_to)
        return True
    
    except Exception as e:
        log_error(f"Failed to unpack archive: {file_path}\n{str(e)}")
        return False


def log_error(error_message):
    messagebox.showerror("Error", error_message)
        
    script_directory = os.path.dirname(os.path.realpath(__file__))  
    log_file = os.path.join(script_directory, 'error_log.txt')

    with open(log_file, 'a') as f:
        f.write(f"{error_message}\n") 


def hash_file(file_path):
    hash_algo = hashlib.md5()
    with open(file_path, 'rb') as f:
        while chunk := f.read(8192):
            hash_algo.update(chunk)
    return hash_algo.hexdigest()

def update_status(processed_files, total_files, start_time, status_label):
    percentage = (processed_files / total_files) * 100
    elapsed_time = time.time() - start_time
    estimated_total_time = (elapsed_time / processed_files) * total_files if processed_files > 0 else 0
    remaining_time = max(estimated_total_time - elapsed_time, 0)

    status_text = (f"Progress: {percentage:.2f}% | "
                   f"Elapsed Time: {time.strftime('%H:%M:%S', time.gmtime(elapsed_time))} | "
                   f"Time Left: {time.strftime('%H:%M:%S', time.gmtime(remaining_time))}")
    status_label.config(text=status_text)

def save_seen_files(seen_files):

    script_directory = os.path.dirname(os.path.realpath(__file__))  
    seen_file = os.path.join(script_directory, 'seen_files.txt')
    
    try:
        with open(seen_file, 'w') as f:
            for file_hash, file_path in seen_files.items():
                f.write(f"Hash: {file_hash} | Path: {file_path}\n")
        print(f"Seen files written to {seen_file}")
    except Exception as e:
        print(f"Error saving seen files: {str(e)}")

def find_duplicates_and_move_non_media(root_folder, dest_folder, unsupported_folder, extensions, status_label):

    def extract_all_files():
     for dirpath, _, filenames in os.walk(root_folder):
        for filename in filenames:
            file_path = os.path.join(dirpath, filename)
            try:
                if is_archive(file_path):
                    nested_extract_to = os.path.join(root_folder, os.path.splitext(filename)[0])
                    if not os.path.exists(nested_extract_to):
                        os.makedirs(nested_extract_to)

                    status_label.config(text=f"Currently extracting: {file_path}")
                    app.update_idletasks()

                    unpack_archive(file_path, nested_extract_to)
                    extract_all_files_in_folder(nested_extract_to)

            except Exception as e:
                log_error(f"Error processing file: {file_path}\n{str(e)}")
                continue

    def extract_all_files_in_folder(folder):
      for dirpath, _, filenames in os.walk(folder):
        for filename in filenames:
            file_path = os.path.join(dirpath, filename)
            if is_archive(file_path):
                nested_extract_to = os.path.join(folder, os.path.splitext(filename)[0])
                if not os.path.exists(nested_extract_to):
                    os.makedirs(nested_extract_to)

                status_label.config(text=f"Currently extracting: {file_path}")
                app.update_idletasks()

                unpack_archive(file_path, nested_extract_to)

    def move_duplicates():
        processed_files = 0
        seen_files = {}
        start_time = time.time()
        total_files = sum([len(filenames) for _, _, filenames in os.walk(root_folder)])
        threshold_size = int(size_entry.get()) * 1024
        for dirpath, _, filenames in os.walk(root_folder):
            for filename in filenames:
                file_path = os.path.join(dirpath, filename)
                try:
                    if extensions is None or filename.lower().endswith(extensions):
                        file_hash = hash_file(file_path)
                        if threshold_size <= os.path.getsize(file_path) and file_hash in seen_files and os.path.basename(file_path) == os.path.basename(seen_files[file_hash]):
                            shutil.copy(file_path, os.path.join(dest_folder, filename))
                        else:
                            seen_files[file_hash] = file_path
                    else:
                        shutil.copy(file_path, os.path.join(unsupported_folder, filename))
                except Exception as e:
                    log_error(f"Error processing file: {file_path}\n{str(e)}")
                    continue

                processed_files += 1
                update_status(processed_files, total_files, start_time, status_label)
        
        messagebox.showinfo("Info", "Duplicates copied. Review files in the destination folder before continuing.")
        
        save_seen_files(seen_files)

        user_ready = messagebox.askyesno("Ready to Remove?", "Are you ready to remove duplicates from the source?")
        if user_ready:
            for filename in os.listdir(dest_folder):
                duplicate_path = os.path.join(dest_folder, filename)
                if os.path.isfile(duplicate_path):
                    duplicate_hash = hash_file(duplicate_path) 
                    
                    if duplicate_hash in seen_files:
                        original_file = seen_files[duplicate_hash] 
                        os.remove(original_file) 

    try:
        extract_all_files()
        move_duplicates()
    finally:
        try:
            download_folder = os.path.expanduser("~/Downloads")
            zip_path = os.path.join(download_folder, "archived_files.zip")
            with zipfile.ZipFile(zip_path, 'w', zipfile.ZIP_DEFLATED) as zipf:
                for item in os.listdir(dest_folder):
                    item_path = os.path.normpath(os.path.join(dest_folder, item))
                    if os.path.isfile(item_path) or os.path.islink(item_path):
                        zipf.write(item_path, os.path.basename(item_path))
                        os.remove(item_path)
            send2trash(os.path.normpath(zip_path))
        except Exception as e:
            log_error(f"Error clearing folder: {str(e)}")
        messagebox.showinfo("Info", "Program finished running... files can be recovered from bin")

def select_folder(prompt):
    folder = filedialog.askdirectory(title=prompt)
    return folder

def start_processing():
    try:
        
        source_folder = select_folder('Select the folder to search for duplicates')
        destination_folder = select_folder('Select the folder to move duplicates to')
        unsupported_folder = ''

        if source_folder and destination_folder:
            if media_var.get():
                unsupported_folder = select_folder('Select the folder to move unsupported files to')
                file_extensions = load_extensions('extensions.json')
                if not file_extensions:
                    messagebox.showerror("Error", "Failed to load file extensions.")
            else:
                file_extensions = None

            threading.Thread(target=find_duplicates_and_move_non_media, args=(
                source_folder, destination_folder, unsupported_folder, file_extensions, status_label)).start()
        else:
            messagebox.showerror("Error", "All folders must be selected.")
    except Exception as e:
        log_error(str(e))

def load_extensions(json_file):
    try:
        with open(json_file, 'r') as file:
            data = json.load(file)
            return tuple(data.get("file_extensions", []))
    except Exception as e:
        log_error(f"Error loading JSON file: {str(e)}")
        return None

app = tk.Tk()
app.title("Duplicate and Non-Media File Mover")
app.geometry('400x250')

media_var = tk.BooleanVar()
media_checkbox = tk.Checkbutton(app, text="Filter media only", variable=media_var)
media_checkbox.pack(pady=10)

size_label = tk.Label(app, text="Enter file size threshold (KB):")
size_label.pack(pady=10)

size_entry = tk.Entry(app)
size_entry.pack(pady=5)
size_entry.insert(0, "0")

start_button = tk.Button(app, text="Start", command=start_processing)
start_button.pack(pady=10)

status_label = tk.Label(app, text="Progress: 0.00% | Elapsed Time: 00:00:00 | Time Left: 00:00:00", wraplength=350, justify="left")
status_label.pack(pady=10)

app.mainloop()