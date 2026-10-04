package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// asSimpleEdition makes the rest of the test run as the simple edition.
func asSimpleEdition(t *testing.T) {
	t.Helper()
	saved := edition
	edition = "simple"
	t.Cleanup(func() { edition = saved })
}

// setLocale sets the environment that systemLanguage reads.
func setLocale(t *testing.T, language, lcAll, lcMessages, lang string) {
	t.Helper()
	t.Setenv("LANGUAGE", language)
	t.Setenv("LC_ALL", lcAll)
	t.Setenv("LC_MESSAGES", lcMessages)
	t.Setenv("LANG", lang)
}

func TestEachEditionServesItsOwnPage(t *testing.T) {
	for name, simple := range map[string]bool{"regular": false, "simple": true} {
		t.Run(name, func(t *testing.T) {
			want := guiPage
			if simple {
				asSimpleEdition(t)
				want = simplePage
			}
			_, srv := guiServer(t)
			res, err := srv.Client().Get(srv.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			page, _ := io.ReadAll(res.Body)
			res.Body.Close()
			// Only the simple page shows pictures.
			csp := res.Header.Get("Content-Security-Policy")
			if !bytes.Equal(page, want) || strings.Contains(csp, "img-src 'self'") != simple {
				t.Errorf("%d bytes of page, CSP %q", len(page), csp)
			}
		})
	}
}

func TestOnlyTheSimpleEditionTellsThePageTheLanguage(t *testing.T) {
	setLocale(t, "", "", "", "da_DK.UTF-8")
	for name, simple := range map[string]bool{"regular": false, "simple": true} {
		t.Run(name, func(t *testing.T) {
			if simple {
				asSimpleEdition(t)
			}
			s, srv := guiServer(t)
			var state map[string]any
			call(t, s, srv, "GET", "/api/state", nil, &state)
			lang, told := state["lang"]
			if told != simple || (simple && lang != "da") {
				t.Errorf("lang %v in %v", lang, state)
			}
		})
	}
}

func TestSimpleEditionTalksInTheComputersLanguage(t *testing.T) {
	asSimpleEdition(t)
	setLocale(t, "", "", "", "da_DK.UTF-8")
	out := startAndQuit(t)
	if !strings.Contains(out, "Billedoprydning er åbnet") || !strings.Contains(out, "Billedoprydning er lukket.") {
		t.Errorf("not in Danish:\n%s", out)
	}
	setLocale(t, "", "", "", "en_GB.UTF-8")
	if out := startAndQuit(t); !strings.Contains(out, "Photo cleanup has closed.") {
		t.Errorf("not in English:\n%s", out)
	}
}

func TestSystemLanguage(t *testing.T) {
	for _, tc := range []struct {
		language, lcAll, lcMessages, lang string
		want                              string
	}{
		{"", "", "", "da_DK.UTF-8", "da"},
		{"", "", "", "da_DK@euro", "da"},
		{"", "", "", "da", "da"},
		{"", "", "", "en_US.UTF-8", "en"},
		{"", "", "", "de_DE.UTF-8", "en"}, // the page speaks English to everyone else
		{"da:en", "", "", "en_US.UTF-8", "da"},
		{"", "", "da_DK.UTF-8", "en_DK.UTF-8", "da"},
		{"", "en_US.UTF-8", "", "da_DK.UTF-8", "en"},
		{"", "", "", "C.UTF-8", ""},
		{"", "", "", "POSIX", ""},
		{"", "", "", "", ""}, // Windows: the page asks the browser
	} {
		setLocale(t, tc.language, tc.lcAll, tc.lcMessages, tc.lang)
		if got := systemLanguage(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc, got, tc.want)
		}
	}
}

