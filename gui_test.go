package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// guiServer serves a new session the way gui does, at an address of its own.
func guiServer(t *testing.T) (*session, *httptest.Server) {
	t.Helper()
	s := newSession("photo_cleanup", "")
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = s.handler(srv.Listener.Addr().String())
	srv.Start()
	t.Cleanup(srv.Close)
	return s, srv
}

// call sends a request to the page's API and decodes the JSON reply into out.
func call(t *testing.T, s *session, srv *httptest.Server, method, path string, body, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Token", s.token)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode
}

// result waits for the running job to end and returns how it ended.
func result[T any](t *testing.T, s *session, srv *httptest.Server) T {
	t.Helper()
	var out T
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var st struct {
			Busy   bool
			Result json.RawMessage
		}
		call(t, s, srv, "GET", "/api/state", nil, &st)
		if !st.Busy && string(st.Result) != "null" {
			if err := json.Unmarshal(st.Result, &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatal("the job did not finish")
	return out
}

func TestGUIServesThePageAndGuardsTheAPI(t *testing.T) {
	s, srv := guiServer(t)
	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "<title>Photo cleanup</title>") ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("page: %d %q", res.StatusCode, res.Header)
	}

	for name, prepare := range map[string]func(*http.Request){
		"no token":    func(*http.Request) {},
		"wrong token": func(req *http.Request) { req.Header.Set("X-Token", "0123") },
		"other host":  func(req *http.Request) { req.Header.Set("X-Token", s.token); req.Host = "photos.example.com" },
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
		prepare(req)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, res.StatusCode)
		}
	}
	if code := call(t, s, srv, "GET", "/api/state", nil, nil); code != 200 {
		t.Errorf("with the token: %d", code)
	}
}

func TestGUIChecksTheFolder(t *testing.T) {
	s, srv := guiServer(t)
	root := library(t)
	type answer struct {
		Path    string
		OK      bool
		Journal int
	}
	var got answer
	call(t, s, srv, "POST", "/api/check", map[string]string{"path": root}, &got)
	if !got.OK || got.Journal != 0 {
		t.Errorf("%+v", got)
	}
	runOK(t, root, "--apply")
	call(t, s, srv, "POST", "/api/check", map[string]string{"path": `"` + root + `"`}, &got) // as "Copy as path" pastes it
	if !got.OK || got.Journal != 3 {
		t.Errorf("after a clean-up: %+v", got)
	}
	for _, path := range []string{"", "  ", filepath.Join(root, "nope")} {
		got = answer{}
		call(t, s, srv, "POST", "/api/check", map[string]string{"path": path}, &got)
		if got.OK {
			t.Errorf("%q counted as a folder", path)
		}
	}
}

func TestGUIScanMoveAndUndo(t *testing.T) {
	s, srv := guiServer(t)
	root := library(t)
	before := files(t, root)

	if code := call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, nil); code != 200 {
		t.Fatalf("scan: %d", code)
	}
	p := result[previewView](t, s, srv)
	if p.Kind != "preview" || p.Media != 3 || p.DupFiles != 1 || p.DupBytes != 9 || p.OtherFiles != 2 {
		t.Fatalf("preview %+v", p)
	}
	want := []groupView{{Keep: filepath.FromSlash("Trip/IMG_1.jpg"), Reason: "other copy is named like a copy", Why: "copy-name",
		Size: 9, Copies: []string{filepath.FromSlash("Trip/IMG_1 (1).jpg")}, Sizes: []int64{9, 9}}}
	if !reflect.DeepEqual(p.Groups, want) {
		t.Errorf("groups %+v", p.Groups)
	}
	types := []typeView{{"(empty)", 1, 0, []string{"empty.jpg"}}, {"txt", 1, 12, []string{filepath.FromSlash("Notes/notes.txt")}}}
	if !reflect.DeepEqual(p.Types, types) {
		t.Errorf("types %+v", p.Types)
	}

	// Leave the .txt files where they are.
	call(t, s, srv, "POST", "/api/move", map[string]any{"skipTypes": []string{"txt"}}, nil)
	m := result[outcome](t, s, srv)
	if m.Kind != "moved" || m.Moved != 2 || m.Failed != 0 || m.Bytes != 9 || m.Stopped || m.GaveUp {
		t.Errorf("move %+v", m)
	}
	left := map[string]string{"Trip/IMG_1.jpg": "photo one", "Trip/IMG_2.jpg": "photo two", "Notes/notes.txt": "packing list"}
	if got := files(t, root); !reflect.DeepEqual(got, left) {
		t.Errorf("left %v", got)
	}
	if code := call(t, s, srv, "POST", "/api/move", map[string]any{}, nil); code != http.StatusConflict {
		t.Errorf("a plan must be carried out once only, got %d", code)
	}

	call(t, s, srv, "POST", "/api/undo", map[string]string{"path": root}, nil)
	if u := result[outcome](t, s, srv); u.Kind != "undone" || u.Restored != 2 || u.Missing != 0 || u.Blocked != 0 {
		t.Errorf("undo %+v", u)
	}
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
}

