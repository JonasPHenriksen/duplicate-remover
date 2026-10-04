package main

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The simple edition is photo_cleanup for people who would rather have large
// text, plain words and pictures of their photos than file paths, in Danish or
// English. It is the same program with another page, so the two editions can
// undo each other's clean-ups. build.sh builds it as photo_cleanup-simple-*.

//go:embed simple.html
var simplePage []byte

// edition is "simple" in the simple edition and "" in the regular one. build.sh
// sets it with -ldflags "-X main.edition=simple".
var edition string

func simpleEdition() bool { return edition == "simple" }

// systemLanguage is "da" when the computer is set up in Danish, "en" when it is
// set up in another language, and "" when its environment does not say, as on
// Windows. The simple page then goes by the web browser's language.
func systemLanguage() string {
	first, _, _ := strings.Cut(os.Getenv("LANGUAGE"), ":") // a list, such as da:en
	for _, locale := range []string{first, os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG")} {
		if lang := languageOf(locale); lang != "" {
			return lang
		}
	}
	return ""
}

// languageOf is "da" for a Danish locale such as da_DK.UTF-8, "en" for a locale
// of another language, and "" for no locale or the C one.
func languageOf(locale string) string {
	name, _, _ := strings.Cut(locale, ".")
	name, _, _ = strings.Cut(name, "@")
	name, _, _ = strings.Cut(strings.ReplaceAll(name, "-", "_"), "_")
	switch strings.ToLower(name) {
	case "", "c", "posix":
		return ""
	case "da":
		return "da"
	}
	return "en"
}

// picturesFolder is the folder the desktop keeps photos in, such as ~/Billeder
// on a Danish Linux Mint, for the simple page to suggest. It is "" on macOS,
// where that folder holds the Photos library, which is no place to tidy up,
// and on Windows, where OneDrive can keep the real one somewhere else.
func picturesFolder() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	return xdgPictures(home, config)
}

// xdgPictures reads XDG_PICTURES_DIR from user-dirs.dirs in config, where Linux
// desktops write the names of the user's folders in the user's language. It
// returns that folder, or ~/Pictures without one, if it is a folder and not
// home itself, which is how user-dirs.dirs says there is none.
func xdgPictures(home, config string) string {
	dir := filepath.Join(home, "Pictures")
	if data, err := os.ReadFile(filepath.Join(config, "user-dirs.dirs")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "XDG_PICTURES_DIR="); ok {
				value = strings.Trim(value, `"`)
				if rest, ok := strings.CutPrefix(value, "$HOME"); ok {
					value = home + rest
				}
				dir = filepath.Clean(value)
			}
		}
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || !filepath.IsAbs(dir) || dir == filepath.Clean(home) {
		return ""
	}
	return dir
}

// pictureTypes are the photos web browsers can show, by extension. Only Safari
// shows HEIC: other browsers fail to, and the page shows its stand-in instead.
var pictureTypes = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "jpe": "image/jpeg", "jfif": "image/jpeg",
	"png": "image/png", "gif": "image/gif", "webp": "image/webp", "bmp": "image/bmp", "dib": "image/bmp",
	"avif": "image/avif", "heic": "image/heic", "heif": "image/heif",
}

// maxPictureSize is the largest photo the page gets to show.
const maxPictureSize = 64 << 20

// picture sends the page a photo from the last scan, so that the simple page can
// show what each group of duplicates looks like. The page asks for a group by
// its number and gets the copy that stays: it can never name a path. Only
// photos a browser can show are sent, and only while the scan is the last job.
func (s *session) picture(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	g, err := strconv.Atoi(q.Get("g"))
	var f *file
	s.mu.Lock()
	if err == nil && s.plan != nil && q.Get("job") == strconv.Itoa(s.job) && 0 <= g && g < len(s.plan.groups) {
		f = s.plan.groups[g].keeper
	}
	s.mu.Unlock()
	if f == nil || pictureTypes[extension(f.rel)] == "" || f.size > maxPictureSize {
		http.NotFound(w, req)
		return
	}
	fh, err := os.Open(f.path)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	defer fh.Close()
	h := w.Header()
	h.Set("Content-Type", pictureTypes[extension(f.rel)])
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, req, "", time.Time{}, fh)
}

// windowText is what gui prints in the window the program runs in.
type windowText struct{ open, noBrowser, closed string }

// windowWords is windowText in English in the regular edition, and in the
// simple one in the computer's language.
func windowWords(lang string) windowText {
	switch {
	case !simpleEdition():
		return windowText{
			open: "photo_cleanup is open in your web browser. If it did not open, go to\n  %s\n\n" +
				"Keep this window open while you use it: closing it quits photo_cleanup.\n",
			noBrowser: "Could not open the web browser:",
			closed:    "photo_cleanup has closed.",
		}
	case lang == "da":
		return windowText{
			open: "Billedoprydning er åbnet i din webbrowser. Hvis den ikke kom frem, så gå til\n  %s\n\n" +
				"Lad dette vindue være åbent, mens du bruger programmet. Lukker du det, lukker Billedoprydning også.\n",
			noBrowser: "Webbrowseren kunne ikke åbnes:",
			closed:    "Billedoprydning er lukket. Du kan lukke dette vindue.",
		}
	}
	return windowText{
		open: "Photo cleanup is open in your web browser. If it did not open, go to\n  %s\n\n" +
			"Keep this window open while you use it: closing it quits Photo cleanup.\n",
		noBrowser: "Could not open the web browser:",
		closed:    "Photo cleanup has closed. You can close this window.",
	}
}

// pickerTitle is the title of the folder picker, in the page's language.
func pickerTitle(lang string) string {
	if lang == "da" {
		return "Vælg den mappe, du vil rydde op i"
	}
	return "Choose the folder to clean up"
}
