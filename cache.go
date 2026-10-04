package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const cacheName = "fingerprints.jsonl"

// cache keeps what runs learn about files in _Quarantine/fingerprints.jsonl,
// so that the next run on the folder only reads the files that are new or
// changed. A file's hashes count while its size, its modification time and
// the quick fingerprint of its first and last 64 KB, which every run takes
// anew, stay the same. Its look goes by size and time alone, as looks never
// move anything by themselves. A nil cache remembers nothing.
type cache struct {
	old  map[string]cacheEntry // as read at the start, and never changed
	mu   sync.Mutex
	seen map[string]cacheEntry // learnt in this run
}

type cacheEntry struct {
	Key    string `json:"k"`
	Size   int64  `json:"s"`
	Mtime  int64  `json:"t"`
	Quick  string `json:"q,omitempty"`  // of the first and last 64 KB
	Full   string `json:"f,omitempty"`  // of all of it
	PQuick string `json:"pq,omitempty"` // likewise, of a JPEG's picture without its tags
	Pic    string `json:"p,omitempty"`
	Look   string `json:"l,omitempty"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
}

func loadCache(root string) *cache {
	c := &cache{old: map[string]cacheEntry{}, seen: map[string]cacheEntry{}}
	fh, err := os.Open(filepath.Join(root, quarantine, cacheName))
	if err != nil {
		return c
	}
	defer fh.Close()
	lines := bufio.NewScanner(fh)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		var e cacheEntry
		if json.Unmarshal(lines.Bytes(), &e) == nil && e.Key != "" {
			c.old[e.Key] = e
		}
	}
	return c
}

// cacheKey is a file's path inside the folder, so that the cache still works
// when the folder moves, or a full path for files of the reference folder.
func cacheKey(f *file) string {
	if f.ref {
		return "ref:" + filepath.ToSlash(f.path)
	}
	return f.rel
}

// known is what the cache knew of a file at the start, if it is still the same
// size and age.
func (c *cache) known(f *file) (cacheEntry, bool) {
	e, ok := c.old[cacheKey(f)]
	return e, ok && e.Size == f.size && e.Mtime == f.mtime
}

// learn records something about a file for the next run.
func (c *cache) learn(f *file, change func(e *cacheEntry)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(f)
	e, ok := c.seen[key]
	if !ok {
		if e, ok = c.known(f); !ok {
			e = cacheEntry{Key: key, Size: f.size, Mtime: f.mtime}
		}
	}
	change(&e)
	c.seen[key] = e
}

func (c *cache) full(f *file, quick [sha256.Size]byte) ([sha256.Size]byte, bool) {
	if c == nil {
		return [sha256.Size]byte{}, false
	}
	e, ok := c.known(f)
	if !ok || e.Quick != hex.EncodeToString(quick[:]) {
		return [sha256.Size]byte{}, false
	}
	return decodeSum(e.Full)
}

func (c *cache) putFull(f *file, quick, sum [sha256.Size]byte) {
	c.learn(f, func(e *cacheEntry) { e.Quick, e.Full = hex.EncodeToString(quick[:]), hex.EncodeToString(sum[:]) })
}

func (c *cache) picture(f *file, quick [sha256.Size]byte) ([sha256.Size]byte, bool) {
	if c == nil {
		return [sha256.Size]byte{}, false
	}
	e, ok := c.known(f)
	if !ok || e.PQuick != hex.EncodeToString(quick[:]) {
		return [sha256.Size]byte{}, false
	}
	return decodeSum(e.Pic)
}

func (c *cache) putPicture(f *file, quick, sum [sha256.Size]byte) {
	c.learn(f, func(e *cacheEntry) { e.PQuick, e.Pic = hex.EncodeToString(quick[:]), hex.EncodeToString(sum[:]) })
}

func (c *cache) look(f *file) (look uint64, w, h int, ok bool) {
	if c == nil {
		return 0, 0, 0, false
	}
	e, ok := c.known(f)
	if !ok || e.Look == "" {
		return 0, 0, 0, false
	}
	look, err := strconv.ParseUint(e.Look, 16, 64)
	return look, e.W, e.H, err == nil
}

func (c *cache) putLook(f *file, look uint64, w, h int) {
	c.learn(f, func(e *cacheEntry) { e.Look, e.W, e.H = strconv.FormatUint(look, 16), w, h })
}

func decodeSum(s string) (sum [sha256.Size]byte, ok bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return sum, false
	}
	copy(sum[:], b)
	return sum, true
}

// save writes what the cache knows of the files of this run, and so forgets
// files that are gone. A nil cache writes nothing.
func (c *cache) save(root string, files []*file) error {
	if c == nil {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, f := range files {
		e, ok := c.seen[cacheKey(f)]
		if !ok {
			e, ok = c.known(f)
		}
		if ok {
			enc.Encode(e)
		}
	}
	dir := filepath.Join(root, quarantine)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, cacheName+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, cacheName))
}