func TestGUIRunsOneJobAtATime(t *testing.T) {
	s, srv := guiServer(t)
	release := make(chan struct{})
	defer close(release)
	s.start(httptest.NewRecorder(), "scan", t.TempDir(), nil, func(r *reporter) any { <-release; return nil })
	root := library(t)
	for _, path := range []string{"/api/scan", "/api/undo"} {
		if code := call(t, s, srv, "POST", path, map[string]string{"path": root}, nil); code != http.StatusConflict {
			t.Errorf("%s while busy: %d, want 409", path, code)
		}
	}
}

func TestGUIStopsTheJobBetweenFiles(t *testing.T) {
	s, srv := guiServer(t)
	s.start(httptest.NewRecorder(), "move", t.TempDir(), nil, func(r *reporter) any {
		for !r.stopped() {
			time.Sleep(time.Millisecond)
		}
		return outcome{Kind: "moved", Stopped: true}
	})
	call(t, s, srv, "POST", "/api/stop", map[string]string{}, nil)
	if got := result[outcome](t, s, srv); !got.Stopped {
		t.Errorf("%+v", got)
	}
}

func TestGUIRevealsOnlyTheLastFolder(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	saved := revealFolder
	revealFolder = func(dir string) error { mu.Lock(); defer mu.Unlock(); opened = append(opened, dir); return nil }
	defer func() { revealFolder = saved }()

	s, srv := guiServer(t)
	if code := call(t, s, srv, "POST", "/api/reveal", map[string]string{"what": "folder"}, nil); code != http.StatusConflict {
		t.Errorf("before any folder was chosen: %d", code)
	}
	root := library(t)
	call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, nil)
	result[previewView](t, s, srv)
	for _, what := range []string{"quarantine", "folder", "/etc"} {
		call(t, s, srv, "POST", "/api/reveal", map[string]string{"what": what}, nil)
	}
	resolved, _ := folder(root)
	mu.Lock()
	defer mu.Unlock()
	if want := []string{filepath.Join(resolved, quarantine), resolved, resolved}; !reflect.DeepEqual(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
}

func TestGUIPicksAFolder(t *testing.T) {
	saved := pickFolder
	defer func() { pickFolder = saved }()
	s, srv := guiServer(t)
	for _, tc := range []struct {
		path        string
		err         error
		unavailable bool
	}{{"/Users/me/Pictures", nil, false}, {"", nil, false}, {"", errNoPicker, true}} {
		pickFolder = func(string, string) (string, error) { return tc.path, tc.err }
		var got struct {
			Path        string
			Unavailable bool
		}
		call(t, s, srv, "POST", "/api/pick", map[string]string{}, &got)
		if got.Path != tc.path || got.Unavailable != tc.unavailable {
			t.Errorf("%+v, want %+v", got, tc)
		}
	}
}

