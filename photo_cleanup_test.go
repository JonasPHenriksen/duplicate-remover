package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestMain lets TestRunsAsAProgram start this test binary as the real program.
func TestMain(m *testing.M) {
	if os.Getenv("PHOTO_CLEANUP_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// helpers

func write(t *testing.T, root, rel, data string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exist(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// files lists every file under root except the quarantine, as {relative path: contents}.
func files(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() && rel == quarantine {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			found[filepath.ToSlash(rel)] = string(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func rels(files []*file) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.rel)
	}
	sort.Strings(out)
	return out
}

// quiet is a reporter that throws everything away.
func quiet() *reporter { return &reporter{out: io.Discard, errOut: io.Discard} }

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skip("cannot create symlinks here:", err)
	}
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(append([]string{"photo_cleanup"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := runCLI(args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	return out
}

func interact(input string) (int, string) {
	var out bytes.Buffer
	code := interactive("photo_cleanup", bufio.NewReader(strings.NewReader(input)), &out, &out)
	return code, out.String()
}

func journalLines(t *testing.T, root string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(read(t, root, quarantine+"/"+journalName)), "\n")
}

func readCSV(t *testing.T, root, rel string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(read(t, root, rel))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// library is one duplicate pair, one unique photo, and two files that are not media.
func library(t *testing.T) string {
	root := filepath.Join(t.TempDir(), "photos")
	write(t, root, "Trip/IMG_1.jpg", "photo one")
	write(t, root, "Trip/IMG_1 (1).jpg", "photo one")
	write(t, root, "Trip/IMG_2.jpg", "photo two") // same size, other bytes
	write(t, root, "Notes/notes.txt", "packing list")
	write(t, root, "empty.jpg", "")
	return root
}

var reports = []string{"duplicates.csv", "not-media.csv", "summary.txt"}

func quarantineListing(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, quarantine))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// ---------------------------------------------------------------------------
// scanning

func TestIsMediaGoesByExtensionIgnoringCase(t *testing.T) {
	for _, name := range []string{"IMG_1.JPG", "clip.MoV", "scan.pdf", "raw.CR3"} {
		if !isMedia(name) {
			t.Errorf("%s should be media", name)
		}
	}
	for _, name := range []string{"notes.txt", "README", ".DS_Store", "photo.", "Thumbs.db"} {
		if isMedia(name) {
			t.Errorf("%s should not be media", name)
		}
	}
}

func TestExtensionAndStemFollowPythonPathlib(t *testing.T) {
	for name, want := range map[string][2]string{
		"IMG_1.JPG":      {"jpg", "IMG_1"},
		".DS_Store":      {"", ".DS_Store"},
		"photo.":         {"", "photo."},
		"archive.tar.gz": {"gz", "archive.tar"},
		".picasa.ini":    {"ini", ".picasa"},
		"README":         {"", "README"},
	} {
		if got := [2]string{extension(name), stem(name)}; got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestScanSortsFilesIntoMediaAndOther(t *testing.T) {
	root := library(t)
	res := scan(root, quiet())
	if got, want := rels(res.media), []string{"Trip/IMG_1 (1).jpg", "Trip/IMG_1.jpg", "Trip/IMG_2.jpg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("media %q, want %q", got, want)
	}
	if got, want := rels(res.other), []string{"Notes/notes.txt", "empty.jpg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("other %q, want %q", got, want)
	}
	if res.skipped != 0 || res.system != 0 {
		t.Errorf("skipped %d, system %d", res.skipped, res.system)
	}
}

func TestScanSkipsSymlinksAndQuarantineFolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	photo := write(t, root, "IMG_1.jpg", "x")
	write(t, root, "_Quarantine/duplicates/IMG_2.jpg", "x")
	write(t, root, "Album/_Quarantine/duplicates/IMG_3.jpg", "x") // from a run on Album alone
	outside := filepath.Dir(write(t, filepath.Dir(root), "elsewhere/IMG_4.jpg", "x"))
	symlink(t, photo, filepath.Join(root, "link.jpg"))
	symlink(t, outside, filepath.Join(root, "linked folder"))

	res := scan(root, quiet())
	if got := rels(res.media); !reflect.DeepEqual(got, []string{"IMG_1.jpg"}) {
		t.Errorf("media %q", got)
	}
	if len(res.other) != 0 {
		t.Errorf("other %q", rels(res.other))
	}
	if res.skipped != 1 { // the file link; the folder link is just not followed
		t.Errorf("skipped %d, want 1", res.skipped)
	}
}

func TestSystemFilesAndFoldersAreLeftAlone(t *testing.T) {
	root := library(t)
	systemFiles := []string{
		".DS_Store", "Thumbs.db", "Trip/desktop.ini",
		"Trip/._IMG_1.jpg", "Trip/._IMG_1 (1).jpg", // identical, so they would look like duplicate photos
		"photo_cleanup-windows-x64.exe", "photo_cleanup-macos",
	}
	systemDirs := []string{
		".Spotlight-V100/Store-V2/index", ".fseventsd/0000", ".Trashes/501/old.jpg",
		".Trash-1000/files/old.txt", "$RECYCLE.BIN/S-1-5-21/$R1.jpg",
		"System Volume Information/tracking.log", "@eaDir/IMG_1.jpg/SYNOPHOTO_THUMB_M.jpg",
	}
	for _, rel := range append(systemFiles, systemDirs...) {
		write(t, root, rel, "system")
	}

	runOK(t, root, "--apply")
	for _, rel := range append(systemFiles, systemDirs...) {
		if !exist(root, rel) {
			t.Errorf("%s was moved", rel)
		}
	}
	if !strings.Contains(read(t, root, "_Quarantine/summary.txt"), "skipped (system)  : 14") {
		t.Error("summary should count 7 system files and 7 system folders")
	}
	if got := len(readCSV(t, root, "_Quarantine/not-media.csv")); got != 3 {
		t.Errorf("not-media.csv has %d lines, want the header and 2 files", got)
	}
}

func TestNeverMovesItself(t *testing.T) {
	root := library(t)
	tool := write(t, root, "Tools/cleanup.bin", "the program, renamed")
	info, err := os.Stat(tool)
	if err != nil {
		t.Fatal(err)
	}
	saved := self
	self = info
	defer func() { self = saved }()

	runOK(t, root, "--apply")
	if !exist(root, "Tools/cleanup.bin") {
		t.Error("the program quarantined itself")
	}
}

// ---------------------------------------------------------------------------
// finding duplicates

func TestOnlyByteIdenticalFilesAreDuplicates(t *testing.T) {
	root := t.TempDir()
	data := make([]byte, 300_000)
	rand.Read(data)
	for _, dir := range []string{"a", "b", "c"} {
		write(t, root, dir+"/IMG_1.jpg", string(data))
	}
	// Same size and same first/last 64 KB: only the full hash tells it apart.
	tweaked := bytes.Clone(data)
	tweaked[150_000] ^= 1
	write(t, root, "d/IMG_1.jpg", string(tweaked))
	write(t, root, "e/IMG_9.jpg", "something else")

	groups := findDuplicates(scan(root, quiet()).media, quiet())
	if len(groups) != 1 || len(groups[0].losers) != 2 {
		t.Fatalf("got %d groups", len(groups))
	}
	got := rels(append([]*file{groups[0].keeper}, groups[0].losers...))
	if want := []string{"a/IMG_1.jpg", "b/IMG_1.jpg", "c/IMG_1.jpg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("group %q, want %q", got, want)
	}
}

func TestHardLinksAreOneFileNotDuplicates(t *testing.T) {
	root := t.TempDir()
	photo := write(t, root, "IMG_1.jpg", "same bytes")
	os.Mkdir(filepath.Join(root, "Album"), 0o755)
	if err := os.Link(photo, filepath.Join(root, "Album", "IMG_1.jpg")); err != nil {
		t.Skip("cannot create hard links here:", err)
	}
	media := scan(root, quiet()).media
	if len(media) != 2 {
		t.Fatalf("found %d files", len(media))
	}
	if groups := findDuplicates(media, quiet()); len(groups) != 0 {
		t.Errorf("hard links reported as %d duplicate groups", len(groups))
	}
}

func skipWithoutPermissions(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
}

func TestUnreadableFileIsReportedAndLeftAlone(t *testing.T) {
	skipWithoutPermissions(t)
	root := t.TempDir()
	write(t, root, "a/IMG_1.jpg", "same")
	locked := write(t, root, "b/IMG_1.jpg", "same")
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o644)

	var errOut bytes.Buffer
	if groups := findDuplicates(scan(root, quiet()).media, &reporter{out: io.Discard, errOut: &errOut}); len(groups) != 0 {
		t.Errorf("got %d groups", len(groups))
	}
	if !strings.Contains(errOut.String(), "cannot read") {
		t.Errorf("no warning in %q", errOut.String())
	}
}

func TestUnreadableFolderIsReportedAndSkipped(t *testing.T) {
	skipWithoutPermissions(t)
	root := t.TempDir()
	write(t, root, "IMG_1.jpg", "x")
	locked := filepath.Dir(write(t, root, "Private/IMG_2.jpg", "x"))
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o755)

	var errOut bytes.Buffer
	res := scan(root, &reporter{out: io.Discard, errOut: &errOut})
	if len(res.media) != 1 || res.skipped != 1 {
		t.Errorf("media %q, skipped %d", rels(res.media), res.skipped)
	}
	if !strings.Contains(errOut.String(), "cannot read folder") {
		t.Errorf("no warning in %q", errOut.String())
	}
}

// ---------------------------------------------------------------------------
// choosing the copy to keep

func fake(rel string, mtime int64) *file {
	return &file{rel: rel, mtime: mtime}
}

func TestKeeperRulesAndReasons(t *testing.T) {
	// Each `other` would win on every rule below the one that decides.
	for _, tc := range []struct {
		name        string
		kept, other *file
		reason      string
	}{
		{"backup folder", fake("Wedding/IMG_1.jpg", 0), fake("Backup/2019/IMG_1.jpg", 0),
			"other copy sits in a backup/DCIM folder"},
		{"DCIM folder", fake("Trip/IMG_1.jpg", 0), fake("DCIM/100APPLE/IMG_1.jpg", 0),
			"other copy sits in a backup/DCIM folder"},
		{"copy name", fake("Trip/IMG_1.jpg", 0), fake("Trip/Day 1/IMG_1 (1).jpg", 0),
			"other copy is named like a copy"},
		{"Danish copy name", fake("Trip/IMG_1.jpg", 0), fake("Trip/Day 1/IMG_1 - kopi.jpg", 0),
			"other copy is named like a copy"},
		{"deeper folder", fake("Summer/IMG_1.jpg", 0), fake("IMG_1.jpg", 0),
			"this copy is in a more specific folder"},
		{"older", fake("B/IMG_1.jpg", 1000), fake("A/IMG_1.jpg", 2000),
			"this copy is older"},
		{"name", fake("A/IMG_1.jpg", 0), fake("b/IMG_1.jpg", 0),
			"identical in every way, first by name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ranked := []*file{tc.other, tc.kept}
			sortForKeeping(ranked)
			if ranked[0] != tc.kept {
				t.Errorf("kept %s, want %s", ranked[0].rel, tc.kept.rel)
			}
			if got := whyKept(tc.kept, tc.other); got != tc.reason {
				t.Errorf("reason %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestFoldersAboveTheScannedFolderDoNotCount(t *testing.T) {
	// The whole library sits in Downloads, which is also a "dump" folder name.
	root := filepath.Join(t.TempDir(), "Downloads", "Billeder")
	write(t, root, "Wedding/IMG_1.jpg", "same")
	write(t, root, "Backup/Phone/IMG_1.jpg", "same")

	runOK(t, root)
	rows := readCSV(t, root, "_Quarantine/duplicates.csv")
	if got, want := rows[1][1:3], []string{"KEPT", filepath.FromSlash("Wedding/IMG_1.jpg")}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// preview, apply, undo

func TestPreviewWritesReportsAndMovesNothing(t *testing.T) {
	root := library(t)
	before := files(t, root)
	out := runOK(t, root)
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("files changed: %v", got)
	}
	if got := quarantineListing(t, root); !reflect.DeepEqual(got, reports) {
		t.Errorf("quarantine holds %q", got)
	}
	if !strings.Contains(out, "Nothing was moved") {
		t.Error(out)
	}
}

func TestApplyMovesIntoQuarantineUnderTheSameRelativePath(t *testing.T) {
	root := library(t)
	runOK(t, root, "--apply")
	if got, want := files(t, root), map[string]string{"Trip/IMG_1.jpg": "photo one", "Trip/IMG_2.jpg": "photo two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("left %v", got)
	}
	for rel, want := range map[string]string{
		"_Quarantine/duplicates/Trip/IMG_1 (1).jpg": "photo one",
		"_Quarantine/not-media/Notes/notes.txt":     "packing list",
		"_Quarantine/not-media/empty.jpg":           "",
	} {
		if got := read(t, root, rel); got != want {
			t.Errorf("%s holds %q", rel, got)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "Notes")); err != nil || !info.IsDir() {
		t.Error("folders must stay, even when emptied")
	}
	if got := len(journalLines(t, root)); got != 3 {
		t.Errorf("journal has %d lines", got)
	}
	if !strings.Contains(read(t, root, "_Quarantine/summary.txt"), "mode              : APPLIED") {
		t.Error("summary does not say APPLIED")
	}
}

func TestUndoPutsEverythingBack(t *testing.T) {
	root := library(t)
	before := files(t, root)
	runOK(t, root, "--apply")
	runOK(t, root, "--undo")
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
	// the journal and the emptied quarantine folders are gone, the reports stay
	if got := quarantineListing(t, root); !reflect.DeepEqual(got, reports) {
		t.Errorf("quarantine holds %q", got)
	}
}

func TestSecondApplyLeavesTheQuarantineAlone(t *testing.T) {
	root := library(t)
	runOK(t, root, "--apply")
	kept, journal := files(t, root), journalLines(t, root)

	runOK(t, root, "--apply")
	if got := files(t, root); !reflect.DeepEqual(got, kept) {
		t.Errorf("second run changed %v", got)
	}
	if got := journalLines(t, root); !reflect.DeepEqual(got, journal) {
		t.Errorf("second run moved %q", got[len(journal):])
	}
}

func TestApplyNeverOverwritesAFileAlreadyInQuarantine(t *testing.T) {
	root := library(t)
	write(t, root, "_Quarantine/not-media/Notes/notes.txt", "earlier run")
	runOK(t, root, "--apply")
	if got := read(t, root, "_Quarantine/not-media/Notes/notes.txt"); got != "earlier run" {
		t.Errorf("overwritten with %q", got)
	}
	if got := read(t, root, "_Quarantine/not-media/Notes/notes~2.txt"); got != "packing list" {
		t.Errorf("notes~2.txt holds %q", got)
	}

	runOK(t, root, "--undo")
	if got := read(t, root, "Notes/notes.txt"); got != "packing list" {
		t.Errorf("restored %q", got)
	}
	if got := read(t, root, "_Quarantine/not-media/Notes/notes.txt"); got != "earlier run" {
		t.Errorf("earlier file now holds %q", got)
	}
}

func TestUndoNeverOverwritesAFileThatCameBack(t *testing.T) {
	root := library(t)
	runOK(t, root, "--apply")
	write(t, root, "Notes/notes.txt", "new notes")
	_, _, errOut := runCLI(root, "--undo")
	if got := read(t, root, "Notes/notes.txt"); got != "new notes" {
		t.Errorf("overwritten with %q", got)
	}
	if got := read(t, root, "_Quarantine/not-media/Notes/notes.txt"); got != "packing list" {
		t.Errorf("quarantined copy holds %q", got)
	}
	if !strings.Contains(errOut, "exists again") {
		t.Error(errOut)
	}
}

func TestPartialUndoKeepsWhatItCouldNotPutBack(t *testing.T) {
	root := library(t)
	before := files(t, root)
	runOK(t, root, "--apply")
	write(t, root, "Notes/notes.txt", "new notes")

	out := runOK(t, root, "--undo")
	if !strings.Contains(out, "restored 2, already gone 0, left alone 1") {
		t.Error(out)
	}
	journal := journalLines(t, root)
	if len(journal) != 1 || !strings.Contains(journal[0], "Notes/notes.txt") {
		t.Fatalf("journal should hold just the blocked file, has %q", journal)
	}

	os.Remove(filepath.Join(root, "Notes", "notes.txt"))
	runOK(t, root, "--undo")
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after the second undo %v", got)
	}
	if exist(root, "_Quarantine/"+journalName) {
		t.Error("journal should be gone once everything is back")
	}
}

func TestUndoWorksAfterTheFolderMoved(t *testing.T) {
	root := library(t)
	before := files(t, root)
	runOK(t, root, "--apply")
	for _, line := range journalLines(t, root) {
		var e journalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil || filepath.IsAbs(e.From) || filepath.IsAbs(e.To) ||
			strings.Contains(line, `\`) {
			t.Errorf("journal paths must be relative with / separators: %s", line)
		}
	}

	// Like a USB drive that comes back as "/Volumes/32 GB 1" or another drive letter.
	moved := filepath.Join(filepath.Dir(root), "remounted")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	runOK(t, moved, "--undo")
	if got := files(t, moved); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
}

func TestUndoReadsJournalsFromThePythonVersion(t *testing.T) {
	root := library(t)
	from := filepath.Join(root, "Notes", "notes.txt")
	to := filepath.Join(root, quarantine, otherDir, "Notes", "notes.txt")
	os.MkdirAll(filepath.Dir(to), 0o755)
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]string{"from": from, "to": to}) // absolute, as Python wrote them
	write(t, root, "_Quarantine/"+journalName, string(line)+"\n")

	runOK(t, root, "--undo")
	if got := read(t, root, "Notes/notes.txt"); got != "packing list" {
		t.Errorf("restored %q", got)
	}
}

func TestUndoWithNothingToUndo(t *testing.T) {
	code, out, _ := runCLI(t.TempDir(), "--undo")
	if code != 1 || !strings.Contains(out, "Nothing to undo") {
		t.Errorf("exit %d: %s", code, out)
	}
}

func TestUniquifyFindsTheNextFreeName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", "")
	write(t, dir, "notes~2.txt", "")
	write(t, dir, "README", "")
	full, rel := uniquify(filepath.Join(dir, "notes.txt"), "_Quarantine/not-media/notes.txt")
	if full != filepath.Join(dir, "notes~3.txt") || rel != "_Quarantine/not-media/notes~3.txt" {
		t.Errorf("got %s, %s", full, rel)
	}
	if full, _ := uniquify(filepath.Join(dir, "README"), "README"); full != filepath.Join(dir, "README~2") {
		t.Errorf("got %s", full)
	}
}

func TestProgressCoversEveryPhase(t *testing.T) {
	root := library(t)
	var phases []string
	last := map[string][2]int64{}
	r := quiet()
	r.step = func(phase string, done, total int64, item string) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
		last[phase] = [2]int64{done, total}
	}
	p, ok := prepare(root, r)
	if !ok {
		t.Fatal("stopped")
	}
	p.apply(r)
	if want := []string{"scan", "fingerprint", "hash", "move"}; !reflect.DeepEqual(phases, want) {
		t.Errorf("phases %q, want %q", phases, want)
	}
	if got := last["hash"]; got != [2]int64{18, 18} { // hashing counts bytes: two 9-byte photos
		t.Errorf("hash progress ended at %v", got)
	}
	if got := last["move"]; got != [2]int64{3, 3} {
		t.Errorf("move progress ended at %v", got)
	}
}

func TestStoppingAMoveLeavesAnUndoableJournal(t *testing.T) {
	root := library(t)
	before := files(t, root)
	p, _ := prepare(root, quiet())
	checks := 0
	r := quiet()
	r.stop = func() bool { checks++; return checks > 2 }
	if code := p.apply(r); code != 130 || p.moved != 2 || !p.stopped {
		t.Fatalf("exit %d after %d moves", code, p.moved)
	}
	if got := len(journalLines(t, root)); got != 2 {
		t.Errorf("journal has %d lines, want 2", got)
	}
	runOK(t, root, "--undo")
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
}

func TestStoppingAScanChangesNothing(t *testing.T) {
	root := library(t)
	r := quiet()
	r.stop = func() bool { return true }
	if p, ok := prepare(root, r); ok || p != nil {
		t.Fatal("a stopped scan must not make a plan")
	}
	if exist(root, quarantine) {
		t.Error("a stopped scan must not write anything")
	}
}

func TestLeaveOutKeepsChosenFilesInPlace(t *testing.T) {
	root := library(t)
	p, _ := prepare(root, quiet())
	p.leaveOut(true, setOf("(empty)"))
	if len(p.moves) != 1 || p.moves[0].from != "Notes/notes.txt" {
		t.Fatalf("moves %+v", p.moves)
	}
	p.apply(quiet())
	if !exist(root, "Trip/IMG_1 (1).jpg") || !exist(root, "empty.jpg") || exist(root, "Notes/notes.txt") {
		t.Errorf("left %v", files(t, root))
	}
}

// ---------------------------------------------------------------------------
// reports

func TestReportsExplainEveryPlannedMove(t *testing.T) {
	root := library(t)
	runOK(t, root)

	want := [][]string{
		{"group", "role", "path", "size", "reason"},
		{"1", "KEPT", filepath.FromSlash("Trip/IMG_1.jpg"), "9", "other copy is named like a copy"},
		{"1", "quarantined", filepath.FromSlash("Trip/IMG_1 (1).jpg"), "9", "byte-for-byte identical"},
	}
	if got := readCSV(t, root, "_Quarantine/duplicates.csv"); !reflect.DeepEqual(got, want) {
		t.Errorf("duplicates.csv\n%q\nwant\n%q", got, want)
	}
	want = [][]string{
		{"path", "size", "extension"},
		{"empty.jpg", "0", "jpg"},
		{filepath.FromSlash("Notes/notes.txt"), "12", "txt"},
	}
	if got := readCSV(t, root, "_Quarantine/not-media.csv"); !reflect.DeepEqual(got, want) {
		t.Errorf("not-media.csv\n%q\nwant\n%q", got, want)
	}
	summary := read(t, root, "_Quarantine/summary.txt")
	for _, line := range []string{
		"mode              : preview only, nothing moved",
		"duplicate files   : 1  (9 B)",
		"non-media files   : 2  (12 B)",
		"total to move     : 3  (21 B)",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("summary is missing %q:\n%s", line, summary)
		}
	}
}

func TestCSVRowReadsBackWithEncodingCSV(t *testing.T) {
	values := []any{"Trip/notes, misc.txt", `say "cheese".jpg`, "two\nlines", 12}
	got, err := csv.NewReader(strings.NewReader(csvRow(values...))).Read()
	if want := []string{"Trip/notes, misc.txt", `say "cheese".jpg`, "two\nlines", "12"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestHuman(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 1280: "1.2 KB",
		5 << 30: "5.0 GB", 2 << 50: "2048.0 TB",
	} {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestShellQuoteSurvivesTheShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		if got := shellQuote(`C:\My Photos`); got != `"C:\My Photos"` {
			t.Errorf("got %s", got)
		}
		if got := shellQuote(`D:\Photos`); got != `D:\Photos` {
			t.Errorf("got %s", got)
		}
		return
	}
	for _, s := range []string{"/plain/path", "/Volumes/32 GB", "/Users/me/Mom's photos", "/Users/me/Mom's_photos", "$HOME"} {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q came back as %q (%v)", s, out, err)
		}
	}
}

// ---------------------------------------------------------------------------
// command line

func TestBadArgumentsPrintUsageAndTouchNothing(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{}, {"one", "two"}, {root, "--apply", "--undo"}, {root, "--aply"}} {
		code, out, errOut := runCLI(args...)
		if code != 2 || !strings.Contains(out+errOut, "Usage:") {
			t.Errorf("%q: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	if exist(root, quarantine) {
		t.Error("bad arguments must not create the quarantine")
	}
}

func TestHelp(t *testing.T) {
	if code, out, _ := runCLI("--help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("exit %d: %s", code, out)
	}
}

func TestMissingFolderIsAnError(t *testing.T) {
	code, _, errOut := runCLI(filepath.Join(t.TempDir(), "nope"))
	if code != 2 || !strings.Contains(errOut, "Not a folder") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestRunsAsAProgram(t *testing.T) {
	root := library(t)
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{root}, 0, "Nothing was moved"},
		{[]string{root, "--bogus"}, 2, "Unknown option --bogus"},
		{[]string{"--gui", "--apply"}, 2, "Usage:"},
	} {
		cmd := exec.Command(os.Args[0], tc.args...)
		cmd.Env = append(os.Environ(), "PHOTO_CLEANUP_RUN_MAIN=1")
		out, _ := cmd.CombinedOutput()
		if code := cmd.ProcessState.ExitCode(); code != tc.code || !strings.Contains(string(out), tc.says) {
			t.Errorf("%q: exit %d\n%s", tc.args, code, out)
		}
	}
}

// ---------------------------------------------------------------------------
// double-click mode

func TestInteractiveCleanUpAsksBeforeMoving(t *testing.T) {
	root := library(t)
	before := files(t, root)

	code, out := interact(root + "\n1\nn\n")
	if code != 0 || !strings.Contains(out, "Move these 3 files") || !strings.Contains(out, "Nothing was moved") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("moved files without a yes: %v", got)
	}

	code, out = interact(root + "\n\ny\n") // Enter picks 1
	if code != 0 || !strings.Contains(out, "Done.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got, want := files(t, root), map[string]string{"Trip/IMG_1.jpg": "photo one", "Trip/IMG_2.jpg": "photo two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("left %v", got)
	}
	if !strings.Contains(read(t, root, "_Quarantine/summary.txt"), "APPLIED") {
		t.Error("summary was not updated after moving")
	}
}

func TestInteractiveUndo(t *testing.T) {
	root := library(t)
	before := files(t, root)
	runOK(t, root, "--apply")
	if code, out := interact(root + "\n2\n"); code != 0 || !strings.Contains(out, "restored 3") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
}

func TestInteractiveAsksAgainForAFolder(t *testing.T) {
	root := library(t)
	code, out := interact(filepath.Join(root, "nope") + "\n\n" + root + "\n1\nn\n")
	if code != 0 || !strings.Contains(out, "Not a folder") || !strings.Contains(out, "Scanning") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestInteractiveStopsWhenInputEnds(t *testing.T) {
	root := library(t)
	for _, input := range []string{"", root + "\n"} {
		if code, _ := interact(input); code != 2 {
			t.Errorf("%q: exit %d, want 2", input, code)
		}
	}
	if got := files(t, root); len(got) != 5 {
		t.Errorf("files changed: %v", got)
	}
}

func TestInteractiveWithNothingToMove(t *testing.T) {
	root := t.TempDir()
	write(t, root, "IMG_1.jpg", "only photo")
	if code, out := interact(root + "\n1\n"); code != 0 || !strings.Contains(out, "Nothing to move") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestDroppedPath(t *testing.T) {
	cases := map[string]string{ // Windows Terminal and the old console
		`"C:\Users\Me\My Pictures"`: `C:\Users\Me\My Pictures`,
		`  C:\Photos  `:             `C:\Photos`,
	}
	if runtime.GOOS != "windows" {
		cases = map[string]string{
			`/Volumes/32\ GB/Photos`:       `/Volumes/32 GB/Photos`, // macOS Terminal, iTerm
			`/Users/me/Billeder\ \(2019\)`: `/Users/me/Billeder (2019)`,
			`/Users/me/Påske\ på\ Ærø`:     `/Users/me/Påske på Ærø`,
			`/Users/me/Mom\'s`:             `/Users/me/Mom's`,
			`'/home/me/My Photos' `:        `/home/me/My Photos`, // GNOME Terminal, Konsole
			`'/home/me/Mom'\''s photos'`:   `/home/me/Mom's photos`,
			`"/Users/me/My Photos"`:        `/Users/me/My Photos`,
			"  /plain/path\n":              `/plain/path`,
		}
	}
	for in, want := range cases {
		if got := droppedPath(in); got != want {
			t.Errorf("droppedPath(%q) = %q, want %q", in, got, want)
		}
	}
}
