package main

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// A file found by scan.
type file struct {
	path   string // full path
	rel    string // path inside the scanned folder, or the reference folder, always with / separators
	size   int64
	mtime  int64       // modification time in nanoseconds
	info   fs.FileInfo // lets os.SameFile spot hard links
	ref    bool        // in the reference folder: never moved, and kept before any other copy
	inKeep bool        // in a folder to keep: never moved, and kept before copies elsewhere
	tags   int64       // bytes of tags, for JPEGs compared without them
	w, h   int         // size in pixels, for photos compared by how they look
}

// A set of copies of one photo or video: the copy that stays and the ones that go.
type group struct {
	keeper *file
	losers []*file
	kind   string // "" byte-identical, "tags" the same picture with other tags, "alike" looks alike
	chosen bool   // the keeper was chosen by hand
}

type scanResult struct {
	media, other []*file
	skipped      int // links and entries that could not be read
	system       int // system files and folders left alone
	leftOut      int // folders left out on purpose
}

// self is this program's own file, so it never quarantines itself.
var self = func() fs.FileInfo {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	return info
}()

// scan walks the folder once: the files of each folder in name order, then
// its subfolders in name order.
func scan(root string, r *reporter) scanResult { return scanFolder(root, nil, nil, false, r) }

// scanFolder is scan, leaving out the folders in skip and marking the files in
// the folders in keep. The files of a reference folder are marked as such, and
// only its photos and videos count.
func scanFolder(root string, skip, keep []fs.FileInfo, ref bool, r *reporter) scanResult {
	phase := "scan"
	if ref {
		phase = "reference"
	}
	type subdir struct {
		name string
		kept bool
	}
	var res scanResult
	var walk func(dir, rel string, kept bool)
	walk = func(dir, rel string, kept bool) {
		if r.stopped() {
			return
		}
		r.progress(phase, int64(len(res.media)+len(res.other)), 0, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			r.warnf("  ! cannot read folder %s: %v\n", dir, err)
			res.skipped++
		}
		var subdirs []subdir
		for _, e := range entries {
			name := e.Name()
			p := filepath.Join(dir, name)
			t := e.Type()
			switch {
			case t&fs.ModeSymlink != 0:
				// Never follow or move links. A link to a folder is simply not entered.
				if info, err := os.Stat(p); err != nil || !info.IsDir() {
					res.skipped++
				}
				continue
			case t.IsDir():
				switch {
				case isSystemDir(name):
					res.system++
				case strings.EqualFold(name, quarantine): // ours, at any depth
				case len(skip)+len(keep) == 0:
					subdirs = append(subdirs, subdir{name, kept})
				default:
					info, err := e.Info()
					switch {
					case err != nil:
						res.skipped++
					case sameAsAny(info, skip):
						res.leftOut++
					default:
						subdirs = append(subdirs, subdir{name, kept || sameAsAny(info, keep)})
					}
				}
				continue
			case t&fs.ModeIrregular != 0:
				// Windows reparse points: OneDrive files are files, junctions link to folders.
				info, err := os.Stat(p)
				if err != nil {
					res.skipped++
					continue
				}
				if info.IsDir() {
					continue
				}
			case !t.IsRegular():
				continue // pipes, sockets, devices
			}

			info, err := e.Info()
			if err != nil {
				res.skipped++
				continue
			}
			if isSystemFile(name, info) {
				res.system++
				continue
			}
			f := &file{path: p, rel: joinRel(rel, name), size: info.Size(),
				mtime: info.ModTime().UnixNano(), info: info, ref: ref, inKeep: kept}
			if isMedia(name) && f.size > 0 {
				res.media = append(res.media, f)
			} else if !ref {
				res.other = append(res.other, f)
			}
		}
		for _, d := range subdirs {
			walk(filepath.Join(dir, d.name), joinRel(rel, d.name), d.kept)
		}
	}
	walk(root, "", false)
	return res
}

