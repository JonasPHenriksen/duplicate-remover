// Command photo_cleanup quarantines duplicate photos/videos and non-media files.
//
// Folder structure is never changed. Nothing is deleted. Nothing is renamed.
// Everything removed is moved into <folder>/_Quarantine/ under the exact same
// relative path it came from, so you can review it and put it back.
//
// Usage:
//
//	photo_cleanup                            # open the graphical interface
//	photo_cleanup /path/to/photos            # look only, change nothing
//	photo_cleanup /path/to/photos --apply    # move files to _Quarantine
//	photo_cleanup /path/to/photos --undo     # put everything back
//
// The graphical interface is a page in the web browser, served by the program
// to this computer only. It is a single self-contained program: nothing to install.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
)

// ---------------------------------------------------------------------------
// Settings - edit these if you need to, then rebuild with build.sh
// ---------------------------------------------------------------------------

// Anything whose extension is not in here is treated as "not a photo or video".
// Includes pdf as it can also be used as a photo.
var mediaExt = setOf(
	// photos
	"jpg", "jpeg", "jpe", "jfif", "png", "gif", "bmp", "dib", "tif", "tiff", "webp",
	"heic", "heif", "hif", "avif", "jxl", "jp2", "j2k", "jpf", "jpx", "tga", "pcx",
	"ppm", "pgm", "pbm", "pnm", "psd", "psb", "xcf", "svg", "pdf",
	// camera raw
	"dng", "cr2", "cr3", "crw", "nef", "nrw", "arw", "srf", "sr2", "raf", "orf",
	"rw2", "raw", "pef", "ptx", "srw", "x3f", "erf", "mef", "mos", "iiq", "3fr",
	"fff", "kdc", "dcr", "k25", "mrw", "rwl", "rwz", "gpr", "ari", "cap", "eip",
	// videos
	"mp4", "m4v", "mov", "qt", "avi", "mkv", "webm", "wmv", "asf", "flv", "f4v",
	"mpg", "mpeg", "mpe", "m1v", "m2v", "mts", "m2ts", "ts", "mod", "tod", "vob",
	"3gp", "3g2", "mxf", "ogv", "ogm", "rm", "rmvb", "divx", "dv", "mjpg", "mjpeg",
	"insv", "lrv", "360", "braw", "r3d", "amv", "svi",
)

const (
	quarantine  = "_Quarantine"
	dupDir      = "duplicates" // _Quarantine/duplicates/<original path>
	otherDir    = "not-media"  // _Quarantine/not-media/<original path>
	journalName = "journal.jsonl"
)

// Folders that look like a phone dump or a backup rather than a curated album.
// When the same photo exists in one of these and also elsewhere, the copy in the
// curated folder is the one that stays. Only folders inside the scanned folder
// count, so it makes no difference where that folder itself lives.
var disposableParts = setOf(
	"backup", "backups", "dcim", "camera", "camera roll", "camera uploads",
	"unsorted", "usorteret", "downloads", "download", "temp", "tmp", "new folder",
	"recovered", "recovery", "restored", "old phone", "gammel telefon", "import",
	"imported", "copy", "copies", "kopi", "til sortering", "diverse", "misc",
	"whatsapp", "whatsapp images", "whatsapp video", "telegram", "screenshots",
	"100andro", "100apple", "100canon", "100nikon", "100media", "100msdcf", "101apple",
)

// Filename markers that suggest a copy rather than the original.
var copyMarkers = []string{"copy", "kopi", "copia", "duplicate", "dublet", "(1)", "(2)", "(3)",
	"(4)", "(5)", " - kopi", "-copy", "_copy"}

// Folders managed by the operating system, the trash or a NAS: never looked in.
// Freedesktop trash folders (.Trash-1000 and so on) are matched by prefix.
var systemDirs = setOf(
	".spotlight-v100", ".fseventsd", ".trashes", ".temporaryitems", ".documentrevisions-v100", // macOS
	"$recycle.bin", "recycler", "system volume information", // Windows
	".trash", "lost+found", // Linux
	"@eadir", "#recycle", "@recycle", ".@__thumb", // Synology, QNAP
)

