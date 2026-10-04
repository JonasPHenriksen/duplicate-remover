package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed gui.html
var guiPage []byte

// How long the program stays after its page was closed, in case it is only
// being reloaded, and after the page last asked for news at all.
var (
	closeGrace = 5 * time.Second
	idleLimit  = 15 * time.Minute
)

// The most duplicate groups, and file names per type, a preview sends to the
// page. The CSV reports always have everything.
const (
	maxGroupsShown = 1000
	maxPathsShown  = 500
)

// gui runs the graphical interface: a page in the web browser, served by this
// program to this computer only. It returns once the page has been closed.
func gui(prog, folder string, out io.Writer) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(out, "Cannot start the graphical interface:", err)
		return 1
	}
	s := newSession(prog, folder)
	srv := &http.Server{Handler: s.handler(ln.Addr().String()), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)

	say := windowWords(s.lang)
	url := fmt.Sprintf("http://%s/#%s", ln.Addr(), s.token)
	fmt.Fprintf(out, say.open, url)
	if os.Getenv("PHOTO_CLEANUP_NO_BROWSER") == "" {
		if err := openBrowser(url); err != nil {
			fmt.Fprintln(out, say.noBrowser, err)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for !s.finished(time.Now()) {
		select {
		case <-signals:
			if !s.quit() { // the second Ctrl+C quits at once
				os.Exit(130)
			}
		case <-tick.C:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	fmt.Fprintln(out, say.closed)
	return 0
}

// session is what the page shows, shared by the page's requests and the job
// running in the background.
type session struct {
	token, prog, folder string // folder: given when the program started, if any
	lang, pictures      string // the simple edition's: see systemLanguage and picturesFolder

	stopping atomic.Bool // the running job should stop between two files

	mu       sync.Mutex
	job      int // counts jobs, so the page can tell a new one from the last
	kind     string
	busy     bool
	phase    string
	done     int64
	total    int64
	item     string
	log      []logLine
	result   any
	plan     *plan  // what the last scan found, for the move
	root     string // the folder of the last job, for reveal
	lastSeen time.Time
	byeAt    time.Time
	quitting bool
}

type logLine struct {
	Text string `json:"text"`
	Warn bool   `json:"warn,omitempty"`
}

func newSession(prog, folder string) *session {
	b := make([]byte, 16)
	rand.Read(b)
	s := &session{token: hex.EncodeToString(b), prog: prog, folder: folder, lastSeen: time.Now()}
	if simpleEdition() {
		s.lang, s.pictures = systemLanguage(), picturesFolder()
	}
	return s
}

// handler serves the page and its API. Only requests addressed to host get
// through, which stops other websites from reaching it through DNS tricks, and
// the API also needs the token the page was given in its address.
func (s *session) handler(host string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/check", s.check)
	mux.HandleFunc("POST /api/pick", s.pick)
	mux.HandleFunc("POST /api/scan", s.startScan)
	mux.HandleFunc("POST /api/move", s.startMove)
	mux.HandleFunc("POST /api/undo", s.startUndo)
	mux.HandleFunc("POST /api/empty", s.startEmpty)
	mux.HandleFunc("POST /api/stop", s.stop)
	mux.HandleFunc("POST /api/reveal", s.reveal)
	mux.HandleFunc("GET /api/picture", s.picture)
	mux.HandleFunc("POST /api/quit", s.quitPage)
	mux.HandleFunc("POST /api/bye", s.bye)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != host {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(req.URL.Path, "/api/") {
			token := req.Header.Get("X-Token")
			if token == "" {
				token = req.URL.Query().Get("token") // sendBeacon and <img> cannot set headers
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
				http.Error(w, "wrong token", http.StatusForbidden)
				return
			}
			s.mu.Lock()
			s.lastSeen = time.Now()
			s.mu.Unlock()
		}
		mux.ServeHTTP(w, req)
	})
}

func (s *session) page(w http.ResponseWriter, req *http.Request) {
	page := guiPage
	if simpleEdition() {
		page = simplePage
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
		"img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(page)
}

type stateReply struct {
	Job    int       `json:"job"`
	Busy   bool      `json:"busy"`
	Kind   string    `json:"kind"`
	Phase  string    `json:"phase"`
	Done   int64     `json:"done"`
	Total  int64     `json:"total"`
	Item   string    `json:"item"`
	Log    []logLine `json:"log"`  // the lines from since on
	Next   int       `json:"next"` // since, next time
	Result any       `json:"result"`
	Folder string    `json:"folder"`

	Lang     string `json:"lang,omitempty"`     // the simple edition's, for its page
	Pictures string `json:"pictures,omitempty"` // likewise
}

// state is what the page asks for a few times a second.
func (s *session) state(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	since, _ := strconv.Atoi(q.Get("since"))
	s.mu.Lock()
	if q.Get("job") != strconv.Itoa(s.job) || since < 0 || since > len(s.log) {
		since = 0
	}
	reply := stateReply{Job: s.job, Busy: s.busy, Kind: s.kind, Phase: s.phase, Done: s.done, Total: s.total,
		Item: s.item, Log: s.log[since:], Next: len(s.log), Result: s.result, Folder: s.folder,
		Lang: s.lang, Pictures: s.pictures}
	s.mu.Unlock()
	writeJSON(w, reply)
}

// check tells the page whether a path is a folder, and how many moves an
// earlier clean-up recorded there for undo.
func (s *session) check(w http.ResponseWriter, req *http.Request) {
	var in struct{ Path string }
	if !readJSON(w, req, &in) {
		return
	}
	root, ok := chosenFolder(in.Path)
	reply := struct {
		Path    string `json:"path"`
		OK      bool   `json:"ok"`
		Journal int    `json:"journal"`
	}{Path: root, OK: ok}
	if ok {
		entries, _ := readJournal(filepath.Join(root, quarantine, journalName))
		reply.Journal = len(entries)
	}
	writeJSON(w, reply)
}

// chosenFolder is folder for paths from the page, which may be pasted with
// quotes around them and must not be empty.
func chosenFolder(path string) (string, bool) {
	path = droppedPath(path)
	if path == "" {
		return "", false
	}
	return folder(path)
}

// pick shows the folder picker, titled in the page's language and for its
// purpose: the folder to clean up, or a folder for the options. It opens in
// start, if that is a folder, and in the simple edition at the folder the
// page suggests, where it can.
func (s *session) pick(w http.ResponseWriter, req *http.Request) {
	var in struct{ Lang, Purpose, Start string }
	if !readJSON(w, req, &in) {
		return
	}
	start := s.pictures
	if dir, ok := chosenFolder(in.Start); ok {
		start = dir
	}
	title := pickerTitle(in.Lang)
	switch in.Purpose {
	case "keep":
		title = "Choose a folder whose copies always stay"
	case "skip":
		title = "Choose a folder to leave out"
	case "reference":
		title = "Choose the reference folder"
	}
	path, err := pickFolder(title, start)
	writeJSON(w, map[string]any{"path": path, "unavailable": err != nil})
}

func (s *session) startScan(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Path    string
		Options options
	}
	if !readJSON(w, req, &in) {
		return
	}
	root, ok := chosenFolder(in.Path)
	if !ok {
		http.Error(w, "Not a folder: "+root, http.StatusBadRequest)
		return
	}
	ru, err := newRules(root, in.Options)
	if err != nil {
		http.Error(w, "Cannot look through the folder like that: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.start(w, "scan", root, nil, func(r *reporter) any {
		p, ok := prepareWith(root, ru, r)
		if !ok {
			return outcome{Kind: "stopped", Folder: root}
		}
		if _, err := p.writeReports(s.prog); err != nil {
			r.warnf("  ! could not write the reports: %v\n", err)
		}
		s.mu.Lock()
		s.plan = p
		s.mu.Unlock()
		return preview(p)
	})
}

// startMove carries out the last scan's plan, as the page changed it.
func (s *session) startMove(w http.ResponseWriter, req *http.Request) {
	var in choices
	if !readJSON(w, req, &in) {
		return
	}
	s.mu.Lock()
	p := s.plan
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "Scan the folder first.", http.StatusConflict)
		return
	}
	if err := p.check(in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	removeEmpty := p.ru.RemoveEmpty
	if in.RemoveEmpty != nil {
		removeEmpty = *in.RemoveEmpty
	}
	s.start(w, "move", p.root, p, func(r *reporter) any {
		p.choose(in)
		code := p.apply(r)
		if code == 0 {
			if removeEmpty {
				p.removed = p.removeLeftEmpty()
			}
			if _, err := p.writeReports(s.prog); err != nil {
				r.warnf("  ! could not write the reports: %v\n", err)
			}
		}
		return outcome{Kind: "moved", Folder: p.root, Quarantine: filepath.Join(p.root, quarantine),
			Moved: p.moved, Failed: p.failed, Bytes: p.movedBytes, Stopped: p.stopped, GaveUp: code == 1,
			Folders: p.removed}
	})
}

// startEmpty deletes, for good, what clean-ups moved into the folder's
// _Quarantine. The page asks first, and sends the number of files it asked
// about: if the journal has another, nothing is deleted.
func (s *session) startEmpty(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Path   string
		Expect int
	}
	if !readJSON(w, req, &in) {
		return
	}
	root, ok := chosenFolder(in.Path)
	if !ok {
		http.Error(w, "Not a folder: "+root, http.StatusBadRequest)
		return
	}
	s.start(w, "empty", root, nil, func(r *reporter) any {
		res, err := emptyQuarantine(root, in.Expect, r)
		if errors.Is(err, fs.ErrNotExist) {
			return outcome{Kind: "error", Folder: root, Error: "There is nothing in _Quarantine to delete."}
		}
		if err != nil {
			return outcome{Kind: "error", Folder: root, Error: err.Error()}
		}
		return outcome{Kind: "emptied", Folder: root, Deleted: res.deleted, Bytes: res.bytes,
			Missing: res.missing, Failed: res.failed, Kept: res.kept, Stopped: res.stopped}
	})
}