func TestGUIClosesWhenThePageIsGone(t *testing.T) {
	now := time.Now()
	s := newSession("photo_cleanup", "")
	s.lastSeen = now
	if s.finished(now) {
		t.Error("closed while the page is open")
	}
	s.byeAt = now // the page was closed
	if s.finished(now.Add(closeGrace / 2)) {
		t.Error("closed before the grace period, when a reload could still come")
	}
	if !s.finished(now.Add(2 * closeGrace)) {
		t.Error("still open after the page was closed")
	}
	s.lastSeen = now.Add(time.Second) // ...but it was a reload
	if s.finished(now.Add(2 * closeGrace)) {
		t.Error("closed although the page came back")
	}
	if !s.finished(s.lastSeen.Add(idleLimit + time.Second)) {
		t.Error("still open long after the page went quiet")
	}

	scan := newSession("photo_cleanup", "")
	scan.busy, scan.kind = true, "scan"
	scan.quit()
	if scan.finished(time.Now()) || !scan.stopping.Load() {
		t.Error("quitting must stop the job and wait for it")
	}
	scan.busy = false
	if !scan.finished(time.Now()) {
		t.Error("still open after the job stopped")
	}

	move := newSession("photo_cleanup", "")
	move.busy, move.kind, move.byeAt, move.lastSeen = true, "move", now, now.Add(-time.Second)
	if move.finished(now.Add(2*closeGrace)) || move.stopping.Load() {
		t.Error("closing the page must let a move finish")
	}
}

// syncBuffer is a bytes.Buffer that gui can write to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestGUIStartsAndQuits(t *testing.T) {
	if out := startAndQuit(t); !strings.Contains(out, "photo_cleanup has closed.") {
		t.Errorf("did not say so when it closed:\n%s", out)
	}
}

// startAndQuit starts gui, opens its page, clicks Quit and returns what gui
// printed.
func startAndQuit(t *testing.T) string {
	t.Helper()
	t.Setenv("PHOTO_CLEANUP_NO_BROWSER", "1")
	var out syncBuffer
	done := make(chan int, 1)
	go func() { done <- gui("photo_cleanup", "", &out) }()

	var url string
	for deadline := time.Now().Add(5 * time.Second); url == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		url = regexp.MustCompile(`http://\S+`).FindString(out.String())
	}
	base, token, ok := strings.Cut(url, "/#")
	if !ok {
		t.Fatalf("no address in %q", out.String())
	}
	res, err := http.Get(base + "/")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("page: %v %v", res, err)
	}
	res.Body.Close()
	req, _ := http.NewRequest("POST", base+"/api/quit", nil)
	req.Header.Set("X-Token", token)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("quit: %v %v", res, err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d\n%s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not close after Quit")
	}
	return out.String()
}

func TestGUIScanTakesOptions(t *testing.T) {
	s, srv := guiServer(t)
	root, _ := folder(library(t))
	bad := map[string]any{"path": root, "options": map[string]any{"prefer": []string{"/nowhere/at/all"}}}
	if code := call(t, s, srv, "POST", "/api/scan", bad, nil); code != http.StatusBadRequest {
		t.Errorf("a folder to keep that is not there: %d, want 400", code)
	}
	call(t, s, srv, "POST", "/api/scan", map[string]any{"path": root, "options": map[string]any{"duplicatesOnly": true}}, nil)
	if p := result[previewView](t, s, srv); !p.Options.DuplicatesOnly || p.OtherFiles != 0 || p.DupFiles != 1 {
		t.Errorf("preview %+v", p)
	}
}

func TestGUIMoveTakesChoices(t *testing.T) {
	s, srv := guiServer(t)
	root, _ := folder(library(t))
	call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, nil)
	result[previewView](t, s, srv)
	if code := call(t, s, srv, "POST", "/api/move", map[string]any{"skipGroups": []int{5}}, nil); code != http.StatusBadRequest {
		t.Errorf("a group that is not there: %d, want 400", code)
	}
	// Keep the copy named like a copy instead, and leave the other files alone.
	call(t, s, srv, "POST", "/api/move", map[string]any{"keep": map[string]int{"0": 1}, "skipTypes": []string{"txt", "(empty)"}}, nil)
	if m := result[outcome](t, s, srv); m.Kind != "moved" || m.Moved != 1 {
		t.Errorf("move %+v", m)
	}
	if !exist(root, "Trip/IMG_1 (1).jpg") || exist(root, "Trip/IMG_1.jpg") || !exist(root, "Notes/notes.txt") {
		t.Errorf("left %v", files(t, root))
	}
}