func joinRel(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

func isMedia(name string) bool {
	return mediaExt[extension(name)]
}

func isSystemDir(name string) bool {
	lower := strings.ToLower(name)
	return systemDirs[lower] || strings.HasPrefix(lower, ".trash-")
}

func isSystemFile(name string, info fs.FileInfo) bool {
	lower := strings.ToLower(name)
	return systemFiles[lower] || strings.HasPrefix(name, "._") || strings.HasPrefix(lower, "photo_cleanup") ||
		(self != nil && strings.EqualFold(name, self.Name()) && os.SameFile(info, self))
}

// extension is the lowercase extension without its dot. Like Python's pathlib,
// ".DS_Store" and "photo." have none.
func extension(name string) string {
	return strings.ToLower(strings.TrimPrefix(name[len(stem(name)):], "."))
}

// stem is the name without its extension.
func stem(name string) string {
	if i := strings.LastIndexByte(name, '.'); 0 < i && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// findDuplicates groups byte-identical files. Only files of the same size are
// compared, first by their first and last 64 KB, then by a hash of everything.
// It returns nothing when the run is stopped.
func findDuplicates(media []*file, r *reporter) []group {
	return defaultRules.findDuplicates(media, nil, r)
}

// findDuplicates is findDuplicates by the rules, which read ru.jobs files at
// once. Files of a reference folder take part, but only to tell which files of
// the folder being cleaned are copies of them.
func (ru *rules) findDuplicates(media []*file, c *cache, r *reporter) []group {
	var groups []group
	for _, files := range ru.identical(media, comparison{
		size:       func(f *file) int64 { return f.size },
		open:       func(f *file) (io.ReaderAt, io.Closer, error) { fh, err := os.Open(f.path); return fh, fh, err },
		quickPhase: "fingerprint",
		hashPhase:  "hash",
		cached:     c.full,
		remember:   c.putFull,
		told:       "files share a size with another file; fingerprinting those",
	}, r) {
		if g := ru.group(files, ""); g != nil {
			groups = append(groups, *g)
		}
	}
	if r.stopped() {
		return nil
	}
	return groups
}

// comparison is what identical compares of each file, and how it says so.
type comparison struct {
	size                  func(f *file) int64
	extra                 func(f *file) int64 // more that must be the same, if not nil
	open                  func(f *file) (io.ReaderAt, io.Closer, error)
	quickPhase, hashPhase string
	cached                func(f *file, quick [sha256.Size]byte) ([sha256.Size]byte, bool)
	remember              func(f *file, quick, sum [sha256.Size]byte)
	told                  string
}

// identical sorts out the files whose content is the same: those of the same
// size, then of those the ones whose first and last 64 KB are the same, then of
// those the ones whose hash is the same. Only sets with a file of the folder
// being cleaned are returned, in the order their first file was found.
func (ru *rules) identical(files []*file, cmp comparison, r *reporter) [][]*file {
	key := func(f *file) [2]int64 {
		if cmp.extra == nil {
			return [2]int64{cmp.size(f), 0}
		}
		return [2]int64{cmp.size(f), cmp.extra(f)}
	}
	sameSize := shared(groupBy(files, func(_ int, f *file) ([2]int64, bool) { return key(f), true }))
	r.printf("  %d %s\n", len(sameSize), cmp.told)

	quick := make([][sha256.Size]byte, len(sameSize))
	ok := make([]bool, len(sameSize))
	var started atomic.Int64
	parallel(sameSize, ru.jobs, r, func(i int, f *file, buf []byte) {
		r.progress(cmp.quickPhase, started.Add(1)-1, int64(len(sameSize)), f.rel)
		src, closer, err := cmp.open(f)
		if err == nil {
			quick[i], err = quickSum(src, cmp.size(f), buf)
			closer.Close()
		}
		if err != nil {
			r.warnf("  ! cannot read %s: %v\n", f.path, err)
			return
		}
		ok[i] = true
	})
	if r.stopped() {
		return nil
	}
	type sumKey struct {
		key [2]int64
		sum [sha256.Size]byte
	}
	quickOf := map[*file][sha256.Size]byte{}
	for i, f := range sameSize {
		quickOf[f] = quick[i]
	}
	candidates := shared(groupBy(sameSize, func(i int, f *file) (sumKey, bool) {
		return sumKey{key(f), quick[i]}, ok[i]
	}))

	var total int64
	for _, f := range candidates {
		total += cmp.size(f)
	}
	sums := make([][sha256.Size]byte, len(candidates))
	hashed := make([]bool, len(candidates))
	var read, done atomic.Int64
	parallel(candidates, ru.jobs, r, func(i int, f *file, buf []byte) {
		defer func() {
			if n := done.Add(1); n%500 == 0 {
				r.printf("  hashed %d/%d\n", n, len(candidates))
			}
		}()
		if sum, hit := cmp.cached(f, quickOf[f]); hit {
			sums[i], hashed[i] = sum, true
			r.progress(cmp.hashPhase, read.Add(cmp.size(f)), total, f.rel)
			return
		}
		src, closer, err := cmp.open(f)
		if err == nil {
			sums[i], err = fullSum(io.NewSectionReader(src, 0, cmp.size(f)), buf, r, func(n int) {
				r.progress(cmp.hashPhase, read.Add(int64(n)), total, f.rel)
			})
			closer.Close()
		}
		if errors.Is(err, errStopped) {
			return
		}
		if err != nil {
			r.warnf("  ! cannot read %s: %v\n", f.path, err)
			return
		}
		hashed[i] = true
		cmp.remember(f, quickOf[f], sums[i])
	})
	if r.stopped() {
		return nil
	}
	if len(candidates) > 0 {
		r.progress(cmp.hashPhase, total, total, "")
	}
	var sets [][]*file
	for _, g := range groupBy(candidates, func(i int, f *file) (sumKey, bool) { return sumKey{key(f), sums[i]}, hashed[i] }) {
		if len(g) > 1 && slices.ContainsFunc(g, movable) {
			sets = append(sets, g)
		}
	}
	return sets
}

// movable reports whether a file may move at all: not when it is in the
// reference folder, or in a folder to keep.
func movable(f *file) bool { return !f.ref && !f.inKeep }

// shared is the files of the groups with more than one file, at least one of
// them movable.
func shared(groups [][]*file) []*file {
	var files []*file
	for _, g := range groups {
		if len(g) > 1 && slices.ContainsFunc(g, movable) {
			files = append(files, g...)
		}
	}
	return files
}

// parallel calls work for every file, on up to jobs goroutines at once, each
// with a read buffer of its own, until the run is stopped. A file that breaks
// work, as a damaged one might, is skipped with a warning, rather than end the
// program.
func parallel(files []*file, jobs int, r *reporter, work func(i int, f *file, buf []byte)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(jobs, len(files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, readChunk)
			for i := int(next.Add(1) - 1); i < len(files) && !r.stopped(); i = int(next.Add(1) - 1) {
				func() {
					defer func() {
						if v := recover(); v != nil {
							r.warnf("  ! skipped %s after an error: %v\n", files[i].path, v)
						}
					}()
					work(i, files[i], buf)
				}()
			}
		}()
	}
	wg.Wait()
}

// group makes a group of copies: the best one, by the rules, stays, and the
// movable others go. Hard links to one another are a single file
// on disk already, so they count once. It is nil when nothing would go.
func (ru *rules) group(files []*file, kind string) *group {
	ru.sortForKeeping(files)
	var unique []*file
	for _, f := range files {
		if !slices.ContainsFunc(unique, func(u *file) bool { return os.SameFile(u.info, f.info) }) {
			unique = append(unique, f)
		}
	}
	g := &group{keeper: unique[0], kind: kind}
	for _, f := range unique[1:] {
		if movable(f) {
			g.losers = append(g.losers, f)
		}
	}
	if len(g.losers) == 0 {
		return nil
	}
	return g
}

// sameImages finds JPEGs that are the same picture with other tags, such as
// dates, ratings or captions a photo app wrote into one of them. It compares
// the pictures without their tags, among the photos no group moves yet, and
// joins each set it finds with the groups of their byte-identical copies.
func (ru *rules) sameImages(groups []group, media []*file, c *cache, r *reporter) []group {
	keeperOf := map[*file]int{}
	losing := map[*file]bool{}
	for i, g := range groups {
		keeperOf[g.keeper] = i
		for _, f := range g.losers {
			losing[f] = true
		}
	}
	var jpegs []*file
	for _, f := range media {
		if isJPEG(f) && !losing[f] {
			jpegs = append(jpegs, f)
		}
	}

	infos := make([]jpegInfo, len(jpegs))
	read := make([]bool, len(jpegs))
	var started atomic.Int64
	parallel(jpegs, ru.jobs, r, func(i int, f *file, _ []byte) {
		r.progress("image", started.Add(1)-1, int64(len(jpegs)), f.rel)
		fh, err := os.Open(f.path)
		if err != nil {
			r.warnf("  ! cannot read %s: %v\n", f.path, err)
			return
		}
		defer fh.Close()
		if infos[i], err = readJPEG(fh, f.size); err == nil {
			read[i] = true
			f.tags = infos[i].tags
		}
	})
	if r.stopped() {
		return nil
	}
	pictures := map[*file]jpegInfo{}
	var readable []*file
	for i, f := range jpegs {
		if read[i] {
			pictures[f] = infos[i]
			readable = append(readable, f)
		}
	}
	for _, g := range groups {
		for _, twin := range g.losers {
			twin.tags = g.keeper.tags // byte for byte the same as their keeper
		}
	}

	// Copies turned another way don't look the same, so their orientation must match.
	sets := ru.identical(readable, comparison{
		size:  func(f *file) int64 { return pictures[f].pictureSize },
		extra: func(f *file) int64 { return int64(pictures[f].orientation) },
		open: func(f *file) (io.ReaderAt, io.Closer, error) {
			fh, err := os.Open(f.path)
			return pictureReader{fh, pictures[f].picture}, fh, err
		},
		quickPhase: "imagequick",
		hashPhase:  "imagehash",
		cached:     c.picture,
		remember:   c.putPicture,
		told:       "JPEGs share a picture size with another; comparing those without their tags",
	}, r)
	if r.stopped() {
		return nil
	}

	absorbed := map[int]bool{}
	var joined []group
	for _, set := range sets {
		var all []*file
		var into []int
		for _, f := range set {
			if i, ok := keeperOf[f]; ok {
				into = append(into, i)
				all = append(all, groups[i].losers...)
			}
			all = append(all, f)
		}
		if g := ru.group(all, "tags"); g != nil {
			joined = append(joined, *g)
			for _, i := range into {
				absorbed[i] = true
			}
		}
	}
	var out []group
	for i, g := range groups {
		if !absorbed[i] {
			out = append(out, g)
		}
	}
	return append(out, joined...)
}

// groupBy groups files by key, keeping the groups in the order their first file
// was seen. Files for which key returns false are left out.
func groupBy[K comparable](files []*file, key func(i int, f *file) (K, bool)) [][]*file {
	index := map[K]int{}
	var groups [][]*file
	for i, f := range files {
		k, ok := key(i, f)
		if !ok {
			continue
		}
		j, seen := index[k]
		if !seen {
			j = len(groups)
			index[k] = j
			groups = append(groups, nil)
		}
		groups[j] = append(groups[j], f)
	}
	return groups
}

// quickFingerprint hashes the first and last 64 KB. Cheap way to rule files out.
func quickFingerprint(f *file, buf []byte) (sum [sha256.Size]byte, err error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return sum, err
	}
	defer fh.Close()
	return quickSum(fh, f.size, buf)
}

