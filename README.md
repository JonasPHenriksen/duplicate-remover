# duplicate-remover

`photo_cleanup` tidies a folder of photos and videos. It finds files that are byte-for-byte identical, and files that are not photos or videos, and moves them into a `_Quarantine/` folder inside the folder you point it at.

- **Nothing is deleted or renamed.** Every moved file keeps its relative path inside `_Quarantine/`, so you can drag it back by hand or undo the whole run.
- **Preview first.** Nothing moves until you say so.
- **Nothing to install.** It is one file for Windows, macOS or Linux. Put it on a USB stick and run it anywhere.
- **A window to click through,** in your web browser, with progress bars, a review of everything before it moves, and undo.

## Get it

Pick the file for the computer you are on. They are published on the repository's Releases page when a version tag is pushed, or you can build them yourself (see [Building](#building)).

| Computer | File |
| --- | --- |
| Windows (almost every PC) | `photo_cleanup-windows-x64.exe` |
| Windows on ARM | `photo_cleanup-windows-arm64.exe` |
| Mac, Apple Silicon or Intel | `photo_cleanup-macos` |
| Linux, 64-bit PC | `photo_cleanup-linux-x64` |
| Linux, 64-bit ARM such as a Raspberry Pi 4 or 5 | `photo_cleanup-linux-arm64` |

They need Windows 10 or later, macOS 13 Ventura or later, or any 64-bit Linux.

The programs are not signed with a paid developer certificate, so a copy downloaded with a web browser gets a warning the first time. Copies on a USB stick usually don't.

- **Windows:** if SmartScreen says "Windows protected your PC", click **More info**, then **Run anyway**.
- **macOS:** if it says Apple could not verify the program, open **System Settings → Privacy & Security** and click **Open Anyway**. In Terminal, `xattr -d com.apple.quarantine photo_cleanup-macos` does the same. A downloaded copy also needs `chmod +x photo_cleanup-macos`.
- **Linux:** run `chmod +x photo_cleanup-linux-x64`. If a USB stick won't run programs, copy the file to your home folder first.

## Usage

**Double-click it.** It opens in your web browser:

1. Choose the folder: click **Choose…**, or paste its location.
2. **Find duplicates** shows how far it has got, with the file it is on and the time left.
3. Check what will move: the duplicate groups, with the copy that stays and why, and the files that aren't photos or videos, by type. Untick a type to leave its files where they are, for example `.xmp` sidecars.
4. **Move** them to `_Quarantine`. Afterwards you can open that folder, or **Undo** everything.

A small window stays open next to the browser while it runs. Closing it, the browser tab, or clicking **Quit** ends the program. Only your own computer can reach the page, and only through the address photo_cleanup opens, which contains a key that is new every time. On Windows you can also drag a folder onto the program to open it with that folder chosen.

**Or use a terminal:**

```bash
photo_cleanup /path/to/photos            # preview: write reports, move nothing
photo_cleanup /path/to/photos --apply    # move files into _Quarantine/
photo_cleanup /path/to/photos --undo     # put everything back
photo_cleanup --gui /path/to/photos      # the browser window, with this folder chosen
```

Use the file name for your computer, for example `photo_cleanup-windows-x64.exe "D:\Photos"` or `./photo_cleanup-macos ~/Pictures`. Read the reports from the preview before running `--apply`. Ctrl+C stops between two files, so everything moved so far can still be undone.

Without a screen, for example over SSH, starting it without arguments asks for the folder in the terminal instead.

## Moving it around

`photo_cleanup` is a single file with no settings or other files of its own, so you can move, copy or rename it freely: to a USB stick, the desktop, or another computer.

The photo folder can move too. Everything a clean-up records lives inside the folder, in `_Quarantine/`, with paths relative to the folder. You can rename or move the folder, or open the drive on another computer or operating system, and undo still works: just point it at the folder where it is now. Two things to keep:

- Leave `_Quarantine/` inside the folder it was made in, or undo can't find the files.
- If the program sits inside the folder you clean, keep a name that starts with `photo_cleanup`, so it leaves itself and its copies for other systems alone.

## What gets moved

| Moved to | What |
| --- | --- |
| `_Quarantine/duplicates/<original path>` | Every extra copy of a photo or video that is identical to another one. One copy always stays. |
| `_Quarantine/not-media/<original path>` | Every file whose extension is not in `mediaExt`, plus empty files. |

"Not media" means everything else, such as documents and sidecar files like `.xmp` and `.aae`. PDFs count as media.

Duplicates must match byte for byte. Files are grouped by size, then by a hash of their first and last 64 KB, then by a full SHA-256. Resized, re-encoded or re-tagged versions of a photo are left alone.

These are never moved:

- **System files and folders:** `.DS_Store`, `Thumbs.db`, `desktop.ini`, the `._*` files macOS writes on USB sticks, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.Trash-1000`, `$RECYCLE.BIN`, `System Volume Information`, and NAS folders such as `@eaDir`.
- **The program itself:** anything named `photo_cleanup*`, so it can live on the same stick as your photos.
- **Any folder named `_Quarantine`**, so a run never touches an earlier one.
- **Symlinks**, which are never followed. Hard links to the same file count as one file.

A moved file never overwrites anything already in quarantine: a clashing name gets a `~2` suffix.

## Which copy stays

The first rule that tells identical copies apart decides which one is kept:

1. Not in a backup or phone-dump folder (`disposableParts`: `Backup`, `DCIM`, `Camera Roll`, `Downloads`, `WhatsApp Images`, `Usorteret`, …). Only folders inside the folder you scan count, so a library that itself lives in `Downloads` is judged the same as one anywhere else.
2. Not named like a copy (`copyMarkers`: `copy`, `kopi`, `(1)`, …)
3. In the deeper, more specific folder
4. The oldest modification time
5. The path, alphabetically, so every run picks the same file

`duplicates.csv` gives the reason for each group.

## Reports

Every run writes these to `_Quarantine/`:

| File | Contents |
| --- | --- |
| `summary.txt` | Counts and sizes, and the non-media files by extension |
| `duplicates.csv` | Each duplicate group: the kept file and why, and the copies to quarantine |
| `not-media.csv` | Every non-media file with its size and extension |
| `journal.jsonl` | Every move made, which undo reads |

## Undo

Undo moves every file in the journal back, most recent move first. It never overwrites: if something new sits at a file's original path, the quarantined copy stays where it is and stays in the journal, so you can undo again once the path is free. Afterwards it removes the empty quarantine folders and keeps the reports.

The journal stores paths relative to the folder, so undo works wherever the drive is mounted, even on another operating system. Journals written by the old Python version can still be undone from the same location.

## Settings

Edit the lists at the top of [main.go](main.go) (`mediaExt`, `disposableParts`, `copyMarkers`, `systemDirs`, `systemFiles`), then rebuild.

## Building

Needs [Go](https://go.dev/dl/) 1.22 or later.

```bash
./build.sh        # every file in the table above, into dist/
go test ./...     # the tests
```

`build.sh` makes a single Mac file for Apple Silicon and Intel when it runs on macOS. The macOS 13 minimum comes from the Go version: building with Go 1.22 supports Macs back to macOS 10.15.

GitHub runs the tests on Windows, macOS and Linux for every push. Pushing a tag such as `v1.0.0` also publishes the files as a release (see [.github/workflows/ci.yml](.github/workflows/ci.yml)).
