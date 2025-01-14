import os
import hashlib
import shutil
import py7zr
import rarfile
import zipfile
import tarfile
import time
import tkinter as tk
from tkinter import filedialog, ttk, messagebox

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
        error_message = f"Failed to unpack archive: {file_path}\n{str(e)}"
        
        messagebox.showerror("Error", error_message)
        
        script_directory = os.path.dirname(os.path.realpath(__file__))  
        log_file = os.path.join(script_directory, 'error_log.txt')

        with open(log_file, 'a') as f:
            f.write(f"{error_message}\n") 

        return False

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

def find_duplicates_and_move_non_media(root_folder, dest_folder, unsupported_folder, extensions, status_label, total_files):
    seen_files = {}
    processed_files = 0
    temp_extract_folder = os.path.join(root_folder, 'temp_extracted')

    if not os.path.exists(temp_extract_folder):
        os.makedirs(temp_extract_folder)

    start_time = time.time()

    def count_files_in_archive(extract_to):
        count = 0
        for _, _, filenames in os.walk(extract_to):
            count += len(filenames)
        return count

    try:
        for dirpath, _, filenames in os.walk(root_folder):
            for filename in filenames:
                file_path = os.path.join(dirpath, filename)
                try:
                    if is_archive(file_path):
                        nested_extract_to = os.path.join(temp_extract_folder, os.path.splitext(filename)[0])
                        if not os.path.exists(nested_extract_to):
                            os.makedirs(nested_extract_to)
                        if unpack_archive(file_path, nested_extract_to):
                            total_files += count_files_in_archive(nested_extract_to)  
                            continue
                    elif extensions is None or filename.lower().endswith(extensions):
                        file_hash = hash_file(file_path)
                        if file_hash in seen_files:
                            print("")
                            shutil.move(file_path, os.path.join(dest_folder, filename))
                        else:
                            seen_files[file_hash] = file_path
                    else:
                        print("")
                        shutil.move(file_path, os.path.join(unsupported_folder, filename))

                except Exception as e:
                    messagebox.showerror("Error", f"Error processing file: {file_path}\n{str(e)}")
                    continue

                processed_files += 1
                update_status(processed_files, total_files, start_time, status_label)

        for dirpath, _, filenames in os.walk(temp_extract_folder):
            for filename in filenames:
                file_path = os.path.join(dirpath, filename)

    finally:
        print("finished extraction")
        #shutil.rmtree(temp_extract_folder)


def count_files(root_folder):
    return sum(len(files) for _, _, files in os.walk(root_folder))

def select_folder(prompt):
    folder = filedialog.askdirectory(title=prompt)
    return folder

def start_processing():
    try:
        
        
        #source_folder = select_folder('Select the folder to search for duplicates')
        #destination_folder = select_folder('Select the folder to move duplicates to')
        #unsupported_folder = select_folder('Select the folder to move unsupported files to')

        source_folder = r'C:\Users\Jonas Henriksen\Desktop\TEST DUPLICATE REMOVER'
        destination_folder = r'C:\Users\Jonas Henriksen\Desktop\FOUND DUPLICATES'
        unsupported_folder = r'C:\Users\Jonas Henriksen\Desktop\UNSUPPORTED FILES'

        if source_folder and destination_folder and unsupported_folder:
            total_files = count_files(source_folder)

            if media_var.get():
                 file_extensions = ('.jpg', '.jpeg', '.png', '.gif', '.bmp', '.tiff', 
                   '.mp4', '.avi', '.mkv', '.mov', '.wmv', '.flv', '.webm')
            else:
                file_extensions = None

            find_duplicates_and_move_non_media(
                source_folder, destination_folder, unsupported_folder, 
                file_extensions, status_label, total_files
            )
        else:
            messagebox.showerror("Error", "All folders must be selected.")
    except Exception as e:
        messagebox.showerror("Error", str(e))

app = tk.Tk()
app.title("Duplicate and Non-Media File Mover")
app.geometry('400x250')

media_var = tk.BooleanVar()
media_checkbox = tk.Checkbutton(app, text="Filter media only", variable=media_var)
media_checkbox.pack(pady=10)

start_button = tk.Button(app, text="Start", command=start_processing)
start_button.pack(pady=10)

status_label = tk.Label(app, text="Progress: 0.00% | Elapsed Time: 00:00:00 | Time Left: 00:00:00")
status_label.pack(pady=10)

app.mainloop()