func (s *session) startUndo(w http.ResponseWriter, req *http.Request) {
	var in struct{ Path string }
	if !readJSON(w, req, &in) {
		return
	}
	root, ok := chosenFolder(in.Path)
	if !ok {
		http.Error(w, "Not a folder: "+root, http.StatusBadRequest)
		return
	}
	s.start(w, "undo", root, nil, func(r *reporter) any {
		res, err := undo(root, r)
		if errors.Is(err, fs.ErrNotExist) {
			return outcome{Kind: "error", Folder: root, Error: "There is nothing to undo in this folder."}
		}
		if err != nil {
			return outcome{Kind: "error", Folder: root, Error: err.Error()}
		}
		return outcome{Kind: "undone", Folder: root, Restored: res.restored, Missing: res.missing,
			Blocked: res.blocked, Stopped: res.stopped}
	})
}

// start runs a job in the background, one at a time. Any new job replaces the
// last scan's plan: a move uses it up, and a scan or undo makes it stale. A
// move gives the plan it carries out as plan, and only starts if that is still
// the last scan's, so that two moves never carry out one plan.
func (s *session) start(w http.ResponseWriter, kind, root string, plan *plan, job func(r *reporter) any) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		http.Error(w, "photo_cleanup is busy.", http.StatusConflict)
		return
	}
	if plan != nil && s.plan != plan {
		s.mu.Unlock()
		http.Error(w, "Scan the folder first.", http.StatusConflict)
		return
	}
	s.job++
	s.busy, s.kind, s.root, s.plan = true, kind, root, nil
	s.phase, s.done, s.total, s.item = kind, 0, 0, ""
	s.log, s.result = nil, nil
	s.stopping.Store(false)
	n := s.job
	s.mu.Unlock()

	r := &reporter{out: logWriter{s, false}, errOut: logWriter{s, true}, step: s.progress, stop: s.stopping.Load}
	go func() {
		var result any
		defer func() {
			if v := recover(); v != nil {
				result = outcome{Kind: "error", Folder: root, Error: fmt.Sprint(v)}
			}
			s.mu.Lock()
			s.busy, s.result = false, result
			s.mu.Unlock()
		}()
		result = job(r)
	}()
	writeJSON(w, map[string]int{"job": n})
}

