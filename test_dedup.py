import pytest
import os
from duplicateRemover import Deduplicator, ScanConfig, HashCache, CleanupApp
import zipfile, tarfile, py7zr, rarfile
import tkinter as tk
from tkinter import filedialog, messagebox, simpledialog

@pytest.fixture
def temp_env(tmp_path):
    """
    Sets up a temporary directory structure for every test.
    tmp_path is a built-in pytest fixture that provides a unique path.
    """
    src = tmp_path / "source"
    dst = tmp_path / "destination"
    src.mkdir()
    dst.mkdir()
    return src, dst

def test_find_duplicate_files(temp_env):
    src, dst = temp_env
    
    # Create two identical files
    file1 = src / "file1.txt"
    file2 = src / "file2.txt"
    content = b"duplicate content"
    file1.write_bytes(content)
    file2.write_bytes(content)

    # Setup the Deduplicator
    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    cache = HashCache(":memory:") # Use in-memory DB for speed
    dedup = Deduplicator(cache, config)

    # Execute
    duplicates = dedup.find_duplicate_files(str(src))

    # Assert: We expect 1 group of duplicates containing 2 files
    assert len(duplicates) == 1
    assert len(duplicates[0]) == 2
    assert str(file1) in duplicates[0]
    assert str(file2) in duplicates[0]

def test_exclude_dirs(temp_env):
    src, dst = temp_env
    
    # Create a file inside an excluded directory
    git_dir = src / ".git"
    git_dir.mkdir()
    hidden_file = git_dir / "hidden.txt"
    hidden_file.write_bytes(b"should not be found")

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    cache = HashCache(":memory:")
    dedup = Deduplicator(cache, config)

    duplicates = dedup.find_duplicate_files(str(src))
    
    # Assert: The .git folder should have been skipped
    assert len(duplicates) == 0

def test_resolution_logic(temp_env):
    src, dst = temp_env
    
    # Create two identical files with different timestamps
    file_old = src / "old.txt"
    file_new = src / "new.txt"
    content = b"same"
    file_old.write_bytes(content)
    file_new.write_bytes(content)
    
    # Set file_old to the past (timestamp: 1000)
    os.utime(file_old, (1000, 1000))

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    cache = HashCache(":memory:")
    dedup = Deduplicator(cache, config)

    groups = [[str(file_old), str(file_new)]]
    dedup.resolve(groups, str(dst), ask_fn=None)

    # Assert: newest rule should keep 'new.txt' and move 'old.txt'
    assert os.path.exists(file_new)
    assert not os.path.exists(file_old)
    assert os.path.exists(dst / "old.txt")

def test_nested_archive_expansion(temp_env):
    src, _ = temp_env
    
    # 1. Create the inner zip
    inner_zip_path = src / "inner.zip"
    with zipfile.ZipFile(inner_zip_path, 'w') as z:
        z.writestr("deep_file.txt", "I am very deep")
        
    # 2. Create the outer zip containing the inner zip
    outer_zip_path = src / "outer.zip"
    with zipfile.ZipFile(outer_zip_path, 'w') as z:
        z.write(inner_zip_path, arcname="inner.zip")
    
    # Remove the loose inner zip so we only have the outer one
    os.remove(inner_zip_path)

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=True)
    dedup = Deduplicator(HashCache(":memory:"), config)

    # Execute
    dedup.expand_archives(str(src))

    # Assertions
    # Level 1: Outer should be a folder now
    assert (src / "outer").is_dir()
    # Level 2: Inner should have been extracted from Outer and turned into a folder
    assert (src / "outer" / "inner").is_dir()
    # Level 3: The actual content should be present
    assert (src / "outer" / "inner" / "deep_file.txt").exists()