func TestPicturesFolderComesFromTheDesktopSettings(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config")
	mkdir := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := t.TempDir()
	for _, tc := range []struct {
		dirs   string // user-dirs.dirs, if any
		folder string // a folder to make first
		want   string
	}{
		{"", "", ""}, // nothing at all
		{"", "Pictures", filepath.Join(home, "Pictures")},
		{"XDG_DESKTOP_DIR=\"$HOME/Skrivebord\"\nXDG_PICTURES_DIR=\"$HOME/Billeder\"\n", "", ""}, // not made yet
		{"# written by xdg-user-dirs-update\nXDG_PICTURES_DIR=\"$HOME/Billeder\"\n", "Billeder", filepath.Join(home, "Billeder")},
		{"XDG_PICTURES_DIR=\"$HOME/\"\n", "", ""}, // how it says there is none
		{"XDG_PICTURES_DIR=\"" + elsewhere + "\"\n", "", elsewhere},
	} {
		if tc.folder != "" {
			mkdir(filepath.Join(home, tc.folder))
		}
		if tc.dirs != "" {
			write(t, config, "user-dirs.dirs", tc.dirs)
		}
		if got := xdgPictures(home, config); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.dirs, got, tc.want)
		}
	}
}

func TestThePickerTalksInThePagesLanguage(t *testing.T) {
	saved := pickFolder
	defer func() { pickFolder = saved }()
	var title, start string
	pickFolder = func(ti, st string) (string, error) { title, start = ti, st; return "", nil }

	s, srv := guiServer(t)
	s.pictures = "/home/anna/Billeder" // as the simple edition suggests it
	for lang, want := range map[string]string{"da": "Vælg den mappe, du vil rydde op i", "en": "Choose the folder to clean up",
		"": "Choose the folder to clean up"} {
		call(t, s, srv, "POST", "/api/pick", map[string]string{"lang": lang}, nil)
		if title != want || start != s.pictures {
			t.Errorf("%q: title %q in %q", lang, title, start)
		}
	}
}

