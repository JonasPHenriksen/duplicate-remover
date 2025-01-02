import os
import hashlib
import shutil
import py7zr
import rarfile
import tkinter as tk
from tkinter import filedialog, ttk, messagebox

def hash_file(file_path):
    hash_algo = hashlib.md5()
    with open(file_path, 'rb') as f:
        while chunk := f.read(8192):
            hash_algo.update(chunk)
    return hash_algo.hexdigest()

def find_duplicates_and_move_non_media(root_folder, dest_folder, unsupported_folder, extensions, progress_bar, total_files):
    seen_files = {}
    processed_files = 0
    
    for dirpath, _, filenames in os.walk(root_folder):
        for filename in filenames:
            file_path = os.path.join(dirpath, filename)
            
            try:
                if filename.lower().endswith(extensions):
                    file_hash = hash_file(file_path)
                    file_size = os.path.getsize(file_path)

                    if file_hash in seen_files:
                        seen_file_path = seen_files[file_hash]
                        if os.path.getsize(seen_file_path) == file_size:
                            shutil.move(file_path, os.path.join(dest_folder, filename))
                    else:
                        seen_files[file_hash] = file_path
                else:
                    shutil.move(file_path, os.path.join(unsupported_folder, filename))
                
            except Exception as e:
                messagebox.showerror("Error", f"Error processing file: {file_path}\n{str(e)}")
                continue
            
            processed_files += 1
            progress_bar['value'] = (processed_files / total_files) * 100
            progress_bar.update()

def count_files(root_folder):
    return sum(len(files) for _, _, files in os.walk(root_folder))

def select_folder(prompt):
    folder = filedialog.askdirectory(title=prompt)
    return folder

def start_processing():
    try:
        source_folder = select_folder('Select the folder to search for duplicates')
        destination_folder = select_folder('Select the folder to move duplicates to')
        unsupported_folder = select_folder('Select the folder to move unsupported files to')

        if source_folder and destination_folder and unsupported_folder:
            total_files = count_files(source_folder)
            progress_bar['maximum'] = total_files

            if media_var.get():
                file_extensions = ('.jpg', '.jpeg', '.png', '.gif', '.bmp', '.tiff')
            else:
                file_extensions = None

            find_duplicates_and_move_non_media(
                source_folder, destination_folder, unsupported_folder, 
                file_extensions, progress_bar, total_files
            )
        else:
            messagebox.showerror("Error", "All folders must be selected.")
    except Exception as e:
        messagebox.showerror("Error", str(e))

app = tk.Tk()
app.title("Duplicate and Non-Media File Mover")
app.geometry('400x200')

media_var = tk.BooleanVar()
images_checkbox = tk.Checkbutton(app, text="Filter media only", variable=media_var)
images_checkbox.pack(pady=20)

start_button = tk.Button(app, text="Start", command=start_processing)
start_button.pack(pady=20)

progress_bar = ttk.Progressbar(app, orient="horizontal", length=300, mode="determinate")
progress_bar.pack(pady=20)

app.mainloop()