// progress keeps the latest news of the running job. Within a phase, done only
// grows: files read at once can report a little out of order.
func (s *session) progress(phase string, done, total int64, item string) {
	s.mu.Lock()
	if phase != s.phase || done > s.done {
		s.done = done
	}
	s.phase, s.total, s.item = phase, total, filepath.FromSlash(item)
	s.mu.Unlock()
}

// logWriter adds what a job prints to the page's log, line by line.
type logWriter struct {
	s    *session
	warn bool
}

func (lw logWriter) Write(b []byte) (int, error) {
	lw.s.mu.Lock()
	defer lw.s.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		lw.s.log = append(lw.s.log, logLine{Text: line, Warn: lw.warn})
	}
	return len(b), nil
}

func (s *session) stop(w http.ResponseWriter, req *http.Request) {
	s.stopping.Store(true)
	writeJSON(w, map[string]bool{"ok": true})
}

// reveal shows the last job's folder, or its quarantine, in Finder, Explorer or
// the file manager. The page can only choose between those two, never a path.
func (s *session) reveal(w http.ResponseWriter, req *http.Request) {
	var in struct{ What string }
	if !readJSON(w, req, &in) {
		return
	}
	s.mu.Lock()
	dir := s.root
	s.mu.Unlock()
	if dir == "" {
		http.Error(w, "Choose a folder first.", http.StatusConflict)
		return
	}
	if in.What == "quarantine" {
		dir = filepath.Join(dir, quarantine)
	}
	if err := revealFolder(dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *session) quitPage(w http.ResponseWriter, req *http.Request) {
	s.quit()
	writeJSON(w, map[string]bool{"ok": true})
}

// bye comes from the page as it closes. A reload asks for news again at once.
func (s *session) bye(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	s.byeAt = time.Now()
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// quit asks the program to close, stopping the running job between two files.
// It reports false when that had been asked for already.
func (s *session) quit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quitting {
		return false
	}
	s.quitting = true
	s.stopping.Store(true)
	return true
}

// finished reports whether the program can close: when it was asked to, or
// when its page was closed (and not reloaded), or went quiet long ago. A scan
// still running then is stopped, while a move or undo is let finish.
func (s *session) finished(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	closed := !s.byeAt.IsZero() && !s.lastSeen.After(s.byeAt) && now.Sub(s.byeAt) > closeGrace
	gone := s.quitting || closed || now.Sub(s.lastSeen) > idleLimit
	if gone && s.busy && s.kind == "scan" {
		s.stopping.Store(true)
	}
	return gone && !s.busy
}

// outcome is how a move, an undo, emptying _Quarantine, a stopped scan or a
// failure ended.
type outcome struct {
	Kind       string `json:"kind"` // "moved", "undone", "emptied", "stopped" or "error"
	Folder     string `json:"folder"`
	Quarantine string `json:"quarantine,omitempty"`
	Moved      int    `json:"moved"`
	Failed     int    `json:"failed"`
	Bytes      int64  `json:"bytes"`
	Restored   int    `json:"restored"`
	Missing    int    `json:"missing"`
	Blocked    int    `json:"blocked"`
	Stopped    bool   `json:"stopped"`
	GaveUp     bool   `json:"gaveUp,omitempty"`  // a move that met an error it could not go on after, in the log
	Folders    int    `json:"folders,omitempty"` // folders removed after a move, being left empty
	Deleted    int    `json:"deleted,omitempty"` // files deleted from _Quarantine
	Kept       int    `json:"kept,omitempty"`    // journal lines pointing outside _Quarantine, left alone
	Error      string `json:"error,omitempty"`
}

// previewView is a scan as the page shows it.
type previewView struct {
	Kind        string      `json:"kind"` // "preview"
	Folder      string      `json:"folder"`
	Quarantine  string      `json:"quarantine"`
	Media       int         `json:"media"`
	Groups      []groupView `json:"groups"`
	MoreGroups  int         `json:"moreGroups"`
	DupFiles    int         `json:"dupFiles"`
	DupBytes    int64       `json:"dupBytes"`
	Similar     []groupView `json:"similar"` // photos that look alike
	MoreSimilar int         `json:"moreSimilar"`
	Types       []typeView  `json:"types"` // non-media files by group, most first
	OtherFiles  int         `json:"otherFiles"`
	OtherBytes  int64       `json:"otherBytes"`
	Skipped     int         `json:"skipped"`
	System      int         `json:"system"`
	LeftOut     int         `json:"leftOut"` // folders left out on purpose
	Journal     int         `json:"journal"` // moves earlier clean-ups recorded
	Options     options     `json:"options"`
	Reference   string      `json:"reference,omitempty"`
}

type groupView struct {
	Keep   string   `json:"keep"`
	Reason string   `json:"reason"`
	Why    string   `json:"why"` // the reason as a code, for the simple page
	Size   int64    `json:"size"`
	Copies []string `json:"copies"`
	Kind   string   `json:"kind,omitempty"`  // see group
	Ref    bool     `json:"ref,omitempty"`   // Keep is the copy in the reference folder, in full
	Stays  bool     `json:"stays,omitempty"` // Keep may not move: it is in the reference folder or a folder to keep
	Sizes  []int64  `json:"sizes"`           // of Keep, then of each copy
	Dims   []string `json:"dims,omitempty"`  // likewise, width×height, for look-alikes
}

// view is a group as the page shows it.
func (p *plan) view(g group) groupView {
	why := p.ru.why(g)
	gv := groupView{Keep: shown(g.keeper), Reason: reasons[why], Why: why, Size: g.keeper.size, Kind: g.kind,
		Ref: g.keeper.ref, Stays: !movable(g.keeper), Sizes: []int64{g.keeper.size}}
	for _, f := range g.losers {
		gv.Copies = append(gv.Copies, shown(f))
		gv.Sizes = append(gv.Sizes, f.size)
	}
	if g.kind == "alike" {
		for _, f := range append([]*file{g.keeper}, g.losers...) {
			gv.Dims = append(gv.Dims, fmt.Sprintf("%d×%d", f.w, f.h))
		}
	}
	return gv
}

type typeView struct {
	Type  string   `json:"type"` // as groupOf gives it
	Files int      `json:"files"`
	Bytes int64    `json:"bytes"`
	Paths []string `json:"paths"`
}

func preview(p *plan) previewView {
	v := previewView{Kind: "preview", Folder: p.root, Quarantine: filepath.Join(p.root, quarantine),
		Media: len(p.scan.media), Skipped: p.scan.skipped, System: p.scan.system, LeftOut: p.scan.leftOut,
		Groups: []groupView{}, Similar: []groupView{}, Types: []typeView{}, Options: p.ru.options,
		Reference: p.ru.reference}
	for _, g := range p.groups {
		for _, f := range g.losers {
			v.DupFiles++
			v.DupBytes += f.size
		}
		if len(v.Groups) == maxGroupsShown {
			v.MoreGroups++
			continue
		}
		v.Groups = append(v.Groups, p.view(g))
	}
	for _, g := range p.similar {
		if len(v.Similar) == maxGroupsShown {
			v.MoreSimilar++
			continue
		}
		v.Similar = append(v.Similar, p.view(g))
	}

	index := map[string]int{}
	for _, f := range p.scan.other {
		t := groupOf(f)
		i, seen := index[t]
		if !seen {
			i = len(v.Types)
			index[t] = i
			v.Types = append(v.Types, typeView{Type: t, Paths: []string{}})
		}
		tv := &v.Types[i]
		tv.Files++
		tv.Bytes += f.size
		if len(tv.Paths) < maxPathsShown {
			tv.Paths = append(tv.Paths, filepath.FromSlash(f.rel))
		}
		v.OtherFiles++
		v.OtherBytes += f.size
	}
	sort.SliceStable(v.Types, func(i, j int) bool { return v.Types[i].Files > v.Types[j].Files })

	entries, _ := readJournal(filepath.Join(p.root, quarantine, journalName))
	v.Journal = len(entries)
	return v
}

func readJSON(w http.ResponseWriter, req *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