func TestWindowsPickerTakesTheTitle(t *testing.T) {
	const regular = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form -Property @{ TopMost = $true }
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = 'Choose the folder to clean up'
$dialog.ShowNewFolderButton = $false
if ($dialog.ShowDialog($owner) -eq 'OK') { [Console]::Out.Write($dialog.SelectedPath) }
`
	if got := windowsPicker(pickerTitle("")); got != regular {
		t.Errorf("the regular edition's picker changed:\n%s", got)
	}
	if got := windowsPicker("Anna's"); !strings.Contains(got, "$dialog.Description = 'Anna''s'\n") {
		t.Errorf("a quote in the title is not escaped:\n%s", got)
	}
}

func TestPicturesAreOfDuplicatesInTheLastScanOnly(t *testing.T) {
	s, srv := guiServer(t)
	root := library(t)
	write(t, root, "Clips/a.mov", "same clip")
	write(t, root, "Clips/b.mov", "same clip")
	var scan struct{ Job int }
	call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, &scan)
	p := result[previewView](t, s, srv)
	group := func(ext string) string {
		for i, g := range p.Groups {
			if strings.HasSuffix(g.Keep, ext) {
				return strconv.Itoa(i)
			}
		}
		t.Fatalf("no %s group in %+v", ext, p.Groups)
		return ""
	}
	photo, video := group(".jpg"), group(".mov")
	get := func(query string) (int, string, string) {
		t.Helper()
		res, err := srv.Client().Get(srv.URL + "/api/picture?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), string(body)
	}

	token, job := "token="+s.token, "&job="+strconv.Itoa(scan.Job)
	if code, kind, body := get(token + job + "&g=" + photo); code != 200 || kind != "image/jpeg" || body != "photo one" {
		t.Errorf("the photo: %d %s %q", code, kind, body)
	}
	for _, query := range []string{
		token + job + "&g=" + video, // no browser shows a .mov as a picture
		token + job + "&g=2",
		token + job + "&g=-1",
		token + job + "&g=../../etc/passwd",
		token + "&job=" + strconv.Itoa(scan.Job+1) + "&g=" + photo,
	} {
		if code, _, _ := get(query); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", query, code)
		}
	}
	if code, _, _ := get(job[1:] + "&g=" + photo); code != http.StatusForbidden {
		t.Errorf("without the token: %d, want 403", code)
	}

	call(t, s, srv, "POST", "/api/move", map[string]any{}, nil)
	result[outcome](t, s, srv)
	if code, _, _ := get(token + job + "&g=" + photo); code != http.StatusNotFound {
		t.Errorf("after the move: %d, want 404", code)
	}
}

func TestAMoveThatCannotWriteSaysItGaveUp(t *testing.T) {
	skipWithoutPermissions(t)
	s, srv := guiServer(t)
	root := library(t)
	os.Chmod(root, 0o555) // like a locked memory card, or a drive mounted read-only
	defer os.Chmod(root, 0o755)

	call(t, s, srv, "POST", "/api/scan", map[string]string{"path": root}, nil)
	if p := result[previewView](t, s, srv); p.DupFiles != 1 {
		t.Fatalf("preview %+v", p)
	}
	call(t, s, srv, "POST", "/api/move", map[string]any{}, nil)
	if m := result[outcome](t, s, srv); m.Kind != "moved" || !m.GaveUp || m.Moved != 0 {
		t.Errorf("move %+v", m)
	}
}

// simpleWords are the texts of the simple page, by language and key.
func simpleWords(t *testing.T) map[string]map[string]any {
	t.Helper()
	_, rest, _ := strings.Cut(string(simplePage), `<script type="application/json" id="words">`)
	block, _, _ := strings.Cut(rest, "</script>")
	var words map[string]map[string]any
	if err := json.Unmarshal([]byte(block), &words); err != nil {
		t.Fatal(err)
	}
	return words
}

// shape is what a text must have alike in both languages: whether it has a
// form for one and for more, and the names it fills in.
func shape(text any) string {
	var forms []string
	switch v := text.(type) {
	case string:
		forms = []string{v}
	case map[string]any:
		one, ok1 := v["one"].(string)
		other, ok2 := v["other"].(string)
		if !ok1 || !ok2 || len(v) != 2 {
			return "broken"
		}
		forms = []string{one, other}
	default:
		return "broken"
	}
	names := map[string]bool{}
	for _, form := range forms {
		for _, m := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(form, -1) {
			names[m[1]] = true
		}
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return strconv.Itoa(len(forms)) + " forms, " + strings.Join(sorted, " ")
}

func TestSimplePageSaysEverythingInBothLanguages(t *testing.T) {
	words := simpleWords(t)
	en, da := words["en"], words["da"]
	if len(words) != 2 || len(en) < 100 {
		t.Fatalf("%d languages, %d English texts", len(words), len(en))
	}
	for key, text := range en {
		if _, ok := da[key]; !ok {
			t.Errorf("%s: no Danish", key)
		} else if a, b := shape(text), shape(da[key]); a != b || a == "broken" {
			t.Errorf("%s: %s in English, %s in Danish", key, a, b)
		}
	}
	for key := range da {
		if _, ok := en[key]; !ok {
			t.Errorf("%s: no English", key)
		}
	}

	page := string(simplePage)
	for _, m := range regexp.MustCompile(`\btx?\('(\w+)'|data-t(?:-label)?="(\w+)"`).FindAllStringSubmatch(page, -1) {
		if key := m[1] + m[2]; en[key] == nil {
			t.Errorf("the page asks for %s, which has no words", key)
		}
	}
	for key := range en {
		if !strings.Contains(page, "'"+key+"'") && !strings.Contains(page, `="`+key+`"`) {
			t.Errorf("%s is never used", key)
		}
	}
}

func TestSimplePageKnowsWhichFilesArePhotosAndVideos(t *testing.T) {
	sets := map[string][]string{}
	for _, m := range regexp.MustCompile(`const (\w+Ext) = new Set\('([^']*)'\.split\(' '\)\);`).FindAllStringSubmatch(string(simplePage), -1) {
		sets[m[1]] = strings.Fields(m[2])
	}
	var shown []string
	for ext := range pictureTypes {
		shown = append(shown, ext)
	}
	sort.Strings(shown)
	page := append([]string(nil), sets["pictureExt"]...)
	sort.Strings(page)
	if strings.Join(page, " ") != strings.Join(shown, " ") {
		t.Errorf("the page shows pictures of %q, the program sends %q", page, shown)
	}
	if len(sets["videoExt"]) < 30 {
		t.Fatalf("videoExt: %q", sets["videoExt"])
	}
	for _, ext := range append(sets["pictureExt"], sets["videoExt"]...) {
		if !mediaExt[ext] {
			t.Errorf("the page takes .%s for a photo or video, the program does not", ext)
		}
	}
}