def test_all_resolution_rules(temp_env):
    src, dst = temp_env
    
    # Path setup: Root file vs Deep file
    short_p = src / "file.txt"
    deep_dir = src / "subdir" / "extra"
    deep_dir.mkdir(parents=True)
    long_p = deep_dir / "file.txt"
    
    content = b"same content"
    short_p.write_bytes(content)
    long_p.write_bytes(content)

    # 1. Test SHORTEST_PATH
    config_short = ScanConfig(rule="shortest_path", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config_short)
    dedup.resolve([[str(short_p), str(long_p)]], str(dst), None)
    
    assert os.path.exists(short_p)
    assert not os.path.exists(long_p)

    for f in dst.iterdir():
        f.unlink()

    # 2. Test OLDEST
    # Re-create long_p and make short_p older
    long_p.write_bytes(content)
    os.utime(short_p, (500, 500)) 
    os.utime(long_p, (2000, 2000))

    config_old = ScanConfig(rule="oldest", ask=False, dry_run=False, expand_archives=False)
    dedup_old = Deduplicator(HashCache(":memory:"), config_old)
    dedup_old.resolve([[str(short_p), str(long_p)]], str(dst), None)

    assert os.path.exists(short_p) # Oldest kept
    assert not os.path.exists(long_p)

def test_folder_duplication(temp_env):
    src, dst = temp_env
    
    # Create two folders with same name and same content
    f1 = src / "FolderA"
    f2 = src / "other" / "FolderA"
    f2.parent.mkdir()
    for d in [f1, f2]:
        d.mkdir()
        (d / "data.txt").write_bytes(b"content")

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)

    duplicates = dedup.find_duplicate_folders(str(src))

    assert len(duplicates) == 1
    assert str(f1) in duplicates[0]
    assert str(f2) in duplicates[0]

def test_dry_run_safety(temp_env):
    src, dst = temp_env
    file1 = src / "a.txt"
    file2 = src / "b.txt"
    file1.write_bytes(b"data")
    file2.write_bytes(b"data")

    config = ScanConfig(rule="newest", ask=False, dry_run=True, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    groups = [[str(file1), str(file2)]]
    dedup.resolve(groups, str(dst), None)

    # Assert: Both files should still exist in source
    assert file1.exists()
    assert file2.exists()
    # Assert: Destination should be empty
    assert len(list(dst.iterdir())) == 0

from unittest.mock import MagicMock

def test_ask_user_resolution(temp_env):
    src, dst = temp_env
    f1 = src / "keep_me.txt"
    f2 = src / "delete_me.txt"
    f1.write_bytes(b"same")
    f2.write_bytes(b"same")

    config = ScanConfig(rule="newest", ask=True, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)

    # Mock the user picking the first file (index 0)
    mock_ask = MagicMock(return_value=str(f1))
    
    groups = [[str(f1), str(f2)]]
    dedup.resolve(groups, str(dst), ask_fn=mock_ask)

    assert f1.exists()
    assert not f2.exists()
    assert (dst / "delete_me.txt").exists()
    mock_ask.assert_called_once()

import sqlite3

def test_hash_cache_persistence(tmp_path):
    db_file = tmp_path / "test_cache.db"
    file_to_hash = tmp_path / "data.bin"
    file_to_hash.write_bytes(b"very large data")

    # 1. First pass: Hash and save to disk
    cache = HashCache(str(db_file))
    first_hash = cache.hash_file(str(file_to_hash))
    cache.close()

    # 2. Second pass: Open new cache instance and check DB
    cache2 = HashCache(str(db_file))
    conn = sqlite3.connect(str(db_file))
    cur = conn.cursor()
    cur.execute("SELECT hash FROM file_hashes WHERE path=?", (str(file_to_hash),))
    row = cur.fetchone()
    
    assert row is not None
    assert row[0] == first_hash
    cache2.close()

import os
import stat

def test_permission_denied_handling(temp_env):
    src, dst = temp_env
    locked_file = src / "locked.txt"
    locked_file.write_bytes(b"secret")
    
    # Make file unreadable (000 permissions)
    os.chmod(locked_file, 0o000)

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)

    try:
        # This should not raise an exception even if it can't read the file
        duplicates = dedup.find_duplicate_files(str(src))
        assert len(duplicates) == 0
    finally:
        # Fix permissions so pytest can clean up the temp directory
        os.chmod(locked_file, stat.S_IWRITE)