// quickSum is quickFingerprint of size bytes read from r.
func quickSum(r io.ReaderAt, size int64, buf []byte) (sum [sha256.Size]byte, err error) {
	h := sha256.New()
	if _, err = io.CopyBuffer(h, io.NewSectionReader(r, 0, min(size, 65536)), buf); err == nil && size > 131072 {
		_, err = io.CopyBuffer(h, io.NewSectionReader(r, size-65536, 65536), buf)
	}
	h.Sum(sum[:0])
	return sum, err
}

var errStopped = errors.New("stopped")

// fullSum hashes everything src gives, telling read about every chunk, and
// gives up with errStopped when the run is stopped.
func fullSum(src io.Reader, buf []byte, r *reporter, read func(n int)) (sum [sha256.Size]byte, err error) {
	h := sha256.New()
	for {
		if r.stopped() {
			return sum, errStopped
		}
		n, err := src.Read(buf)
		h.Write(buf[:n])
		read(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, err
		}
	}
	h.Sum(sum[:0])
	return sum, nil
}

// rank decides which of several identical copies stays: the one that sorts first.
type rank struct {
	ref         bool   // the reference folder's copy is kept already
	preferred   bool   // a folder chosen to keep beats any other
	inDump      bool   // curated folder beats a phone dump / backup
	looksCopied bool   // "IMG_1234" beats "IMG_1234 (1)"
	tags        int64  // more tags beat fewer, for JPEGs compared without them
	depth       int    // negative: a named album is usually deeper than the root
	mtime       int64  // older file first: likely the original (negative to keep the newest)
	path        string // stable, so runs are repeatable
}

