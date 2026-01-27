import pytest
import os
from duplicateRemover import Deduplicator, ScanConfig, HashCache
import zipfile, tarfile, py7zr, rarfile

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

def test_expand_archives(temp_env):
    src, dst = temp_env
    # Create a dummy zip file
    zip_path = src / "test.zip"
    internal_file_name = "zipped_content.txt"
    
    with zipfile.ZipFile(zip_path, 'w') as z:
        z.writestr(internal_file_name, "hello world")

    config = ScanConfig(rule="newest", ask=False, dry_run=False, expand_archives=True)
    cache = HashCache(":memory:")
    dedup = Deduplicator(cache, config)

    dedup.expand_archives(str(src))

    # Assert: The zip file should be gone (sent to trash/moved)
    # Note: since send2trash is hard to test, we check if the folder exists
    extracted_folder = src / "test"
    assert extracted_folder.is_dir()
    assert (extracted_folder / internal_file_name).exists()

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