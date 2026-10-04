package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	size     int64
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
	movedBytes    int64
	applied       bool
	stopped       bool
}

// prepare scans the folder and works out what to move. It returns false when
// the run was stopped before it got that far.
func prepare(root string, r *reporter) (*plan, bool) {
	r.printf("Scanning %s\n", root)
	res := scan(root, r)
	if r.stopped() {
		return nil, false
	}
	r.printf("  %d photos/videos, %d other files\n", len(res.media), len(res.other))
	if res.skipped > 0 {
		r.printf("  %d symlinks or unreadable entries skipped\n", res.skipped)
	}
	if res.system > 0 {
		r.printf("  %d system files and folders left alone\n", res.system)
	}

	r.printf("Looking for duplicates\n")
	groups := findDuplicates(res.media, r)
	if r.stopped() {
		return nil, false
	}
	return &plan{root: root, scan: res, groups: groups, moves: planMoves(groups, res.other)}, true
}

func planMoves(groups []group, other []*file) []move {
	var moves []move
	for _, g := range groups {
		for _, f := range g.losers {
			moves = append(moves, move{f.rel, quarantine + "/" + dupDir + "/" + f.rel, f.size})
		}
	}
	for _, f := range other {
		moves = append(moves, move{f.rel, quarantine + "/" + otherDir + "/" + f.rel, f.size})
	}
	return moves
}

// leaveOut takes files out of the plan before it is applied: every duplicate,
// and the non-media files whose group (see groupOf) is in groups.
func (p *plan) leaveOut(duplicates bool, groups map[string]bool) {
	if duplicates {
		p.groups = nil
	}
	var other []*file
	for _, f := range p.scan.other {
		if !groups[groupOf(f)] {
			other = append(other, f)
		}
	}
	p.scan.other = other
	p.moves = planMoves(p.groups, other)
}

// apply moves the files into the quarantine, recording each move in the journal
// as soon as it is made.
func (p *plan) apply(r *reporter) int {
	if len(p.moves) == 0 {
		return 0
	}
	r.printf("Moving %d files to %s/\n", len(p.moves), quarantine)
	if err := os.MkdirAll(filepath.Join(p.root, quarantine), 0o755); err != nil {
		r.warnf("  ! cannot create %s: %v\n", quarantine, err)
		return 1
	}
	journal, err := os.OpenFile(filepath.Join(p.root, quarantine, journalName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.warnf("  ! cannot write the journal: %v\n", err)
		return 1
	}
	defer journal.Close()
	enc := json.NewEncoder(journal) // one unbuffered write per move
	enc.SetEscapeHTML(false)

	p.applied = true
	for i, m := range p.moves {
		if r.stopped() {
			p.stopped = true
			r.warnf("\ninterrupted after moving %d files, all of them in the journal\n", p.moved)
			return 130
		}
		r.progress("move", int64(i), int64(len(p.moves)), m.from)
		src, dst, to := full(p.root, m.from), full(p.root, m.to), m.to
		err := os.MkdirAll(filepath.Dir(dst), 0o755)
		if err == nil && exists(dst) {
			dst, to = uniquify(dst, to)
		}
		if err == nil {
			err = os.Rename(src, dst)
		}
		if err != nil {
			r.warnf("  ! could not move %s: %v\n", src, err)
			p.failed++
			continue
		}
		if err := enc.Encode(journalEntry{m.from, to}); err != nil {
			// A move undo cannot see must not happen: put it back and stop.
			os.Rename(dst, src)
			r.warnf("  ! cannot write the journal, stopping: %v\n", err)
			return 1
		}
		p.moved++
		p.movedBytes += m.size
		if p.moved%500 == 0 {
			r.printf("  moved %d/%d\n", p.moved, len(p.moves))
		}
	}
	r.progress("move", int64(len(p.moves)), int64(len(p.moves)), "")
	if p.failed > 0 {
		r.printf("  moved %d, %d failed\n", p.moved, p.failed)
	} else {
		r.printf("  moved %d\n", p.moved)
	}
	return 0
}

type undoResult struct {
	restored, missing, blocked int
	stopped                    bool
}

// undo moves everything in the journal back, newest first. Whatever it cannot
// put back stays in the journal, so undo can be run again later. The error
// wraps fs.ErrNotExist when there is no journal.
func undo(root string, r *reporter) (undoResult, error) {
	var res undoResult
	journal := filepath.Join(root, quarantine, journalName)
	entries, err := readJournal(journal)
	if err != nil {
		return res, err
	}

	restored := make([]bool, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if r.stopped() {
			res.stopped = true
			break
		}
		r.progress("undo", int64(len(entries)-1-i), int64(len(entries)), entries[i].From)
		src, dst := resolve(root, entries[i].From), resolve(root, entries[i].To)
		if !exists(dst) {
			res.missing++
			continue
		}
		if exists(src) {
			r.warnf("  ! %s exists again, leaving %s in quarantine\n", src, dst)
			res.blocked++
			continue
		}
		err := os.MkdirAll(filepath.Dir(src), 0o755)
		if err == nil {
			err = os.Rename(dst, src)
		}
		if err != nil {
			r.warnf("  ! could not restore %s: %v\n", dst, err)
			res.blocked++
			continue
		}
		restored[i] = true
		res.restored++
	}

	// clean up the now-empty quarantine skeleton, keeping the reports
	removeEmptyDirs(filepath.Join(root, quarantine))
	if res.restored > 0 {
		if err := keepUnrestored(journal, entries, restored); err != nil {
			r.warnf("  ! could not update %s: %v\n", journal, err)
		}
	}
	return res, nil
}

// undoInTerminal runs undo and reports how it went, for the command line.
func undoInTerminal(root string, r *reporter) int {
	journal := filepath.Join(root, quarantine, journalName)
	res, err := undo(root, r)
	if errors.Is(err, fs.ErrNotExist) {
		r.printf("Nothing to undo: %s not found.\n", journal)
		return 1
	}
	if err != nil {
		r.warnf("cannot read %s: %v\n", journal, err)
		return 1
	}
	if res.stopped {
		r.warnf("\ninterrupted\n")
	}
	r.printf("\nrestored %d, already gone %d, left alone %d\n", res.restored, res.missing, res.blocked)
	if res.blocked > 0 || res.missing > 0 {
		r.printf("Check %s/ for anything that could not be put back.\n", quarantine)
	}
	if res.blocked > 0 {
		r.printf("Those files are still in the journal: undo again once their places are free.\n")
	}
	if res.stopped {
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