// rank only looks at folders inside the scanned folder, so a photo library
// that itself sits in "Downloads" or "Backup" is judged the same.
func (ru *rules) rank(f *file) rank {
	parts := strings.Split(f.rel, "/")
	r := rank{ref: f.ref, preferred: f.inKeep, tags: f.tags, depth: -len(parts),
		mtime: f.mtime, path: strings.ToLower(f.rel)}
	if ru.KeepNewest {
		r.mtime = -f.mtime
	}
	for _, part := range parts[:len(parts)-1] {
		r.inDump = r.inDump || ru.backups[strings.ToLower(part)]
	}
	name := strings.ToLower(stem(parts[len(parts)-1]))
	for _, marker := range copyMarkers {
		r.looksCopied = r.looksCopied || strings.Contains(name, marker)
	}
	return r
}

func (r rank) less(o rank) bool {
	switch {
	case r.ref != o.ref:
		return r.ref
	case r.preferred != o.preferred:
		return r.preferred
	case r.inDump != o.inDump:
		return !r.inDump
	case r.looksCopied != o.looksCopied:
		return !r.looksCopied
	case r.tags != o.tags:
		return r.tags > o.tags
	case r.depth != o.depth:
		return r.depth < o.depth
	case r.mtime != o.mtime:
		return r.mtime < o.mtime
	}
	return r.path < o.path
}