func TestGUIEmptiesTheQuarantine(t *testing.T) {
	s, srv := guiServer(t)
	root, _ := folder(library(t))
	runOK(t, root, "--apply")
	call(t, s, srv, "POST", "/api/empty", map[string]string{"path": root}, nil)
	if e := result[outcome](t, s, srv); e.Kind != "emptied" || e.Deleted != 3 || e.Bytes != 21 {
		t.Errorf("empty %+v", e)
	}
	call(t, s, srv, "POST", "/api/empty", map[string]string{"path": root}, nil)
	if e := result[outcome](t, s, srv); e.Kind != "error" || !strings.Contains(e.Error, "nothing") {
		t.Errorf("emptying again %+v", e)
	}
}

func TestGUIShowsEachLookAlike(t *testing.T) {
	s, srv := guiServer(t)
	root := t.TempDir()
	small := jpegOf(scene(300, 200, 5), 70)
	write(t, root, "big.jpg", string(jpegOf(scene(600, 400, 5), 92)))
	write(t, root, "small.jpg", string(small))
	var scan struct{ Job int }
	call(t, s, srv, "POST", "/api/scan", map[string]any{"path": root, "options": map[string]any{"similar": "close"}}, &scan)
	p := result[previewView](t, s, srv)
	if len(p.Similar) != 1 || p.Similar[0].Kind != "alike" || p.Similar[0].Dims[1] != "300×200" {
		t.Fatalf("look-alikes %+v", p.Similar)
	}
	get := func(m string) (int, string) {
		res, err := srv.Client().Get(fmt.Sprintf("%s/api/picture?token=%s&job=%d&set=similar&g=0&m=%s", srv.URL, s.token, scan.Job, m))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	if code, body := get("1"); code != 200 || body != string(small) {
		t.Errorf("the second look-alike: %d, %d bytes", code, len(body))
	}
	if code, _ := get("2"); code != http.StatusNotFound {
		t.Errorf("a member that is not there: %d", code)
	}
}

func TestGUIMoveTakesTheTickForEmptyFolders(t *testing.T) {
	s, srv := guiServer(t)
	root, _ := folder(library(t))
	call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, nil)
	result[previewView](t, s, srv)
	call(t, s, srv, "POST", "/api/move", map[string]any{"removeEmpty": true}, nil)
	if m := result[outcome](t, s, srv); m.Folders != 1 || exist(root, "Notes") {
		t.Errorf("move %+v, Notes still there: %v", m, exist(root, "Notes"))
	}
}

func TestGUIMoveNeedsTheLastScansPlan(t *testing.T) {
	s, _ := guiServer(t)
	s.plan = &plan{}
	w := httptest.NewRecorder()
	s.start(w, "move", t.TempDir(), &plan{}, func(r *reporter) any { return nil })
	if w.Code != http.StatusConflict {
		t.Errorf("a plan that is not the last scan's: %d, want 409", w.Code)
	}
}

func TestGUIEmptiesOnlyWhatItAskedAbout(t *testing.T) {
	s, srv := guiServer(t)
	root, _ := folder(library(t))
	runOK(t, root, "--apply")
	call(t, s, srv, "POST", "/api/empty", map[string]any{"path": root, "expect": 2}, nil)
	if e := result[outcome](t, s, srv); e.Kind != "error" || !strings.Contains(e.Error, "changed") {
		t.Errorf("asked about 2 files with 3 in _Quarantine: %+v", e)
	}
	if got := len(journalLines(t, root)); got != 3 {
		t.Errorf("journal has %d lines, want 3", got)
	}
}
