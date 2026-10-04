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
	"slices"
	"sort"
	"strconv"
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
	ru            *rules
	scan          scanResult
	groups        []group // duplicates, whose losers move
	similar       []group // photos that look alike: their losers only move when chosen
	moves         []move
	moved, failed int
	movedBytes    int64
	removed       int // folders removed after the move, as it left them empty
	applied       bool
	stopped       bool
}

// prepare scans the folder and works out what to move. It returns false when
// the run was stopped before it got that far.
func prepare(root string, r *reporter) (*plan, bool) { return prepareWith(root, defaultRules, r) }

// prepareWith is prepare by the rules: what to look in, what to look for, and
// which copy to keep.
func prepareWith(root string, ru *rules, r *reporter) (*plan, bool) {
	r.printf("Scanning %s\n", root)
	res := scanFolder(root, ru.skip, ru.keep, false, r)
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
	if res.leftOut == 1 {
		r.printf("  1 folder left out\n")
	} else if res.leftOut > 1 {
		r.printf("  %d folders left out\n", res.leftOut)
	}
	if ru.DuplicatesOnly {
		res.other = nil
	}
	media := res.media
	if ru.reference != "" {
		r.printf("Scanning the reference folder %s\n", ru.reference)
		refs := scanFolder(ru.reference, nil, nil, true, r)
		if r.stopped() {
			return nil, false
		}
		r.printf("  %d photos/videos\n", len(refs.media))
		media = append(slices.Clip(media), refs.media...)
	}
	var c *cache
	if ru.Cache {
		if c = loadCache(root); len(c.old) > 0 {
			r.printf("  remembers fingerprints of %d files from earlier runs\n", len(c.old))
		}
	}

	r.printf("Looking for duplicates\n")
	groups := ru.findDuplicates(media, c, r)
	if r.stopped() {
		return nil, false
	}
	if ru.SameImage {
		r.printf("Comparing JPEGs without their tags\n")
		if groups = ru.sameImages(groups, media, c, r); r.stopped() {
			return nil, false
		}
	}
	var similar []group
	if ru.Similar != "" {
		r.printf("Looking for photos that look alike\n")
		if similar = ru.lookAlikes(groups, media, c, r); r.stopped() {
			return nil, false
		}
		r.printf("  %d groups of photos look alike\n", len(similar))
	}
	if err := c.save(root, media); err != nil {
		r.warnf("  ! could not remember the fingerprints: %v\n", err)
	}
	return &plan{root: root, ru: ru, scan: res, groups: groups, similar: similar,
		moves: planMoves(groups, nil, res.other)}, true
}

// planMoves plans to move the losers of the groups and of the look-alikes, and
// the other files, each file once: a photo that is a duplicate's loser and a
// look-alike's too moves with the duplicates.
func planMoves(groups, similar []group, other []*file) []move {
	var moves []move
	planned := map[string]bool{}
	add := func(f *file, dir string) {
		if !planned[f.rel] {
			planned[f.rel] = true
			moves = append(moves, move{f.rel, quarantine + "/" + dir + "/" + f.rel, f.size})
		}
	}
	for _, g := range groups {
		for _, f := range g.losers {
			add(f, dupDir)
		}
	}
	for _, g := range similar {
		for _, f := range g.losers {
			add(f, similarDir)
		}
	}
	for _, f := range other {
		add(f, otherDir)
	}
	return moves
}

// leaveOut takes files out of the plan before it is applied: every duplicate,
// and the non-media files whose group (see groupOf) is in groups.
func (p *plan) leaveOut(duplicates bool, groups map[string]bool) {
	var types []string
	for t := range groups {
		types = append(types, t)
	}
	p.choose(choices{SkipDuplicates: duplicates, SkipTypes: types})
}

// choices are what the page changes in a plan before it is carried out. Groups
// are numbered as the preview lists them, and their members too: 0 is the one
// that stays, and the copies follow from 1.
type choices struct {
	SkipDuplicates bool           `json:"skipDuplicates"` // every duplicate stays where it is
	SkipTypes      []string       `json:"skipTypes"`      // the non-media files of these groups (see groupOf) stay
	SkipGroups     []int          `json:"skipGroups"`     // these duplicate groups stay
	Keep           map[string]int `json:"keep"`           // duplicate group → the member to keep instead
	Similar        []int          `json:"similar"`        // these look-alike groups move all but one
	KeepSimilar    map[string]int `json:"keepSimilar"`    // look-alike group → the member to keep instead
	RemoveEmpty    *bool          `json:"removeEmpty"`    // instead of the option the scan had, if given
}

// check reports choices that don't fit the plan.
func (p *plan) check(c choices) error {
	bad := func(what string, i int) error { return fmt.Errorf("there is no %s %d", what, i) }
	for _, i := range c.SkipGroups {
		if i < 0 || i >= len(p.groups) {
			return bad("group", i)
		}
	}
	for _, i := range c.Similar {
		if i < 0 || i >= len(p.similar) {
			return bad("look-alike group", i)
		}
	}
	for what, keep := range map[string]map[string]int{"group": c.Keep, "look-alike group": c.KeepSimilar} {
		groups := p.groups
		if what != "group" {
			groups = p.similar
		}
		for key, m := range keep {
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(groups) {
				return fmt.Errorf("there is no %s %q", what, key)
			}
			if m < 0 || m > len(groups[i].losers) {
				return fmt.Errorf("%s %d has no member %d", what, i, m)
			}
		}
	}
	return nil
}

