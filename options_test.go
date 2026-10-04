package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// made-up photos

// scene draws a made-up photo of waves of colour, the same picture at any size.
// Scenes of other numbers look different.
func scene(w, h, n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fx, fy, phase := 1.5+float64(n%4), 1+float64(n%3), float64(n)*1.3
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			u, v := float64(x)/float64(w), float64(y)/float64(h)
			a := math.Sin(2*math.Pi*(u*fx+v*fy) + phase)
			b := math.Sin(2*math.Pi*(u*(fy+1)-v*fx) + phase/2)
			c := 0.5 + 0.25*a + 0.25*b
			img.SetRGBA(x, y, color.RGBA{uint8(255 * c), uint8(255 * (1 - c)), uint8(128 + 100*a), 255})
		}
	}
	return img
}

// turned is a picture turned a quarter to the right.
func turned(img image.Image) *image.RGBA {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.Set(b.Dy()-1-y, x, img.At(x, y))
		}
	}
	return out
}

func jpegOf(img image.Image, quality int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func pngOf(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// withSegment puts a segment into a JPEG, right after its start.
func withSegment(jpg []byte, marker byte, payload []byte) []byte {
	seg := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{0xFF, 0xD8}, append(seg, payload...)...)
	return append(out, jpg[2:]...)
}

// exif is an APP1 payload with an orientation and, if given, a thumbnail.
func exif(orientation int, thumb []byte) []byte {
	le := binary.LittleEndian
	entry := func(tag, kind uint16, value uint32) []byte {
		e := make([]byte, 12)
		le.PutUint16(e[0:], tag)
		le.PutUint16(e[2:], kind)
		le.PutUint32(e[4:], 1)
		le.PutUint32(e[8:], value)
		return e
	}
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	// The first directory takes 18 bytes from 8, the second 30 from 26.
	tiff = le.AppendUint16(tiff, 1)
	tiff = append(tiff, entry(0x0112, 3, uint32(orientation))...)
	if thumb == nil {
		tiff = le.AppendUint32(tiff, 0)
	} else {
		tiff = le.AppendUint32(tiff, 26)
		tiff = le.AppendUint16(tiff, 2)
		tiff = append(tiff, entry(0x0201, 4, 56)...)
		tiff = append(tiff, entry(0x0202, 4, uint32(len(thumb)))...)
		tiff = le.AppendUint32(tiff, 0)
		tiff = append(tiff, thumb...)
	}
	return append([]byte("Exif\x00\x00"), tiff...)
}

// lookOfBytes is lookOf for a photo written to a file of the given name.
func lookOfBytes(t *testing.T, name string, data []byte) (uint64, int, int) {
	t.Helper()
	p := write(t, t.TempDir(), name, string(data))
	info, _ := os.Stat(p)
	look, w, h, err := lookOf(&file{path: p, rel: name, size: info.Size()})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return look, w, h
}

// resolved is a temporary folder as photo_cleanup sees it, with links resolved.
func resolved(t *testing.T) string {
	t.Helper()
	root, _ := folder(t.TempDir())
	return root
}

func planFor(t *testing.T, root string, o options) *plan {
	t.Helper()
	ru, err := newRules(root, o)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := prepareWith(root, ru, quiet())
	if !ok {
		t.Fatal("stopped")
	}
	return p
}

// kept lists each group as its keeper, then its losers.
func kept(groups []group) [][]string {
	var out [][]string
	for _, g := range groups {
		names := []string{g.keeper.rel}
		for _, f := range g.losers {
			names = append(names, f.rel)
		}
		out = append(out, names)
	}
	return out
}

// ---------------------------------------------------------------------------
// reading JPEGs

func TestReadJPEGFindsTagsOrientationAndThumbnail(t *testing.T) {
	photo := jpegOf(scene(300, 200, 1), 90)
	thumb := jpegOf(scene(60, 40, 1), 70)
	tagged := withSegment(withSegment(photo, 0xFE, []byte("rated 5 stars")), 0xE1, exif(6, thumb))

	info, err := readJPEG(bytes.NewReader(tagged), int64(len(tagged)))
	if err != nil {
		t.Fatal(err)
	}
	if info.width != 300 || info.height != 200 || info.orientation != 6 {
		t.Errorf("%d×%d, orientation %d", info.width, info.height, info.orientation)
	}
	got := tagged[info.thumb.off : info.thumb.off+info.thumb.n]
	if !bytes.Equal(got, thumb) {
		t.Errorf("thumbnail of %d bytes at %d, want %d bytes", info.thumb.n, info.thumb.off, len(thumb))
	}
	// The picture is the JPEG without the two segments put in.
	if want := int64(len(tagged) - len(photo)); info.tags != want {
		t.Errorf("%d bytes of tags, want %d", info.tags, want)
	}
	picture := make([]byte, info.pictureSize)
	if _, err := (pictureReader{bytes.NewReader(tagged), info.picture}).ReadAt(picture, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(picture, photo) {
		t.Errorf("the picture is %d bytes, want the %d of the untagged JPEG", len(picture), len(photo))
	}
}

func TestReadJPEGRefusesWhatIsNot(t *testing.T) {
	photo := jpegOf(scene(40, 30, 1), 80)
	for name, data := range map[string][]byte{
		"empty":       nil,
		"png":         pngOf(scene(10, 10, 1)),
		"cut short":   photo[:3],
		"bad length":  append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, photo[2:]...),
		"no marker":   append([]byte{0xFF, 0xD8, 0x12}, photo[3:]...),
		"short exif":  withSegment(photo, 0xE1, []byte("Exif\x00\x00II*\x00")),
		"wild offset": withSegment(photo, 0xE1, append([]byte("Exif\x00\x00II*\x00\xff\xff\xff\x7f"), make([]byte, 20)...)),
	} {
		info, err := readJPEG(bytes.NewReader(data), int64(len(data)))
		if strings.HasPrefix(name, "short") || strings.HasPrefix(name, "wild") {
			if err != nil || info.orientation != 1 || info.thumb.n != 0 {
				t.Errorf("%s: %v, orientation %d, thumbnail %d bytes", name, err, info.orientation, info.thumb.n)
			}
		} else if err == nil {
			t.Errorf("%s: read as a JPEG", name)
		}
	}
}

func FuzzReadJPEG(f *testing.F) {
	photo := jpegOf(scene(20, 20, 1), 80)
	f.Add(photo)
	f.Add(withSegment(photo, 0xE1, exif(3, photo)))
	f.Add([]byte{0xFF, 0xD8, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := readJPEG(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if info.pictureSize+info.tags > int64(len(data)) || info.thumb.off+info.thumb.n > int64(len(data)) {
			t.Errorf("%+v for %d bytes", info, len(data))
		}
	})
}

// ---------------------------------------------------------------------------
// looks

func TestCopiesLookAlikeAndOtherPhotosDont(t *testing.T) {
	look := func(name string, data []byte) uint64 { l, _, _ := lookOfBytes(t, name, data); return l }
	original := scene(800, 600, 5)
	base := look("a.jpg", jpegOf(original, 92))

	// A thumbnail letterboxed to 4:3 for a 3:2 photo, as cameras make them.
	wide := scene(900, 600, 5)
	thumb := image.NewRGBA(image.Rect(0, 0, 160, 120))
	draw.Draw(thumb, image.Rect(0, 7, 160, 113), scene(160, 106, 5), image.Point{}, draw.Src)
	withThumb := look("wide.jpg", withSegment(jpegOf(wide, 92), 0xE1, exif(1, jpegOf(thumb, 80))))

	for name, other := range map[string]uint64{
		"smaller":           look("small.jpg", jpegOf(scene(400, 300, 5), 75)),
		"saved worse":       look("worse.jpg", jpegOf(original, 40)),
		"as PNG":            look("a.png", pngOf(scene(640, 480, 5))),
		"turned, with EXIF": look("turned.jpg", withSegment(jpegOf(turned(original), 90), 0xE1, exif(8, nil))),
		"by its thumbnail":  withThumb,
		"with EXIF":         look("exif.jpg", withSegment(jpegOf(original, 90), 0xE1, exif(1, nil))),
	} {
		if d := bits.OnesCount64(base ^ other); d > closeLooks {
			t.Errorf("%s: %d bits differ, want at most %d", name, d, closeLooks)
		}
	}
	for n := 6; n < 12; n++ {
		if d := bits.OnesCount64(base ^ look("other.jpg", jpegOf(scene(800, 600, n), 92))); d <= looseLooks {
			t.Errorf("scene %d: only %d bits differ from scene 5, want more than %d", n, d, looseLooks)
		}
	}
	if _, w, h := lookOfBytes(t, "turned.jpg", withSegment(jpegOf(scene(80, 60, 1), 90), 0xE1, exif(6, nil))); w != 60 || h != 80 {
		t.Errorf("a photo turned a quarter is %d×%d upright, want 60×80", w, h)
	}
}

func TestLookAlikesAreListedNotMoved(t *testing.T) {
	root := resolved(t)
	write(t, root, "Trip/big.jpg", string(jpegOf(scene(800, 600, 5), 92)))
	write(t, root, "WhatsApp/small.jpg", string(jpegOf(scene(320, 240, 5), 70)))
	write(t, root, "Trip/other.jpg", string(jpegOf(scene(800, 600, 9), 92)))
	before := files(t, root)

	p := planFor(t, root, options{Similar: "close"})
	if got, want := kept(p.similar), [][]string{{"Trip/big.jpg", "WhatsApp/small.jpg"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("look-alikes %q, want %q", got, want)
	}
	if p.ru.why(p.similar[0]) != "pixels" || len(p.moves) != 0 {
		t.Errorf("why %q, moves %v", p.ru.why(p.similar[0]), p.moves)
	}
	runOK(t, root, "--similar", "--apply")
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("look-alikes moved without being chosen: %v", got)
	}
	if !strings.Contains(read(t, root, "_Quarantine/similar.csv"), filepath.FromSlash("WhatsApp/small.jpg")) {
		t.Error("similar.csv does not list the look-alike")
	}
}

// ---------------------------------------------------------------------------
// the same picture with other tags

func TestSamePictureWithOtherTagsCountsAsDuplicate(t *testing.T) {
	root := resolved(t)
	photo := jpegOf(scene(400, 300, 2), 90)
	write(t, root, "Trip/IMG_1.jpg", string(photo))
	write(t, root, "Trip/IMG_1 copy.jpg", string(photo))                                     // byte for byte the same
	write(t, root, "Album/IMG_1.jpg", string(withSegment(photo, 0xFE, []byte("Mormor 80")))) // captioned
	write(t, root, "Album/IMG_2.jpg", string(jpegOf(scene(400, 300, 2), 60)))                // saved again: not the same

	if got := kept(planFor(t, root, options{}).groups); len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("without the option: %q", got)
	}
	p := planFor(t, root, options{SameImage: true})
	got := kept(p.groups)
	if len(got) != 1 || got[0][0] != "Album/IMG_1.jpg" ||
		!reflect.DeepEqual(sorted(got[0][1:]), []string{"Trip/IMG_1 copy.jpg", "Trip/IMG_1.jpg"}) {
		t.Fatalf("groups %q, want the captioned one kept", got)
	}
	if g := p.groups[0]; g.kind != "tags" || p.ru.why(g) != "tags" {
		t.Errorf("kind %q, why %q", g.kind, p.ru.why(g))
	}
	runOK(t, root, "--same-image")
	if !strings.Contains(read(t, root, "_Quarantine/duplicates.csv"), "same picture, different tags") {
		t.Error("duplicates.csv does not say the tags differ")
	}
}

func sorted(s []string) []string {
	s = append([]string(nil), s...)
	sort.Strings(s)
	return s
}

// ---------------------------------------------------------------------------
// which copy stays, and where to look

func TestFoldersToKeepWin(t *testing.T) {
	root := resolved(t)
	write(t, root, "Album/IMG_1.jpg", "same")
	write(t, root, "Backup/IMG_1.jpg", "same")
	if got := kept(planFor(t, root, options{}).groups); got[0][0] != "Album/IMG_1.jpg" {
		t.Fatalf("without the option: %q", got)
	}
	p := planFor(t, root, options{Prefer: []string{filepath.Join(root, "Backup")}})
	if got := kept(p.groups); got[0][0] != "Backup/IMG_1.jpg" || p.ru.why(p.groups[0]) != "preferred" {
		t.Errorf("kept %q, because %q", got, p.ru.why(p.groups[0]))
	}
	if p := planFor(t, root, options{Prefer: []string{"Backup"}}); kept(p.groups)[0][0] != "Backup/IMG_1.jpg" {
		t.Error("a folder to keep can be given relative to the folder")
	}
}

func TestKeepTheNewest(t *testing.T) {
	root := resolved(t)
	old := write(t, root, "A/IMG_1.jpg", "same")
	write(t, root, "B/IMG_1.jpg", "same")
	os.Chtimes(old, time.Now(), time.Now().Add(-48*time.Hour))
	if got := kept(planFor(t, root, options{}).groups); got[0][0] != "A/IMG_1.jpg" {
		t.Fatalf("by default the oldest stays: %q", got)
	}
	p := planFor(t, root, options{KeepNewest: true})
	if got := kept(p.groups); got[0][0] != "B/IMG_1.jpg" || p.ru.why(p.groups[0]) != "newer" {
		t.Errorf("kept %q, because %q", got, p.ru.why(p.groups[0]))
	}
}

func TestMoreBackupFolderNames(t *testing.T) {
	root := resolved(t)
	write(t, root, "Gamle/IMG_1.jpg", "same")
	os.Chtimes(write(t, root, "Ferie/IMG_1.jpg", "same"), time.Now(), time.Now().Add(time.Hour))
	if got := kept(planFor(t, root, options{}).groups); got[0][0] != "Gamle/IMG_1.jpg" {
		t.Fatalf("by default the older one stays: %q", got)
	}
	p := planFor(t, root, options{Backups: []string{" GAMLE "}})
	if got := kept(p.groups); got[0][0] != "Ferie/IMG_1.jpg" || p.ru.why(p.groups[0]) != "dump" {
		t.Errorf("kept %q, because %q", got, p.ru.why(p.groups[0]))
	}
}

func TestFoldersToSkipAreLeftOut(t *testing.T) {
	root := resolved(t)
	write(t, root, "Trip/IMG_1.jpg", "same")
	write(t, root, "Scans/IMG_1.jpg", "same")
	write(t, root, "Scans/notes.txt", "notes")
	p := planFor(t, root, options{Skip: []string{"Scans"}})
	if len(p.groups) != 0 || len(p.scan.other) != 0 || p.scan.leftOut != 1 {
		t.Errorf("groups %q, other %d, left out %d", kept(p.groups), len(p.scan.other), p.scan.leftOut)
	}
}

func TestDuplicatesOnlyLeavesOtherFilesAlone(t *testing.T) {
	root, _ := folder(library(t))
	p := planFor(t, root, options{DuplicatesOnly: true})
	if len(p.moves) != 1 || p.moves[0].from != "Trip/IMG_1 (1).jpg" {
		t.Fatalf("moves %+v", p.moves)
	}
	runOK(t, root, "--duplicates-only", "--apply")
	if !exist(root, "Notes/notes.txt") || !exist(root, "empty.jpg") || exist(root, "Trip/IMG_1 (1).jpg") {
		t.Errorf("left %v", files(t, root))
	}
}

func TestReferenceFolderMovesCopiesOfWhatItHas(t *testing.T) {
	root, ref := resolved(t), resolved(t)
	write(t, ref, "2019/IMG_1.jpg", "imported")
	write(t, ref, "2019/IMG_1 again.jpg", "imported") // a copy inside the reference: not our business
	write(t, ref, "2019/IMG_3.jpg", "only there")
	write(t, root, "DCIM/IMG_1.jpg", "imported")
	write(t, root, "Album/IMG_1.jpg", "imported")
	write(t, root, "DCIM/IMG_2.jpg", "new")
	refBefore := files(t, ref)

	p := planFor(t, root, options{Reference: ref})
	if len(p.groups) != 1 || !p.groups[0].keeper.ref || p.ru.why(p.groups[0]) != "reference" {
		t.Fatalf("groups %q", kept(p.groups))
	}
	if got := sorted(kept(p.groups)[0][1:]); !reflect.DeepEqual(got, []string{"Album/IMG_1.jpg", "DCIM/IMG_1.jpg"}) {
		t.Errorf("moves %q, want every copy of the reference's photo", got)
	}
	runOK(t, root, "--reference", ref, "--apply")
	if got := files(t, root); !reflect.DeepEqual(got, map[string]string{"DCIM/IMG_2.jpg": "new"}) {
		t.Errorf("left %v", got)
	}
	if got := files(t, ref); !reflect.DeepEqual(got, refBefore) {
		t.Errorf("the reference folder changed: %v", got)
	}
	if !strings.Contains(read(t, root, "_Quarantine/duplicates.csv"), filepath.Join(ref, "2019")) {
		t.Error("duplicates.csv does not show the reference's copy in full")
	}
}

func TestOptionsAreChecked(t *testing.T) {
	root := resolved(t)
	write(t, root, "Album/x.txt", "x")
	outside := resolved(t)
	for name, o := range map[string]options{
		"keep outside":      {Prefer: []string{outside}},
		"keep missing":      {Prefer: []string{"Nope"}},
		"keep the folder":   {Prefer: []string{root}},
		"skip outside":      {Skip: []string{outside}},
		"reference inside":  {Reference: filepath.Join(root, "Album")},
		"reference holding": {Reference: filepath.Dir(root)},
		"reference itself":  {Reference: root},
		"reference missing": {Reference: filepath.Join(outside, "nope")},
		"looks":             {Similar: "kind of"},
	} {
		if _, err := newRules(root, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newRules(root, options{Prefer: []string{"Album"}, Skip: []string{filepath.Join(root, "Album")},
		Reference: outside, Similar: "loose"}); err != nil {
		t.Errorf("good options refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// choosing in the review

func TestChoicesChangeThePlan(t *testing.T) {
	root := resolved(t)
	for _, name := range []string{"a", "b", "c"} {
		write(t, root, "Trip/"+name+".jpg", "photo "+name)
		write(t, root, "Backup/"+name+".jpg", "photo "+name)
	}
	write(t, root, "big.jpg", string(jpegOf(scene(600, 400, 5), 92)))
	write(t, root, "small.jpg", string(jpegOf(scene(300, 200, 5), 80)))
	p := planFor(t, root, options{Similar: "close"})
	if len(p.groups) != 3 || len(p.similar) != 1 {
		t.Fatalf("groups %q, look-alikes %q", kept(p.groups), kept(p.similar))
	}
	for _, bad := range []choices{{SkipGroups: []int{3}}, {Similar: []int{-1}}, {Keep: map[string]int{"0": 2}},
		{Keep: map[string]int{"x": 1}}, {KeepSimilar: map[string]int{"1": 1}}} {
		if p.check(bad) == nil {
			t.Errorf("%+v passed", bad)
		}
	}
	c := choices{SkipGroups: []int{0}, Keep: map[string]int{"1": 1}, Similar: []int{0}, KeepSimilar: map[string]int{"0": 1}}
	if err := p.check(c); err != nil {
		t.Fatal(err)
	}
	p.choose(c)
	var from []string
	for _, m := range p.moves {
		from = append(from, m.from+" → "+strings.SplitN(m.to, "/", 3)[1])
	}
	want := []string{"Trip/b.jpg → duplicates", "Backup/c.jpg → duplicates", "big.jpg → similar"}
	if !reflect.DeepEqual(from, want) {
		t.Errorf("moves %q, want %q", from, want)
	}
	if !p.groups[0].chosen || p.ru.why(p.groups[0]) != "chosen" {
		t.Error("a copy kept by hand does not say so")
	}
}

// ---------------------------------------------------------------------------
// after moving

func TestFoldersLeftEmptyAreRemovedAndComeBackOnUndo(t *testing.T) {
	root := resolved(t)
	write(t, root, "Trip/IMG_1.jpg", "same")
	write(t, root, "Old/Phone/IMG_1 (1).jpg", "same")
	write(t, root, "Trip/IMG_2.jpg", "other")
	os.MkdirAll(filepath.Join(root, "Empty before"), 0o755)
	before := files(t, root)

	runOK(t, root, "--remove-empty", "--apply")
	if exist(root, "Old") || !exist(root, "Trip") || !exist(root, "Empty before") {
		t.Errorf("Old gone %v, Trip there %v, Empty before there %v", !exist(root, "Old"), exist(root, "Trip"), exist(root, "Empty before"))
	}
	if !strings.Contains(read(t, root, "_Quarantine/summary.txt"), "emptied, removed  : 2 folders") {
		t.Error(read(t, root, "_Quarantine/summary.txt"))
	}
	runOK(t, root, "--undo")
	if got := files(t, root); !reflect.DeepEqual(got, before) {
		t.Errorf("after undo %v", got)
	}
}

func TestEmptyingQuarantineDeletesOnlyWhatWasMoved(t *testing.T) {
	root, _ := folder(library(t))
	runOK(t, root, "--apply")
	outside := write(t, resolved(t), "precious.jpg", "keep me")
	write(t, root, "_Quarantine/mine.txt", "not ours")
	journal := filepath.Join(root, quarantine, journalName)
	f, _ := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"from":"x.jpg","to":"` + filepath.ToSlash(outside) + `"}` + "\n")
	f.WriteString(`{"from":"y.jpg","to":"_Quarantine/../Trip/IMG_2.jpg"}` + "\n")
	f.WriteString(`{"from":"z.txt","to":"_Quarantine/summary.txt"}` + "\n")
	f.Close()

	if _, err := emptyQuarantine(root, 4, quiet()); !errors.Is(err, errChanged) || len(journalLines(t, root)) != 6 {
		t.Fatalf("asked about 4 files with 6 in the journal: %v", err)
	}
	res, err := emptyQuarantine(root, 6, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if res.deleted != 3 || res.kept != 3 || res.failed != 0 || res.bytes != 21 {
		t.Errorf("%+v", res)
	}
	if !exist(filepath.Dir(outside), "precious.jpg") || !exist(root, "Trip/IMG_2.jpg") || !exist(root, "_Quarantine/summary.txt") {
		t.Fatal("deleted a file outside the folders in _Quarantine")
	}
	got := quarantineListing(t, root)
	if want := []string{"duplicates.csv", journalName, "mine.txt", "not-media.csv", "summary.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("_Quarantine holds %q, want %q", got, want)
	}
	if lines := journalLines(t, root); len(lines) != 3 {
		t.Errorf("journal keeps %q, want the 3 lines it did not delete", lines)
	}
	if _, err := emptyQuarantine(t.TempDir(), 0, quiet()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("without a journal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// speed

func TestReadingSeveralFilesAtOnceFindsTheSame(t *testing.T) {
	root := resolved(t)
	for i := 0; i < 40; i++ {
		data := strings.Repeat(string(rune('a'+i%26)), 70_000+i%7*1000)
		write(t, root, filepath.Join("A", "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".jpg"), data)
		if i%3 == 0 {
			write(t, root, filepath.Join("B", "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".jpg"), data)
		}
	}
	one := kept(planFor(t, root, options{OneAtATime: true}).groups)
	if got := kept(planFor(t, root, options{}).groups); !reflect.DeepEqual(got, one) || len(one) != 14 {
		t.Errorf("at once %q\none at a time %q", got, one)
	}
}

func TestFingerprintsAreRemembered(t *testing.T) {
	root := resolved(t)
	write(t, root, "A/IMG_1.jpg", "photo one")
	write(t, root, "B/IMG_1.jpg", "photo one")
	write(t, root, "A/IMG_2.jpg", "photo two")
	planFor(t, root, options{Cache: true})
	c := loadCache(root)
	if _, ok := c.old["B/IMG_1.jpg"]; len(c.old) != 2 || !ok {
		t.Fatalf("remembered %v, want the two files that were hashed", c.old)
	}

	// The cache is trusted while a file keeps its size, time and quick fingerprint...
	e := c.old["A/IMG_1.jpg"]
	e.Full = strings.Repeat("0", 64)
	c.old["A/IMG_1.jpg"] = e
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, e := range c.old {
		enc.Encode(e)
	}
	write(t, root, "_Quarantine/"+cacheName, b.String())
	if p := planFor(t, root, options{Cache: true}); len(p.groups) != 0 {
		t.Errorf("the cache was not used: %q", kept(p.groups))
	}
	// ...and read again once its time changes.
	os.Chtimes(filepath.Join(root, "A/IMG_1.jpg"), time.Now(), time.Now())
	if p := planFor(t, root, options{Cache: true}); len(p.groups) != 1 {
		t.Errorf("a changed file was not read again: %q", kept(p.groups))
	}
	if p := planFor(t, root, options{}); len(p.groups) != 1 {
		t.Error("without the option, the cache is not read")
	}
}

// ---------------------------------------------------------------------------
// command line

func TestOptionFlags(t *testing.T) {
	fl, err := parseFlags([]string{"--prefer", "Album", "--skip=Scans", "--backup", "gamle,old", "--similar=loose",
		"--reference", "/ref", "--keep-newest", "photos", "--duplicates-only", "--same-image", "--remove-empty",
		"--one-at-a-time", "--cache", "--apply"})
	want := options{DuplicatesOnly: true, SameImage: true, Similar: "loose", KeepNewest: true, Prefer: []string{"Album"},
		Backups: []string{"gamle", "old"}, Reference: "/ref", Skip: []string{"Scans"}, RemoveEmpty: true,
		OneAtATime: true, Cache: true}
	if err != nil || !reflect.DeepEqual(fl.options, want) || !fl.apply || !fl.optioned || !reflect.DeepEqual(fl.folders, []string{"photos"}) {
		t.Errorf("%+v, %v", fl, err)
	}
	if fl, _ := parseFlags([]string{"--similar", "photos"}); fl.options.Similar != "close" || len(fl.folders) != 1 {
		t.Errorf("--similar took the folder as its value: %+v", fl)
	}
	for _, args := range [][]string{{"--prefer"}, {"--apply=yes"}, {"--cache=1"}, {"--bogus"}} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	root := resolved(t)
	for _, args := range [][]string{{root, "--undo", "--cache"}, {"--gui", "--cache"}} {
		if code, out, _ := runCLI(args...); code != 2 || !strings.Contains(out, "Usage:") {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if code, _, errOut := runCLI(root, "--prefer", "/elsewhere"); code != 2 || !strings.Contains(errOut, "not") {
		t.Errorf("a bad option: exit %d, %q", code, errOut)
	}
}

// ---------------------------------------------------------------------------
// what the review found

// sofSized is a JPEG whose frame header claims another size.
func sofSized(jpg []byte, w, h int) []byte {
	out := bytes.Clone(jpg)
	i := bytes.Index(out, []byte{0xFF, 0xC0})
	binary.BigEndian.PutUint16(out[i+5:], uint16(h))
	binary.BigEndian.PutUint16(out[i+7:], uint16(w))
	return out
}

func TestDamagedPhotosDontStopTheLookAlikes(t *testing.T) {
	photo := jpegOf(scene(200, 150, 1), 90)
	for name, data := range map[string][]byte{
		"no rows":          sofSized(photo, 200, 0),
		"enormous":         sofSized(photo, 65000, 65000),
		"no-row thumbnail": withSegment(photo, 0xE1, exif(1, sofSized(jpegOf(scene(40, 30, 1), 80), 40, 0))),
	} {
		p := write(t, t.TempDir(), "x.jpg", string(data))
		info, _ := os.Stat(p)
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Errorf("%s: %v", name, v)
				}
			}()
			if _, _, _, err := lookOf(&file{path: p, rel: "x.jpg", size: info.Size()}); err == nil && name != "no-row thumbnail" {
				t.Errorf("%s: looked at", name)
			}
		}()
	}

	root := resolved(t)
	write(t, root, "big.jpg", string(jpegOf(scene(600, 400, 5), 92)))
	write(t, root, "small.jpg", string(jpegOf(scene(300, 200, 5), 80)))
	write(t, root, "broken.jpg", string(sofSized(photo, 200, 0)))
	if p := planFor(t, root, options{Similar: "close"}); len(p.similar) != 1 {
		t.Errorf("look-alikes %q", kept(p.similar))
	}
}

func TestAFileThatBreaksTheWorkIsSkipped(t *testing.T) {
	var errOut bytes.Buffer
	r := &reporter{out: io.Discard, errOut: &errOut}
	var done atomic.Int32
	parallel([]*file{{path: "a"}, {path: "b"}, {path: "c"}}, 2, r, func(_ int, f *file, _ []byte) {
		if f.path == "b" {
			panic("broken")
		}
		done.Add(1)
	})
	if done.Load() != 2 || !strings.Contains(errOut.String(), "skipped b after an error: broken") {
		t.Errorf("%d done, said %q", done.Load(), errOut.String())
	}
}

func TestFoldersAreKnownByWhatTheDiskSays(t *testing.T) {
	root := resolved(t)
	write(t, root, "Scans/IMG_1.jpg", "same")
	write(t, root, "Trip/IMG_1.jpg", "same")
	write(t, root, "A\u030arhus/IMG_2.jpg", "other") // Århus, with its ring written apart
	write(t, root, "Trip/IMG_2.jpg", "other")
	for _, typed := range []string{"scans", "\u00c5rhus"} {
		if _, err := os.Stat(filepath.Join(root, typed)); err != nil {
			t.Logf("this disk tells %q apart from how the folder is written", typed)
			continue
		}
		if p := planFor(t, root, options{Skip: []string{typed}}); p.scan.leftOut != 1 {
			t.Errorf("--skip %q: %d folders left out", typed, p.scan.leftOut)
		}
		if p := planFor(t, root, options{Prefer: []string{typed}}); p.groups[0].keeper.inKeep == p.groups[1].keeper.inKeep {
			t.Errorf("--prefer %q: kept %q", typed, kept(p.groups))
		}
	}
}

func TestCopiesInFoldersToKeepNeverMove(t *testing.T) {
	root := resolved(t)
	write(t, root, "Albums/2020/IMG_1.jpg", "same")
	write(t, root, "Albums/Best/IMG_1.jpg", "same")
	write(t, root, "Backup/IMG_1.jpg", "same")
	p := planFor(t, root, options{Prefer: []string{"Albums"}})
	if got := kept(p.groups); len(got) != 1 || len(got[0]) != 2 || got[0][1] != "Backup/IMG_1.jpg" {
		t.Fatalf("groups %q, want only the backup to move", got)
	}
	p.choose(choices{Keep: map[string]int{"0": 1}})
	if len(p.moves) != 0 {
		t.Errorf("keeping the backup instead moves %+v", p.moves)
	}
}

func TestLookAlikesOnlyCountAsMovedWhenTheyAre(t *testing.T) {
	root := resolved(t)
	write(t, root, "big.jpg", string(jpegOf(scene(600, 400, 5), 92)))
	write(t, root, "small.jpg", string(jpegOf(scene(300, 200, 5), 80)))
	write(t, root, "A/IMG_1.jpg", "same")
	write(t, root, "B/IMG_1 (1).jpg", "same")
	runOK(t, root, "--similar", "--apply")
	summary := read(t, root, "_Quarantine/summary.txt")
	if strings.Contains(summary, "look-alikes moved") || !strings.Contains(summary, "look-alike groups : 1") {
		t.Error(summary)
	}
	if csv := read(t, root, "_Quarantine/similar.csv"); strings.Contains(csv, "quarantined") || !exist(root, "small.jpg") {
		t.Error(csv)
	}
}

func TestIdenticalCopiesKeepTheirReasonWithTheSamePictureOption(t *testing.T) {
	root := resolved(t)
	photo := string(withSegment(jpegOf(scene(200, 150, 3), 90), 0xE1, exif(1, nil)))
	write(t, root, "Trip/Day 1/IMG_1.jpg", photo)
	write(t, root, "IMG_1.jpg", photo)
	if p := planFor(t, root, options{SameImage: true}); len(p.groups) != 1 || p.ru.why(p.groups[0]) != "deeper" {
		t.Errorf("groups %q, because %q", kept(p.groups), p.ru.why(p.groups[0]))
	}
}

func TestAFileIsPlannedToMoveOnce(t *testing.T) {
	photo, other := &file{rel: "photo.jpg", size: 5}, &file{rel: "big.jpg", size: 9}
	moves := planMoves([]group{{keeper: other, losers: []*file{photo}}}, []group{{keeper: other, losers: []*file{photo}}}, nil)
	if len(moves) != 1 || moves[0].to != "_Quarantine/duplicates/photo.jpg" {
		t.Errorf("moves %+v", moves)
	}
}

func TestTurnedOrRecolouredIsNotTheSamePicture(t *testing.T) {
	root := resolved(t)
	photo := jpegOf(scene(300, 200, 4), 90)
	write(t, root, "a.jpg", string(photo))
	write(t, root, "tagged.jpg", string(withSegment(photo, 0xFE, []byte("caption"))))
	write(t, root, "turned.jpg", string(withSegment(photo, 0xE1, exif(6, nil))))
	write(t, root, "profile.jpg", string(withSegment(photo, 0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), make([]byte, 64)...))))
	p := planFor(t, root, options{SameImage: true})
	if got, want := kept(p.groups), [][]string{{"tagged.jpg", "a.jpg"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
}