// Files managed by the operating system: left where they are. So are macOS's "._*"
// files, which hold the metadata of the file they sit next to on FAT/exFAT drives,
// and this program itself (anything named photo_cleanup*).
var systemFiles = setOf(
	".ds_store", ".localized", "icon\r", ".apdisk", ".volumeicon.icns", // macOS
	".com.apple.timemachine.donotpresent",
	"thumbs.db", "ehthumbs.db", "ehthumbs_vista.db", "desktop.ini", // Windows
	".directory", // KDE
)

const readChunk = 1 << 20 // 1 MB

// ---------------------------------------------------------------------------

const usage = `photo_cleanup - quarantine duplicate photos/videos and non-media files.

Folder structure is never changed. Nothing is deleted. Nothing is renamed.
Everything removed is moved into <folder>/_Quarantine/ under the exact same
relative path it came from, so you can review it and put it back.

Usage:
    photo_cleanup                            # open the graphical interface
    photo_cleanup /path/to/photos            # look only, change nothing
    photo_cleanup /path/to/photos --apply    # move files to _Quarantine
    photo_cleanup /path/to/photos --undo     # put everything back
    photo_cleanup --gui [/path/to/photos]    # the graphical interface, from here

The graphical interface opens in your web browser. Without a screen, for
example over SSH, photo_cleanup asks for the folder in the terminal instead.
`

func main() {
	in := bufio.NewReader(os.Stdin)
	var code int
	switch {
	case len(os.Args) == 1 && hasDisplay():
		code = gui(os.Args[0], "", os.Stdout)
	case len(os.Args) == 1 && isTerminal(os.Stdin):
		code = interactive(os.Args[0], in, os.Stdout, os.Stderr)
		pause(in, os.Stdout)
	case len(os.Args) == 2 && ownsConsole() && !strings.HasPrefix(os.Args[1], "-"):
		// A folder dropped onto the program in Explorer.
		code = gui(os.Args[0], os.Args[1], os.Stdout)
	default:
		code = run(os.Args, os.Stdout, os.Stderr)
		if ownsConsole() {
			pause(in, os.Stdout)
		}
	}
	os.Exit(code)
}

// run is the command line version. args includes the program name.
func run(args []string, stdout, stderr io.Writer) int {
	var applyIt, undoIt, guiIt bool
	var folders []string
	for _, a := range args[1:] {
		switch {
		case a == "--apply":
			applyIt = true
		case a == "--undo":
			undoIt = true
		case a == "--gui":
			guiIt = true
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, usage)
			return 0
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(stderr, "Unknown option %s\n\n%s", a, usage)
			return 2
		default:
			folders = append(folders, a)
		}
	}
	if guiIt && !applyIt && !undoIt && len(folders) <= 1 {
		return gui(args[0], strings.Join(folders, ""), stdout)
	}
	if guiIt || len(folders) != 1 || (applyIt && undoIt) {
		fmt.Fprint(stdout, usage)
		return 2
	}

	root, ok := folder(folders[0])
	if !ok {
		fmt.Fprintf(stderr, "Not a folder: %s\n", root)
		return 2
	}
	r, done := terminalReporter(stdout, stderr)
	defer done()
	if undoIt {
		return undoInTerminal(root, r)
	}

	p, ok := prepare(root, r)
	if !ok {
		r.warnf("\ninterrupted, nothing was moved\n")
		return 130
	}
	if applyIt {
		if code := p.apply(r); code != 0 {
			return code
		}
	}
	if code := p.report(args[0], r); code != 0 {
		return code
	}
	if !applyIt {
		r.printf("\nNothing was moved. Re-run with --apply when the lists look right.\n")
	}
	return 0
}