def test_multiple_duplicates(temp_env):
    src, dst = temp_env
    files = []
    for i in range(3):
        f = src / f"copy_{i}.txt"
        f.write_bytes(b"identical")
        files.append(str(f))

    config = ScanConfig(rule="oldest", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # Simulate finding them
    groups = [files]
    dedup.resolve(groups, str(dst), None)

    # Check that only 1 remains in source and 2 are in destination
    src_remaining = list(src.glob("*.txt"))
    dst_moved = list(dst.glob("*.txt"))
    
    assert len(src_remaining) == 1
    assert len(dst_moved) == 2

from unittest.mock import patch

def test_app_run_trigger(temp_env):
    src, dst = temp_env
    with patch('tkinter.filedialog.askdirectory') as mock_dir:
        # Mocking the folder selection
        mock_dir.side_effect = [str(src), str(dst)]
        
        root = tk.Tk()
        app = CleanupApp(root)
        
        # Manually trigger the core logic function
        # We skip the thread for testing purposes
        app.run() 
        root.update()
        root.destroy()

def test_unpack_all_formats(temp_env):
    src, dst = temp_env
    config = ScanConfig("newest", False, False, True)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # We create fake files with these extensions to trigger the if/elif branches
    formats = [".zip", ".tar", ".7z", ".rar"]
    for ext in formats:
        fake_archive = src / f"test{ext}"
        fake_archive.write_bytes(b"not a real archive")
        # This will trigger the specific elif and then hit the 'except Exception'
        # which covers both the extraction attempt and the error handling.
        dedup._unpack(str(fake_archive), str(src / "extracted"))

@patch('duplicateRemover.send2trash')
@patch('shutil.move')
def test_resolution_final_fallback_safe(mock_move, mock_trash, temp_env):
    src, dst = temp_env
    f1 = src / "keep.txt"
    f2 = src / "skip_on_error.txt"
    f1.write_text("data")
    f2.write_text("data")

    # Force move and trash to fail
    mock_move.side_effect = Exception("Move failed")
    mock_trash.side_effect = Exception("Trash failed")

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # This should now skip the file instead of deleting it
    dedup.resolve([[str(f1), str(f2)]], str(dst), None)

    # ASSERTION: The file must still exist!
    assert f2.exists(), "The file should be skipped, not deleted, when errors occur."

def test_app_cancel_selection(temp_env):
    with patch('tkinter.filedialog.askdirectory') as mock_dir:
        # User selects source but cancels destination
        mock_dir.side_effect = ["/some/path", ""]
        
        root = tk.Tk()
        app = CleanupApp(root)
        app.run() # Should return early
        
        # Verify it didn't crash and remained idle
        assert "Idle" in app.status.cget("text")
        root.destroy()

@patch('tkinter.simpledialog.askstring')
def test_ask_user_logic_branches(mock_ask, temp_env):
    root = tk.Tk()
    app = CleanupApp(root)
    paths = ["/path/1", "/path/2"]

    # 1. Test user picks a valid number ("1")
    mock_ask.return_value = "1"
    assert app.ask_user(paths, is_folder=False) == "/path/1"

    # 2. Test user picks "0" (Skip)
    mock_ask.return_value = "0"
    assert app.ask_user(paths, is_folder=False) is None

    # 3. Test user hits "Cancel" (returns None)
    mock_ask.return_value = None
    assert app.ask_user(paths, is_folder=False) is None

    # 4. Test user enters gibberish (ValueError branch)
    mock_ask.return_value = "abc"
    assert app.ask_user(paths, is_folder=False) is None

    # 5. Test user enters out-of-bounds number
    mock_ask.return_value = "99"
    assert app.ask_user(paths, is_folder=False) is None

    root.destroy()

def test_auto_select_keep_fallback(temp_env):
    src, _ = temp_env
    p1 = str(src / "a.txt")
    p2 = str(src / "b.txt")
    paths = [p1, p2]
    
    # Create the files so os.path.getmtime doesn't crash if called
    (src / "a.txt").write_text("file a")
    (src / "b.txt").write_text("file b")

    # Pass an unrecognized rule to trigger the final 'return paths[0]'
    from duplicateRemover import auto_select_keep
    result = auto_select_keep(paths, rule="unrecognized_rule")
    
    assert result == p1  # It should return the first item in the list

def test_hash_cache_hit(tmp_path):
    db_file = tmp_path / "cache_hit.db"
    test_file = tmp_path / "static.txt"
    test_file.write_text("consistent content")

    cache = HashCache(str(db_file))
    
    # First pass: This calculates the hash and saves it (Cache Miss)
    first_hash = cache.hash_file(str(test_file))
    
    # Second pass: This should trigger 'return row[0]' (Cache Hit)
    # because the path, size, and mtime are identical.
    second_hash = cache.hash_file(str(test_file))

    assert first_hash == second_hash
    cache.close()

def test_deduplicator_status_callback(temp_env):
    src, _ = temp_env
    # A simple list to act as our callback collector
    status_updates = []
    def mock_callback(text):
        status_updates.append(text)

    config = ScanConfig("newest", False, False, False)
    # Pass our collector into the deduplicator
    dedup = Deduplicator(HashCache(":memory:"), config, status_cb=mock_callback)

    # Directly call the internal _status method
    test_message = "Testing status update"
    dedup._status(test_message)

    # Assert that our callback was triggered and the message was captured
    assert test_message in status_updates

@patch('tkinter.simpledialog.askstring')
def test_resolve_user_skip(mock_ask, temp_env):
    src, dst = temp_env
    f1 = src / "dup.txt"
    f1.write_text("content")
    
    # Setup: Ask is True, but mock returns None (User skipped)
    config = ScanConfig(rule="newest", ask=True, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # We pass the real app.ask_user method or a mock that returns None
    mock_ask.return_value = "0" 
    
    # We call resolve with one group
    dedup.resolve([[str(f1)]], str(dst), ask_fn=lambda g, is_f: None)

    # Assert: The file should still be in source because we 'continued'
    assert f1.exists()

import time

def test_resolve_filename_collision(temp_env):
    src, dst = temp_env
    
    # 1. Create files
    keep_path = src / "to_keep.txt"
    move_path = src / "image.png"
    keep_path.write_text("i am staying")
    move_path.write_text("i am moving")
    
    # 2. Force keep_path to be the NEWEST
    # Set move_path to the past, and keep_path to "now"
    past_time = time.time() - 100
    os.utime(move_path, (past_time, past_time))
    os.utime(keep_path, (time.time(), time.time()))
    
    # 3. Pre-create the collision file in destination
    (dst / "image.png").write_text("i was already here")
    
    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=False)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # Execute resolve
    # Because keep_path is newer, move_path MUST be the one moved.
    dedup.resolve([[str(keep_path), str(move_path)]], str(dst), ask_fn=None)
    
    # Assertions
    assert (dst / "image.png").exists(), "Original collision file should remain"
    assert (dst / "image_1.png").exists(), "The moved file should have been renamed to image_1.png"
    assert keep_path.exists(), "The newer file should have stayed in source"

def test_archive_extraction_file_collision(temp_env):
    src, _ = temp_env
    
    # 1. Create a file named 'data.json'
    collision_file = src / "data.json"
    collision_file.write_text("i am a file")
    
    # 2. Create an archive named 'data.json.zip'
    zip_path = src / "data.json.zip"
    with zipfile.ZipFile(zip_path, 'w') as z:
        z.writestr("test.txt", "content")
        
    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=True)
    dedup = Deduplicator(HashCache(":memory:"), config)
    
    # This should not crash now
    dedup.expand_archives(str(src))
    
    # Verify: The file remains, and the archive is unpacked to a new folder
    assert collision_file.is_file()
    assert (src / "data.json_1").is_dir()
    assert (src / "data.json_1" / "test.txt").exists()