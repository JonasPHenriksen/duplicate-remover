# duplicate-remover

`photo_cleanup` tidies a folder of photos and videos. It finds files that are byte-for-byte identical, and files that are not photos or videos, and moves them into a `_Quarantine/` folder inside the folder you point it at.

- **Nothing is deleted or renamed** unless you ask. Every moved file keeps its relative path inside `_Quarantine/`, so you can drag it back by hand or undo the whole run. Emptying `_Quarantine` is the one thing that deletes, and it asks first.
- **Preview first.** Nothing moves until you say so.
- **Nothing to install.** It is one file for Windows, macOS or Linux. Put it on a USB stick and run it anywhere.
- **A window to click through,** in your web browser, with progress bars, a review of everything before it moves, and undo.
- **Options when you want more control:** photos that look alike, the same picture with other tags, folders to keep or leave out, a reference folder, and more. See [Options](#options).
- **A simple version** in Danish or English, with large text, plain words and pictures of the duplicates, for anyone who would rather not see file paths. See [The simple version](#the-simple-version).

## Get it

Pick the file for the computer you are on, in the regular version or the [simple one](#the-simple-version). They are published on the repository's Releases page when a version tag is pushed, or you can build them yourself (see [Building](#building)).

| Computer | Regular | Simple, in Danish or English |
| --- | --- | --- |
| Windows (almost every PC) | `photo_cleanup-windows-x64.exe` | `photo_cleanup-simple-windows-x64.exe` |
| Windows on ARM | `photo_cleanup-windows-arm64.exe` | `photo_cleanup-simple-windows-arm64.exe` |
| Mac, Apple Silicon or Intel | `photo_cleanup-macos` | `photo_cleanup-simple-macos` |
| Linux, 64-bit PC | `photo_cleanup-linux-x64` | `photo_cleanup-simple-linux-x64` |
| Linux, 64-bit ARM such as a Raspberry Pi 4 or 5 | `photo_cleanup-linux-arm64` | `photo_cleanup-simple-linux-arm64` |

They need Windows 10 or later, macOS 13 Ventura or later, or any 64-bit Linux.

The programs are not signed with a paid developer certificate, so a copy downloaded with a web browser gets a warning the first time. Copies on a USB stick usually don't.

- **Windows:** if SmartScreen says "Windows protected your PC", click **More info**, then **Run anyway**.
- **macOS:** if it says Apple could not verify the program, open **System Settings → Privacy & Security** and click **Open Anyway**. In Terminal, `xattr -d com.apple.quarantine photo_cleanup-macos` does the same. A downloaded copy also needs `chmod +x photo_cleanup-macos`.
- **Linux:** run `chmod +x photo_cleanup-linux-x64`. If a USB stick won't run programs, copy the file to your home folder first.

## Usage

**Double-click it.** It opens in your web browser:

1. Choose the folder: click **Choose…**, or paste its location. **Options** under it change what to look for and which copy stays; see [Options](#options).
2. **Find duplicates** shows how far it has got, with the file it is on and the time left.
3. Check what will move: the duplicate groups, with a picture, the copy that stays and why, and the files that aren't photos or videos, by type. Untick a group to leave it alone, or keep another copy instead. Untick a type to leave its files where they are, for example `.xmp` sidecars.
4. **Move** them to `_Quarantine`. Afterwards you can open that folder, **Undo** everything, or **Empty _Quarantine** to delete what is in it for good.

A small window stays open next to the browser while it runs. Closing it, the browser tab, or clicking **Quit** ends the program. Only your own computer can reach the page, and only through the address photo_cleanup opens, which contains a key that is new every time. On Windows you can also drag a folder onto the program to open it with that folder chosen.

**Or use a terminal:**

```bash
photo_cleanup /path/to/photos            # preview: write reports, move nothing
photo_cleanup /path/to/photos --apply    # move files into _Quarantine/
photo_cleanup /path/to/photos --undo     # put everything back
photo_cleanup --gui /path/to/photos      # the browser window, with this folder chosen
photo_cleanup /path/to/photos --similar --reference /Volumes/Backup/Photos   # with options
```

Use the file name for your computer, for example `photo_cleanup-windows-x64.exe "D:\Photos"` or `./photo_cleanup-macos ~/Pictures`. Read the reports from the preview before running `--apply`. Ctrl+C stops between two files, so everything moved so far can still be undone.

Without a screen, for example over SSH, starting it without arguments asks for the folder in the terminal instead.

## Options

The page has them under **Options**, each with a line explaining it, and the command line takes them as flags. Without any, photo_cleanup works as described in the rest of this file.

| Option | Flag | What it does |
| --- | --- | --- |
| Files that aren't photos or videos | `--duplicates-only` turns it off | Moves documents, sidecar files and the like to `_Quarantine/not-media`. Off, it looks only for duplicates. |
| The same picture with other tags | `--same-image` | JPEGs whose pictures are identical but whose tags differ, such as dates, ratings or captions a photo app wrote, count as duplicates. Their pictures are compared byte for byte without their tags, and the copy with the most tags is preferred. Copies turned another way, or with other colour profiles, don't count, as they don't look the same. |
| Photos that look alike | `--similar`, `--similar=loose` | Lists copies at another size or quality, and with `loose` also shots taken a moment apart. Nothing in the list moves unless you tick it in the review; the command line only writes it to `similar.csv`. Slower, as every photo is opened, and only for JPEG, PNG and GIF. |
| Keep the newest copy | `--keep-newest` | Of copies nothing else tells apart, the newest stays rather than the oldest. |
| Folders whose copies always stay | `--prefer FOLDER` | Copies of photos and videos in these folders never move, and stay rather than their copies elsewhere. |
| More backup folder names | `--backup NAME` | More folder names whose copies move before others, like `Backup` and `DCIM` (see [Which copy stays](#which-copy-stays)). Separate several with commas. |
| Reference folder | `--reference FOLDER` | Photos and videos already in this folder, outside the one you clean, count as kept, so all their copies in the folder you clean move. Nothing in the reference folder changes. Use it to clear a backup drive or a memory card of what is in your library already. |
| Folders to leave out | `--skip FOLDER` | Never looked in, so nothing in them moves. |
| Remove folders left empty | `--remove-empty` | Removes the folders the move empties. Undo makes them again. |
| Read several files at once | `--one-at-a-time` turns it off | Reads up to 8 files at once, which is faster on most disks. Spinning hard disks are faster one file at a time; on Linux, which can tell, a folder on one is read that way anyway. |
| Remember fingerprints | `--cache` | Keeps what it learns about each file in `_Quarantine/fingerprints.jsonl`, so the next run on the folder only reads files that are new or changed. A remembered hash only counts while the file's size, date and first and last 64 KB are unchanged. |

`--prefer`, `--backup` and `--skip` can be given more than once. The folders to keep and to leave out must be inside the folder you clean, and can be given relative to it.

The page remembers its ticks and backup names for the next time, but not the folders, which belong to the folder you cleaned. The simple version has no options: it always works as without them.

## Emptying _Quarantine

**Empty _Quarantine…** on the page deletes, for good, the files that clean-ups moved into the folder's `_Quarantine`, after asking, and naming the folder and how many files. It is the only thing photo_cleanup ever deletes, and it deletes only plain files that the journal lists in the folders inside `_Quarantine`: not the reports or the journal, not anything else you put there, and nothing the journal points to outside it. If the journal has changed since it asked, it deletes nothing and asks again. Afterwards Undo has nothing to put back. On the command line, delete the files yourself.

## The simple version

`photo_cleanup-simple` is the same program with a page for people who would rather not see file paths or technical terms: large text and buttons, one step at a time, plain words, and the duplicates shown as pictures, so you can see which photos they are. Clicking a picture shows it bigger.

It speaks Danish or English. It picks Danish when the computer or the web browser is set up in Danish, and English otherwise, and the buttons at the top switch between them. It remembers the choice. The folder picker uses the page's language. The window the program runs in follows the computer's language on Linux and macOS, and is in English on Windows. The reports in `_Quarantine/` and the technical details at the bottom of the page stay in English.

It finds and moves exactly the same files as the regular version does without options, into the same `_Quarantine/`, so either version can undo the other's clean-up. On Linux it also offers the desktop's Pictures folder, such as `~/Billeder`, as a single button.

### Setting it up for someone on Linux Mint

1. Put `photo_cleanup-simple-linux-x64` in their home folder, for example in `~/Programmer/`, and make it executable: right-click it, choose **Properties → Permissions** and tick **Allow executing file as program**, or run `chmod +x` on it.
2. Give it a menu entry: save the lines below as `~/.local/share/applications/billedoprydning.desktop`, with `Exec` set to where the file is. It then shows up in the menu as **Billedoprydning**, where right-clicking it can add it to the desktop or the panel.

   ```ini
   [Desktop Entry]
   Type=Application
   Name=Billedoprydning
   Comment=Find dobbelte billeder og ryd op
   Exec=/home/anna/Programmer/photo_cleanup-simple-linux-x64
   Icon=image-x-generic
   Terminal=false
   Categories=Graphics;
   ```

3. The button for choosing another folder uses `zenity` or `kdialog`. If the page says the computer can't show a window for choosing folders, install zenity: `sudo apt install zenity`.

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
| `_Quarantine/similar/<original path>` | Photos that look alike, but only those you tick in the review. |

"Not media" means everything else, such as documents and sidecar files like `.xmp` and `.aae`. PDFs count as media.

Duplicates must match byte for byte. Files are grouped by size, then by a hash of their first and last 64 KB, then by a full SHA-256. Re-tagged versions of a photo are left alone unless you choose **The same picture with other tags**, and resized or re-encoded ones are only listed with **Photos that look alike**.

These are never moved:

- **System files and folders:** `.DS_Store`, `Thumbs.db`, `desktop.ini`, the `._*` files macOS writes on USB sticks, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.Trash-1000`, `$RECYCLE.BIN`, `System Volume Information`, and NAS folders such as `@eaDir`.
- **The program itself:** anything named `photo_cleanup*`, so it can live on the same stick as your photos.
- **Any folder named `_Quarantine`**, so a run never touches an earlier one.
- **Symlinks**, which are never followed. Hard links to the same file count as one file.

A moved file never overwrites anything already in quarantine: a clashing name gets a `~2` suffix.

## Which copy stays

The first rule that tells identical copies apart decides which one is kept:

1. In the reference folder, when there is one. Its copies are never moved anyway.
2. In one of the folders to keep, when there are any. Copies there never move either.
3. Not in a backup or phone-dump folder (`disposableParts`: `Backup`, `DCIM`, `Camera Roll`, `Downloads`, `WhatsApp Images`, `Usorteret`, …, and the names you add). Only folders inside the folder you scan count, so a library that itself lives in `Downloads` is judged the same as one anywhere else.
4. Not named like a copy (`copyMarkers`: `copy`, `kopi`, `(1)`, …)
5. With the most tags, for the same picture with other tags
6. In the deeper, more specific folder
7. The oldest modification time, or the newest with **Keep the newest copy**
8. The path, alphabetically, so every run picks the same file

Of photos that look alike, the one with the most pixels is suggested, then the largest file, then these rules. In the review you can keep any other copy of a group instead. `duplicates.csv` gives the reason for each group.

## Reports

Every run writes these to `_Quarantine/`:

| File | Contents |
| --- | --- |
| `summary.txt` | Counts and sizes, and the non-media files by extension |
| `duplicates.csv` | Each duplicate group: the kept file and why, and the copies to quarantine |
| `not-media.csv` | Every non-media file with its size and extension |
| `similar.csv` | With **Photos that look alike**: each group, its size in pixels, and the photo suggested to keep |
| `journal.jsonl` | Every move made, which undo reads |
| `fingerprints.jsonl` | With **Remember fingerprints**: what the next run needn't read again |

## Undo

Undo moves every file in the journal back, most recent move first. It never overwrites: if something new sits at a file's original path, the quarantined copy stays where it is and stays in the journal, so you can undo again once the path is free. Folders that **Remove folders left empty** removed come back as files go back into them. Afterwards it removes the empty quarantine folders and keeps the reports.

The journal stores paths relative to the folder, so undo works wherever the drive is mounted, even on another operating system. Journals written by the old Python version can still be undone from the same location.

## Settings

The [options](#options) cover most needs. To change the defaults themselves, edit the lists at the top of [main.go](main.go) (`mediaExt`, `disposableParts`, `copyMarkers`, `systemDirs`, `systemFiles`), then rebuild.

## Building

Needs [Go](https://go.dev/dl/) 1.22 or later.

```bash
./build.sh        # every file in the table above, both versions, into dist/
go test ./...     # the tests
```

The simple version is the same code built with `-ldflags "-X main.edition=simple"`, which serves [simple.html](simple.html) instead of [gui.html](gui.html). Its Danish and English words are both in `simple.html`, and the tests check that every text exists in both languages.

`build.sh` makes a single Mac file for Apple Silicon and Intel when it runs on macOS. The macOS 13 minimum comes from the Go version: building with Go 1.22 supports Macs back to macOS 10.15.

GitHub runs the tests on Windows, macOS and Linux for every push. Pushing a tag such as `v1.0.0` also publishes the files as a release (see [.github/workflows/ci.yml](.github/workflows/ci.yml)).
