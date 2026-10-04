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
)

// A file found by scan.
type file struct {
	path  string // full path
	rel   string // path inside the scanned folder, always with / separators
	size  int64
	mtime int64       // modification time in nanoseconds
	info  fs.FileInfo // lets os.SameFile spot hard links
}

// A set of byte-identical files: the copy that stays and the ones that go.
type group struct {
	keeper *file
	losers []*file
}

type scanResult struct {
	media, other []*file
	skipped      int // links and entries that could not be read
	system       int // system files and folders left alone
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
func scan(root string, r *reporter) scanResult {
	var res scanResult
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		if r.stopped() {
			return
		}
		r.progress("scan", int64(len(res.media)+len(res.other)), 0, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			r.warnf("  ! cannot read folder %s: %v\n", dir, err)
			res.skipped++
		}
		var subdirs []string
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
				if isSystemDir(name) {
					res.system++
				} else if !strings.EqualFold(name, quarantine) { // ours, at any depth
					subdirs = append(subdirs, name)
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
				mtime: info.ModTime().UnixNano(), info: info}
			if isMedia(name) && f.size > 0 {
				res.media = append(res.media, f)
			} else {
				res.other = append(res.other, f)
			}
		}
		for _, name := range subdirs {
			walk(filepath.Join(dir, name), joinRel(rel, name))
		}
	}
	walk(root, "")
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
	var sameSize []*file
	for _, g := range groupBy(media, func(f *file) (int64, bool) { return f.size, true }) {
		if len(g) > 1 {
			sameSize = append(sameSize, g...)
		}
	}
	r.printf("  %d files share a size with another file; fingerprinting those\n", len(sameSize))

	type quickKey struct {
		size int64
		sum  [sha256.Size]byte
	}
	buf := make([]byte, readChunk)
	var candidates []*file
	var candidateBytes int64
	fingerprinted := 0
	for _, g := range groupBy(sameSize, func(f *file) (quickKey, bool) {
		if r.stopped() {
			return quickKey{}, false
		}
		r.progress("fingerprint", int64(fingerprinted), int64(len(sameSize)), f.rel)
		fingerprinted++
		sum, err := quickFingerprint(f, buf)
		if err != nil {
			r.warnf("  ! cannot read %s: %v\n", f.path, err)
			return quickKey{}, false
		}
		return quickKey{f.size, sum}, true
	}) {
		if len(g) > 1 {
			candidates = append(candidates, g...)
			for _, f := range g {
				candidateBytes += f.size
			}
		}
	}

	done := 0
	var hashedBytes int64
	byHash := groupBy(candidates, func(f *file) ([sha256.Size]byte, bool) {
		sum, err := fullHash(f, buf, r, func(n int) {
			hashedBytes += int64(n)
			r.progress("hash", hashedBytes, candidateBytes, f.rel)
		})
		if errors.Is(err, errStopped) {
			return sum, false
		}
		if err != nil {
			r.warnf("  ! cannot read %s: %v\n", f.path, err)
		}
		done++
		if done%500 == 0 {
			r.printf("  hashed %d/%d\n", done, len(candidates))
		}
		return sum, err == nil
	})
	if r.stopped() {
		return nil
	}

	var groups []group
	for _, g := range byHash {
		if len(g) < 2 {
			continue
		}
		sortForKeeping(g)
		// Hard links to one another are already a single file on disk: leave alone.
		var unique []*file
		for _, f := range g {
			if !slices.ContainsFunc(unique, func(u *file) bool { return os.SameFile(u.info, f.info) }) {
				unique = append(unique, f)
			}
		}
		if len(unique) > 1 {
			groups = append(groups, group{unique[0], unique[1:]})
		}
	}
	return groups
}

// groupBy groups files by key, keeping the groups in the order their first file
// was seen. Files for which key returns false are left out.
func groupBy[K comparable](files []*file, key func(*file) (K, bool)) [][]*file {
	index := map[K]int{}
	var groups [][]*file
	for _, f := range files {
		k, ok := key(f)
		if !ok {
			continue
		}
		i, seen := index[k]
		if !seen {
			i = len(groups)
			index[k] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], f)
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
	h := sha256.New()
	if _, err = io.CopyBuffer(h, io.LimitReader(fh, 65536), buf); err == nil && f.size > 131072 {
		if _, err = fh.Seek(-65536, io.SeekEnd); err == nil {
			_, err = io.CopyBuffer(h, io.LimitReader(fh, 65536), buf)
		}
	}
	h.Sum(sum[:0])
	return sum, err
}

var errStopped = errors.New("stopped")

// fullHash hashes the whole file, telling read about every chunk, and gives up
// with errStopped when the run is stopped.
func fullHash(f *file, buf []byte, r *reporter, read func(n int)) (sum [sha256.Size]byte, err error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return sum, err
	}
	defer fh.Close()
	h := sha256.New()
	for {
		if r.stopped() {
			return sum, errStopped
		}
		n, err := fh.Read(buf)
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
	inDump      bool   // curated folder beats a phone dump / backup
	looksCopied bool   // "IMG_1234" beats "IMG_1234 (1)"
	depth       int    // negative: a named album is usually deeper than the root
	mtime       int64  // older file first: likely the original
	path        string // stable, so runs are repeatable
}

// keeperRank only looks at folders inside the scanned folder, so a photo
// library that itself sits in "Downloads" or "Backup" is judged the same.
func keeperRank(f *file) rank {
	parts := strings.Split(f.rel, "/")
	r := rank{depth: -len(parts), mtime: f.mtime, path: strings.ToLower(f.rel)}
	for _, part := range parts[:len(parts)-1] {
		r.inDump = r.inDump || disposableParts[strings.ToLower(part)]
	}
	name := strings.ToLower(stem(parts[len(parts)-1]))
	for _, marker := range copyMarkers {
		r.looksCopied = r.looksCopied || strings.Contains(name, marker)
	}
	return r
}

func (r rank) less(o rank) bool {
	switch {
	case r.inDump != o.inDump:
		return !r.inDump
	case r.looksCopied != o.looksCopied:
		return !r.looksCopied
	case r.depth != o.depth:
		return r.depth < o.depth
	case r.mtime != o.mtime:
		return r.mtime < o.mtime
	}
	return r.path < o.path
}

// sortForKeeping puts the copy to keep first.
func sortForKeeping(files []*file) {
	ranks := make(map[*file]rank, len(files))
	for _, f := range files {
		ranks[f] = keeperRank(f)
	}
	sort.SliceStable(files, func(i, j int) bool { return ranks[files[i]].less(ranks[files[j]]) })
}

// keptBecause says why keeper stays rather than loser, as a code that the
// simple page puts into words. whyKept says it in English, for the reports.
func keptBecause(keeper, loser *file) string {
	k, l := keeperRank(keeper), keeperRank(loser)
	switch {
	case k.inDump != l.inDump:
		return "dump"
	case k.looksCopied != l.looksCopied:
		return "copy-name"
	case k.depth != l.depth:
		return "deeper"
	case k.mtime != l.mtime:
		return "older"
	}
	return "name"
}

var reasons = map[string]string{
	"dump":      "other copy sits in a backup/DCIM folder",
	"copy-name": "other copy is named like a copy",
	"deeper":    "this copy is in a more specific folder",
	"older":     "this copy is older",
	"name":      "identical in every way, first by name",
}

func whyKept(keeper, loser *file) string { return reasons[keptBecause(keeper, loser)] }