// sortForKeeping puts the copy to keep first.
func sortForKeeping(files []*file) { defaultRules.sortForKeeping(files) }

func (ru *rules) sortForKeeping(files []*file) {
	ranks := make(map[*file]rank, len(files))
	for _, f := range files {
		ranks[f] = ru.rank(f)
	}
	sort.SliceStable(files, func(i, j int) bool { return ranks[files[i]].less(ranks[files[j]]) })
}

// keptBecause says why keeper stays rather than loser, as a code that the
// pages put into words. whyKept says it in English, for the reports.
func keptBecause(keeper, loser *file) string { return defaultRules.keptBecause(keeper, loser) }

func (ru *rules) keptBecause(keeper, loser *file) string {
	k, l := ru.rank(keeper), ru.rank(loser)
	switch {
	case k.ref != l.ref:
		return "reference"
	case k.preferred != l.preferred:
		return "preferred"
	case k.inDump != l.inDump:
		return "dump"
	case k.looksCopied != l.looksCopied:
		return "copy-name"
	case k.tags != l.tags:
		return "tags"
	case k.depth != l.depth:
		return "deeper"
	case k.mtime != l.mtime && ru.KeepNewest:
		return "newer"
	case k.mtime != l.mtime:
		return "older"
	}
	return "name"
}

// why is keptBecause for a group: by hand, by size for look-alikes, or by the rules.
func (ru *rules) why(g group) string {
	k, l := g.keeper, g.losers[0]
	switch {
	case g.chosen:
		return "chosen"
	case g.kind == "alike" && k.w*k.h != l.w*l.h:
		return "pixels"
	case g.kind == "alike" && k.size != l.size:
		return "bytes"
	}
	return ru.keptBecause(k, l)
}

var reasons = map[string]string{
	"reference": "this copy is in the reference folder",
	"preferred": "this copy is in a folder chosen to keep",
	"dump":      "other copy sits in a backup/DCIM folder",
	"copy-name": "other copy is named like a copy",
	"tags":      "this copy has the most tags",
	"deeper":    "this copy is in a more specific folder",
	"older":     "this copy is older",
	"newer":     "this copy is newer",
	"name":      "identical in every way, first by name",
	"chosen":    "chosen by hand",
	"pixels":    "this copy has the most pixels",
	"bytes":     "this copy is the largest file",
}

func whyKept(keeper, loser *file) string { return reasons[keptBecause(keeper, loser)] }