// choose changes the plan as c says, which check must have passed.
func (p *plan) choose(c choices) {
	var groups []group
	for i, g := range p.groups {
		if c.SkipDuplicates || slices.Contains(c.SkipGroups, i) {
			continue
		}
		if g = g.keepInstead(c.Keep[strconv.Itoa(i)]); len(g.losers) > 0 {
			groups = append(groups, g)
		}
	}
	var similar []group
	for i, g := range p.similar {
		if !slices.Contains(c.Similar, i) {
			continue
		}
		if g = g.keepInstead(c.KeepSimilar[strconv.Itoa(i)]); len(g.losers) > 0 {
			similar = append(similar, g)
		}
	}
	types := setOf(c.SkipTypes...)
	var other []*file
	for _, f := range p.scan.other {
		if !types[groupOf(f)] {
			other = append(other, f)
		}
	}
	p.groups, p.similar, p.scan.other = groups, similar, other
	p.moves = planMoves(groups, similar, other)
}

// keepInstead keeps member m of the group, 1 or more being its losers, instead
// of its keeper, which then goes, unless it may not move.
func (g group) keepInstead(m int) group {
	if m < 1 || m > len(g.losers) {
		return g
	}
	out := group{keeper: g.losers[m-1], kind: g.kind, chosen: true}
	if movable(g.keeper) {
		out.losers = append(out.losers, g.keeper)
	}
	for i, f := range g.losers {
		if i != m-1 {
			out.losers = append(out.losers, f)
		}
	}
	return out
}

// removeLeftEmpty removes the folders the move emptied, the deepest first, up
// to but never the scanned folder itself. Folders that still hold anything
// stay, hidden files such as .DS_Store included. Undo makes the folders again
// as it puts files back into them.
func (p *plan) removeLeftEmpty() int {
	pending := map[string]bool{}
	for _, m := range p.moves {
		if dir := path.Dir(m.from); dir != "." {
			pending[dir] = true
		}
	}
	removed := 0
	for len(pending) > 0 {
		dirs := make([]string, 0, len(pending))
		for dir := range pending {
			dirs = append(dirs, dir)
		}
		sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
		pending = map[string]bool{}
		for _, dir := range dirs {
			fp := full(p.root, dir)
			if info, err := os.Lstat(fp); err != nil || !info.IsDir() || os.Remove(fp) != nil {
				continue
			}
			removed++
			if parent := path.Dir(dir); parent != "." {
				pending[parent] = true
			}
		}
	}
	return removed
}

type emptyResult struct {
	deleted, missing, failed, kept int
	bytes                          int64
	stopped                        bool
}

// errChanged is what emptyQuarantine says when the journal no longer has the
// number of lines the person was asked about.
var errChanged = errors.New("_Quarantine changed since you were asked, so nothing was deleted: ask again")

// emptyQuarantine deletes, for good, the files the journal says clean-ups moved
// into _Quarantine, and their lines in the journal. It is the one thing
// photo_cleanup deletes, and it deletes nothing else: only plain files in the
// folders inside _Quarantine, so never the reports, the journal or anything
// the journal points to outside it. expect, when not 0, is the number of lines
// the journal had when the person was asked; if it has another, nothing is
// deleted. The error wraps fs.ErrNotExist when there is no journal.
func emptyQuarantine(root string, expect int, r *reporter) (emptyResult, error) {
	var res emptyResult
	dir := filepath.Join(root, quarantine)
	journal := filepath.Join(dir, journalName)
	entries, err := readJournal(journal)
	if err != nil {
		return res, err
	}
	if expect != 0 && len(entries) != expect {
		return res, errChanged
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return res, err
	}
	gone := make([]bool, len(entries))
	for i, e := range entries {
		if r.stopped() {
			res.stopped = true
			break
		}
		r.progress("empty", int64(i), int64(len(entries)), e.To)
		p := filepath.Clean(resolve(root, e.To))
		parent, err := filepath.EvalSymlinks(filepath.Dir(p))
		if !within(p, dir) || filepath.Dir(p) == dir || p == dir || err == nil && !within(parent, real) {
			res.kept++
			continue
		}
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.missing++
			gone[i] = true
		case err != nil || !info.Mode().IsRegular():
			r.warnf("  ! not deleting %s, which is not a plain file\n", p)
			res.failed++
		default:
			if err := os.Remove(p); err != nil {
				r.warnf("  ! could not delete %s: %v\n", p, err)
				res.failed++
				continue
			}
			res.deleted++
			res.bytes += info.Size()
			gone[i] = true
		}
	}
	if !res.stopped {
		r.progress("empty", int64(len(entries)), int64(len(entries)), "")
	}
	removeEmptyDirs(dir)
	if err := keepUnrestored(journal, entries, gone); err != nil {
		r.warnf("  ! could not update %s: %v\n", journal, err)
	}
	return res, nil
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
