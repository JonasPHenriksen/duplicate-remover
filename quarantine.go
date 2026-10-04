package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A planned move. Both paths are relative to the scanned folder, with /
// separators, so the journal still works when the drive is mounted elsewhere
// or opened on another operating system.
type move struct {
	from, to string
}

// One line of _Quarantine/journal.jsonl.
type journalEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// plan is what one run found, and what it moves.
type plan struct {
	root          string
	scan          scanResult
	groups        []group
	moves         []move
	moved, failed int
	applied       bool
}

// prepare scans the folder and works out what to move.
func prepare(root string, out, errOut io.Writer) *plan {
	fmt.Fprintf(out, "Scanning %s\n", root)
	res := scan(root, errOut)
	fmt.Fprintf(out, "  %d photos/videos, %d other files\n", len(res.media), len(res.other))
	if res.skipped > 0 {
		fmt.Fprintf(out, "  %d symlinks or unreadable entries skipped\n", res.skipped)
	}
	if res.system > 0 {
		fmt.Fprintf(out, "  %d system files and folders left alone\n", res.system)
	}

	fmt.Fprintln(out, "Looking for duplicates")
	groups := findDuplicates(res.media, out, errOut)

	p := &plan{root: root, scan: res, groups: groups}
	for _, g := range groups {
		for _, f := range g.losers {
			p.moves = append(p.moves, move{f.rel, quarantine + "/" + dupDir + "/" + f.rel})
		}
	}
	for _, f := range res.other {
		p.moves = append(p.moves, move{f.rel, quarantine + "/" + otherDir + "/" + f.rel})
	}
	return p
}

// apply moves the files into the quarantine, recording each move in the journal
// as soon as it is made.
func (p *plan) apply(out, errOut io.Writer) int {
	if len(p.moves) == 0 {
		return 0
	}
	fmt.Fprintf(out, "Moving %d files to %s/\n", len(p.moves), quarantine)
	if err := os.MkdirAll(filepath.Join(p.root, quarantine), 0o755); err != nil {
		fmt.Fprintf(errOut, "  ! cannot create %s: %v\n", quarantine, err)
		return 1
	}
	journal, err := os.OpenFile(filepath.Join(p.root, quarantine, journalName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(errOut, "  ! cannot write the journal: %v\n", err)
		return 1
	}
	defer journal.Close()
	enc := json.NewEncoder(journal) // one unbuffered write per move
	enc.SetEscapeHTML(false)

	pressed, stop := interrupts()
	defer stop()
	p.applied = true
	for _, m := range p.moves {
		if pressed() {
			fmt.Fprintf(errOut, "\ninterrupted after moving %d files, all of them in the journal\n", p.moved)
			return 130
		}
		src, dst, to := full(p.root, m.from), full(p.root, m.to), m.to
		err := os.MkdirAll(filepath.Dir(dst), 0o755)
		if err == nil && exists(dst) {
			dst, to = uniquify(dst, to)
		}
		if err == nil {
			err = os.Rename(src, dst)
		}
		if err != nil {
			fmt.Fprintf(errOut, "  ! could not move %s: %v\n", src, err)
			p.failed++
			continue
		}
		if err := enc.Encode(journalEntry{m.from, to}); err != nil {
			// A move undo cannot see must not happen: put it back and stop.
			os.Rename(dst, src)
			fmt.Fprintf(errOut, "  ! cannot write the journal, stopping: %v\n", err)
			return 1
		}
		p.moved++
		if p.moved%500 == 0 {
			fmt.Fprintf(out, "  moved %d/%d\n", p.moved, len(p.moves))
		}
	}
	if p.failed > 0 {
		fmt.Fprintf(out, "  moved %d, %d failed\n", p.moved, p.failed)
	} else {
		fmt.Fprintf(out, "  moved %d\n", p.moved)
	}
	return 0
}

// undo moves everything in the journal back, newest first. Whatever it cannot
// put back stays in the journal, so undo can be run again later.
func undo(root string, out, errOut io.Writer) int {
	journal := filepath.Join(root, quarantine, journalName)
	entries, err := readJournal(journal)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "Nothing to undo: %s not found.\n", journal)
		return 1
	}
	if err != nil {
		fmt.Fprintf(errOut, "cannot read %s: %v\n", journal, err)
		return 1
	}

	pressed, stop := interrupts()
	defer stop()
	restored := make([]bool, len(entries))
	var nRestored, missing, blocked int
	interrupted := false
	for i := len(entries) - 1; i >= 0; i-- {
		if pressed() {
			interrupted = true
			break
		}
		src, dst := resolve(root, entries[i].From), resolve(root, entries[i].To)
		if !exists(dst) {
			missing++
			continue
		}
		if exists(src) {
			fmt.Fprintf(errOut, "  ! %s exists again, leaving %s in quarantine\n", src, dst)
			blocked++
			continue
		}
		err := os.MkdirAll(filepath.Dir(src), 0o755)
		if err == nil {
			err = os.Rename(dst, src)
		}
		if err != nil {
			fmt.Fprintf(errOut, "  ! could not restore %s: %v\n", dst, err)
			blocked++
			continue
		}
		restored[i] = true
		nRestored++
	}

	// clean up the now-empty quarantine skeleton, keeping the reports
	removeEmptyDirs(filepath.Join(root, quarantine))
	if nRestored > 0 {
		if err := keepUnrestored(journal, entries, restored); err != nil {
			fmt.Fprintf(errOut, "  ! could not update %s: %v\n", journal, err)
		}
	}

	if interrupted {
		fmt.Fprintln(errOut, "\ninterrupted")
	}
	fmt.Fprintf(out, "\nrestored %d, already gone %d, left alone %d\n", nRestored, missing, blocked)
	if blocked > 0 || missing > 0 {
		fmt.Fprintf(out, "Check %s/ for anything that could not be put back.\n", quarantine)
	}
	if blocked > 0 {
		fmt.Fprintln(out, "Those files are still in the journal: undo again once their places are free.")
	}
	if interrupted {
		return 130
	}
	return 0
}

func readJournal(journal string) ([]journalEntry, error) {
	data, err := os.ReadFile(journal)
	if err != nil {
		return nil, err
	}
	var entries []journalEntry
	for _, line := range strings.Split(string(data), "\n") {
		var e journalEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.From != "" && e.To != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// keepUnrestored rewrites the journal with only the entries that were not put
// back, or deletes it when everything was.
func keepUnrestored(journal string, entries []journalEntry, restored []bool) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for i, e := range entries {
		if !restored[i] {
			enc.Encode(e)
		}
	}
	if buf.Len() == 0 {
		return os.Remove(journal)
	}
	tmp := journal + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, journal)
}

// removeEmptyDirs removes every empty folder under dir, deepest first.
func removeEmptyDirs(dir string) {
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails, harmlessly, unless the folder is empty
	}
}

// full turns a path relative to the scanned folder into a full path.
func full(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// resolve is full for journal entries. Journals written by the old Python
// version hold absolute paths, which are used as they are.
func resolve(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return full(root, p)
}

// exists reports whether anything is at p. When unsure, it says yes, so that
// nothing gets overwritten.
func exists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// uniquify finds a free name next to dst: name~2.ext, name~3.ext, ... and
// returns it both as a full path and relative like rel.
func uniquify(dst, rel string) (string, string) {
	name := filepath.Base(dst)
	st := stem(name)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s~%d%s", st, i, name[len(st):])
		if !exists(filepath.Join(filepath.Dir(dst), candidate)) {
			return filepath.Join(filepath.Dir(dst), candidate), path.Join(path.Dir(rel), candidate)
		}
	}
}