// interactive asks for the folder and what to do in the terminal, for when
// there is no screen to show the graphical interface on.
func interactive(prog string, in *bufio.Reader, out, errOut io.Writer) int {
	fmt.Fprint(out, "photo_cleanup moves duplicate photos and videos, and files that are not\n"+
		"photos or videos, into a _Quarantine folder inside the folder you choose.\n"+
		"Nothing is deleted, and everything can be put back.\n\n")

	var root string
	for {
		answer, ok := ask(in, out, "Folder to clean up (drag it into this window, then press Enter): ")
		if !ok {
			return 2
		}
		if answer == "" {
			continue
		}
		var isDir bool
		if root, isDir = folder(droppedPath(answer)); isDir {
			break
		}
		fmt.Fprintf(out, "Not a folder: %s\n\n", root)
	}

	fmt.Fprint(out, "\nWhat do you want to do?\n"+
		"  1  Clean up: show what would be moved, then ask before moving anything\n"+
		"  2  Undo: put back everything an earlier clean-up moved\n")
	choice, ok := ask(in, out, "Choose 1 or 2, then press Enter [1]: ")
	if !ok {
		return 2
	}
	r, done := terminalReporter(out, errOut)
	defer done()
	if choice == "2" {
		return undoInTerminal(root, r)
	}

	p, ok := prepare(root, r)
	if !ok {
		r.warnf("\ninterrupted, nothing was moved\n")
		return 130
	}
	if code := p.report(prog, r); code != 0 {
		return code
	}
	if len(p.moves) == 0 {
		fmt.Fprintln(out, "\nNothing to move.")
		return 0
	}
	answer, ok := ask(in, out, fmt.Sprintf("\nMove these %d files into %s now? Type y and press Enter: ",
		len(p.moves), quarantine))
	if !ok || !yes(answer) {
		fmt.Fprintln(out, "Nothing was moved. The lists are in", filepath.Join(root, quarantine))
		return 0
	}
	if code := p.apply(r); code != 0 {
		return code
	}
	if _, err := p.writeReports(prog); err != nil {
		fmt.Fprintln(errOut, "  ! could not write the reports:", err)
		return 1
	}
	fmt.Fprintln(out, "\nDone. To put everything back, start photo_cleanup again and choose 2.")
	return 0
}

// ask prints a question and reads one line. ok is false when input has ended.
func ask(in *bufio.Reader, out io.Writer, question string) (answer string, ok bool) {
	fmt.Fprint(out, question)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(out)
		return "", false
	}
	return strings.TrimSpace(line), true
}

func yes(answer string) bool {
	switch strings.ToLower(answer) {
	case "y", "yes", "j", "ja":
		return true
	}
	return false
}

// pause keeps a console window that only exists for this program open until
// the result has been read.
func pause(in *bufio.Reader, out io.Writer) {
	fmt.Fprint(out, "\nPress Enter to close.")
	in.ReadString('\n')
}

// folder turns a command line or typed path into a clean absolute path and
// reports whether it is a folder.
func folder(arg string) (string, bool) {
	p := expandHome(arg)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	info, err := os.Stat(p)
	return p, err == nil && info.IsDir()
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// droppedPath undoes the quoting a terminal adds when a folder is dragged into
// it: "C:\My Photos" on Windows, '/My Photos' or /My\ Photos elsewhere.
func droppedPath(s string) string {
	s = strings.TrimSpace(s)
	if runtime.GOOS == "windows" {
		return strings.Trim(s, `"`)
	}
	var b strings.Builder
	var quote rune
	escaped := false
	for _, c := range s {
		switch {
		case escaped:
			b.WriteRune(c)
			escaped = false
		case quote != 0 && c == quote:
			quote = 0
		case quote == '\'':
			b.WriteRune(c)
		case c == '\\':
			escaped = true
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// reporter is where a run reports what it does: lines of text for people, and
// done/total counts for the progress bars of the graphical interface. stop,
// when set, asks the run to stop at the next safe point, between two files.
type reporter struct {
	out, errOut io.Writer
	step        func(phase string, done, total int64, item string)
	stop        func() bool
}

func (r *reporter) printf(format string, args ...any) { fmt.Fprintf(r.out, format, args...) }
func (r *reporter) warnf(format string, args ...any)  { fmt.Fprintf(r.errOut, format, args...) }
func (r *reporter) stopped() bool                     { return r.stop != nil && r.stop() }

func (r *reporter) progress(phase string, done, total int64, item string) {
	if r.step != nil {
		r.step(phase, done, total, item)
	}
}

// terminalReporter reports to the terminal. The first Ctrl+C asks the run to
// stop between two files; a second one quits at once.
func terminalReporter(out, errOut io.Writer) (*reporter, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	var stop atomic.Bool
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-signals:
				if stop.Swap(true) {
					os.Exit(130)
				}
				fmt.Fprintln(os.Stderr, "\nStopping after the current file. Press Ctrl+C again to quit at once.")
			case <-quit:
				return
			}
		}
	}()
	return &reporter{out: out, errOut: errOut, stop: stop.Load}, func() {
		signal.Stop(signals)
		close(quit)
	}
}

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, it := range items {
		set[it] = true
	}
	return set
}
