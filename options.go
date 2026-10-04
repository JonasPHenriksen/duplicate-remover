package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// options are the choices a run can make, with flags on the command line or in
// the Options panel of the page. The zero value is how photo_cleanup works
// without any.
type options struct {
	DuplicatesOnly bool     `json:"duplicatesOnly"` // leave files that aren't photos or videos alone
	SameImage      bool     `json:"sameImage"`      // JPEGs that differ only in their tags count as duplicates
	Similar        string   `json:"similar"`        // "close" or "loose": also list photos that look alike
	KeepNewest     bool     `json:"keepNewest"`     // of otherwise equal copies, keep the newest, not the oldest
	Prefer         []string `json:"prefer"`         // folders inside the one cleaned whose copies always stay
	Backups        []string `json:"backups"`        // more folder names that count as backups, see disposableParts
	Reference      string   `json:"reference"`      // a folder outside it whose photos count as kept already
	Skip           []string `json:"skip"`           // folders inside it that are never looked in
	RemoveEmpty    bool     `json:"removeEmpty"`    // remove the folders a move leaves empty
	OneAtATime     bool     `json:"oneAtATime"`     // read one file at a time, which suits spinning disks
	Cache          bool     `json:"cache"`          // remember fingerprints in _Quarantine between runs
}

// The most files read at once, and how alike two photos' looks must be to
// count as looking alike: the most bits of their 64 that may differ.
const (
	maxJobs    = 8
	closeLooks = 6
	looseLooks = 14
)

// rules are options made ready for a run on one folder, with their folders
// checked. Folders to keep and to skip are known by what the disk says they
// are, not by how their names are written, which can differ from how the disk
// keeps them: in case, or in how accents are written.
type rules struct {
	options
	keep, skip []fs.FileInfo   // the folders to keep and to leave out
	backups    map[string]bool // lowercase folder names
	reference  string          // the reference folder's full path, or ""
	jobs       int             // files read at once
	looks      int             // see closeLooks
}

// defaultRules are the rules without options, for a run on any folder.
var defaultRules, _ = newRules("", options{})

// newRules checks options for a run on root. Folders to keep and to skip can
// be given inside root or relative to it, while the reference folder must be
// outside it. A folder on a spinning disk is read one file at a time.
func newRules(root string, o options) (*rules, error) {
	ru := &rules{options: o, backups: map[string]bool{}, jobs: 1}
	for name := range disposableParts {
		ru.backups[name] = true
	}
	for _, name := range o.Backups {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			ru.backups[name] = true
		}
	}
	if !o.OneAtATime && (root == "" || !diskSpins(root)) {
		ru.jobs = min(runtime.NumCPU(), maxJobs)
	}
	switch o.Similar {
	case "":
	case "close":
		ru.looks = closeLooks
	case "loose":
		ru.looks = looseLooks
	default:
		return nil, fmt.Errorf("photos can look alike close or loose, not %q", o.Similar)
	}
	for _, p := range o.Prefer {
		info, err := inside(root, p)
		if err != nil {
			return nil, fmt.Errorf("the folder to keep %w", err)
		}
		ru.keep = append(ru.keep, info)
	}
	for _, p := range o.Skip {
		info, err := inside(root, p)
		if err != nil {
			return nil, fmt.Errorf("the folder to skip %w", err)
		}
		ru.skip = append(ru.skip, info)
	}
	if o.Reference != "" {
		ref, ok := folder(droppedPath(o.Reference))
		switch {
		case !ok:
			return nil, fmt.Errorf("the reference folder %s is not a folder", ref)
		case holds(root, ref) || holds(ref, root):
			return nil, fmt.Errorf("the reference folder %s must be outside the folder you clean, and not hold it", ref)
		}
		ru.reference = ref
	}
	return ru, nil
}

// inside finds a folder given inside root, or relative to it, and makes sure it
// is inside root, and not root itself.
func inside(root, dir string) (fs.FileInfo, error) {
	dir = droppedPath(dir)
	if !filepath.IsAbs(expandHome(dir)) {
		dir = filepath.Join(root, dir)
	}
	full, ok := folder(dir)
	if !ok {
		return nil, fmt.Errorf("%s is not a folder", full)
	}
	info, err := os.Stat(full)
	if r, rerr := os.Stat(root); err != nil || rerr != nil || os.SameFile(info, r) || !holds(root, full) {
		return nil, fmt.Errorf("%s is not inside the folder you clean", full)
	}
	return info, nil
}

// holds reports whether dir holds path, or is it. It walks up from path and
// asks the disk at each step, rather than compare how the names are written.
func holds(dir, path string) bool {
	d, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := path; ; {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, d) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// within reports whether path is dir or inside it, by their names.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sameAsAny reports whether a folder is one of dirs.
func sameAsAny(info fs.FileInfo, dirs []fs.FileInfo) bool {
	return slices.ContainsFunc(dirs, func(d fs.FileInfo) bool { return os.SameFile(info, d) })
}

// summary lists the options that differ from the defaults, for the reports.
func (ru *rules) summary() string {
	var parts []string
	add := func(on bool, s string) {
		if on {
			parts = append(parts, s)
		}
	}
	add(ru.DuplicatesOnly, "duplicates only")
	add(ru.SameImage, "same picture with other tags")
	add(ru.Similar != "", "look-alikes ("+ru.Similar+")")
	add(ru.KeepNewest, "keep the newest")
	add(len(ru.keep) > 0, "keep folders: "+strings.Join(ru.Prefer, ", "))
	add(len(ru.Backups) > 0, "backup names: "+strings.Join(ru.Backups, ", "))
	add(ru.reference != "", "reference: "+ru.reference)
	add(len(ru.skip) > 0, "skip folders: "+strings.Join(ru.Skip, ", "))
	add(ru.RemoveEmpty, "remove empty folders")
	add(ru.OneAtATime, "one file at a time")
	add(ru.Cache, "remember fingerprints")
	return strings.Join(parts, "; ")
}
