package main

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // for image.Decode
	"image/jpeg"
	_ "image/png" // for image.Decode
	"io"
	"math/bits"
	"os"
	"slices"
	"sort"
	"sync/atomic"
)

// A photo's look is a 64-bit difference hash. The photo is shrunk to 32×32 grey
// pixels, turned upright, and shrunk again to 9×8, and each bit says whether
// a pixel is darker than the one to its right. Photos that look alike, such as
// copies at another size or quality, have looks that differ in few bits.
const greySide = 32

// canLook reports whether photo_cleanup can tell how a photo looks: it reads
// JPEG, PNG and GIF, but not HEIC, WebP or camera raw files.
func canLook(f *file) bool {
	switch extension(f.rel) {
	case "jpg", "jpeg", "jpe", "jfif", "png", "gif":
		return true
	}
	return false
}

// The most pixels a photo, or an EXIF thumbnail, may have to be looked at, so
// that a damaged file claiming to be enormous can't use up the memory.
const (
	maxLookPixels  = 100_000_000
	maxThumbPixels = 4_000_000
)

// lookOf works out how a photo looks, and its size in pixels, upright. A JPEG's
// EXIF thumbnail stands in for the JPEG when it has one, which is much quicker
// to read.
func lookOf(f *file) (look uint64, w, h int, err error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer fh.Close()
	orientation := 1
	var img image.Image
	if isJPEG(f) {
		info, err := readJPEG(fh, f.size)
		if err != nil {
			return 0, 0, 0, err
		}
		orientation, w, h = info.orientation, info.width, info.height
		if info.thumb.n > 0 && w > 0 && h > 0 {
			thumb := func() io.Reader { return io.NewSectionReader(fh, info.thumb.off, info.thumb.n) }
			if c, err := jpeg.DecodeConfig(thumb()); err == nil && c.Width > 0 && c.Height > 0 && int64(c.Width)*int64(c.Height) <= maxThumbPixels {
				if t, err := jpeg.Decode(thumb()); err == nil && !t.Bounds().Empty() {
					img = cropToShape(t, w, h)
				}
			}
		}
	}
	if img == nil {
		whole := func() io.Reader { return bufio.NewReader(io.NewSectionReader(fh, 0, f.size)) }
		c, _, err := image.DecodeConfig(whole())
		switch {
		case err != nil:
			return 0, 0, 0, err
		case c.Width <= 0 || c.Height <= 0:
			return 0, 0, 0, errors.New("it has no pixels")
		case int64(c.Width)*int64(c.Height) > maxLookPixels:
			return 0, 0, 0, fmt.Errorf("at %d×%d it is too big to look at", c.Width, c.Height)
		}
		if img, _, err = image.Decode(whole()); err != nil {
			return 0, 0, 0, err
		}
		if img.Bounds().Empty() {
			return 0, 0, 0, errors.New("it has no pixels")
		}
		b := img.Bounds()
		w, h = b.Dx(), b.Dy()
	}
	if orientation >= 5 { // turned a quarter
		w, h = h, w
	}
	return difference(upright(shrinkGrey(img), orientation)), w, h, nil
}

// cropToShape cuts off the bars that cameras put around a thumbnail to make it
// their own shape, by cutting it to the shape of the photo, w×h, around its
// middle.
func cropToShape(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return img
	}
	want, have := float64(w)/float64(h), float64(b.Dx())/float64(b.Dy())
	r := b
	switch {
	case have > want*1.02: // bars at the sides
		cw := int(float64(b.Dy())*want + 0.5)
		r.Min.X += (b.Dx() - cw) / 2
		r.Max.X = r.Min.X + cw
	case have < want/1.02: // bars at the top and bottom
		ch := int(float64(b.Dx())/want + 0.5)
		r.Min.Y += (b.Dy() - ch) / 2
		r.Max.Y = r.Min.Y + ch
	default:
		return img
	}
	if s, ok := img.(subImager); ok && !r.Empty() {
		return s.SubImage(r)
	}
	return img
}

// subImager is what the kinds of picture that can be cut, such as *image.YCbCr, do.
type subImager interface {
	SubImage(image.Rectangle) image.Image
}

// shrinkGrey shrinks a picture to greySide×greySide grey pixels, each the
// average of up to 8×8 samples of its part of the picture.
func shrinkGrey(img image.Image) []float64 {
	b := img.Bounds()
	luma := lumaOf(img)
	out := make([]float64, greySide*greySide)
	if b.Empty() {
		return out
	}
	for cy := 0; cy < greySide; cy++ {
		y0 := b.Min.Y + cy*b.Dy()/greySide
		y1 := max(b.Min.Y+(cy+1)*b.Dy()/greySide, y0+1)
		for cx := 0; cx < greySide; cx++ {
			x0 := b.Min.X + cx*b.Dx()/greySide
			x1 := max(b.Min.X+(cx+1)*b.Dx()/greySide, x0+1)
			sx, sy := max(1, (x1-x0)/8), max(1, (y1-y0)/8)
			sum, n := 0.0, 0
			for y := y0; y < y1; y += sy {
				for x := x0; x < x1; x += sx {
					sum += luma(x, y)
					n++
				}
			}
			out[cy*greySide+cx] = sum / float64(n)
		}
	}
	return out
}

// lumaOf reads how light the pixels of a picture are, quickly for the kinds of
// picture JPEG and PNG decode to.
func lumaOf(img image.Image) func(x, y int) float64 {
	rgb := func(p []uint8) float64 { return 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2]) }
	switch m := img.(type) {
	case *image.YCbCr:
		return func(x, y int) float64 { return float64(m.Y[m.YOffset(x, y)]) }
	case *image.Gray:
		return func(x, y int) float64 { return float64(m.Pix[m.PixOffset(x, y)]) }
	case *image.RGBA:
		return func(x, y int) float64 { return rgb(m.Pix[m.PixOffset(x, y):]) }
	case *image.NRGBA:
		return func(x, y int) float64 { return rgb(m.Pix[m.PixOffset(x, y):]) }
	}
	return func(x, y int) float64 { return float64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y) }
}

// upright turns a greySide×greySide picture upright, as its EXIF orientation says.
func upright(g []float64, orientation int) []float64 {
	if orientation < 2 || orientation > 8 {
		return g
	}
	const n = greySide
	out := make([]float64, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var sx, sy int
			switch orientation {
			case 2: // mirrored
				sx, sy = n-1-x, y
			case 3: // upside down
				sx, sy = n-1-x, n-1-y
			case 4: // mirrored upside down
				sx, sy = x, n-1-y
			case 5: // mirrored, a quarter to the left
				sx, sy = y, x
			case 6: // a quarter to the left: turn it right
				sx, sy = y, n-1-x
			case 7: // mirrored, a quarter to the right
				sx, sy = n-1-y, n-1-x
			case 8: // a quarter to the right: turn it left
				sx, sy = n-1-y, x
			}
			out[y*n+x] = g[sy*n+sx]
		}
	}
	return out
}

// difference makes the look of an upright greySide×greySide picture.
func difference(g []float64) uint64 {
	const cols, rows = 9, 8
	var cells [rows][cols]float64
	for r := 0; r < rows; r++ {
		y0, y1 := r*greySide/rows, (r+1)*greySide/rows
		for c := 0; c < cols; c++ {
			x0, x1 := c*greySide/cols, (c+1)*greySide/cols
			sum := 0.0
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					sum += g[y*greySide+x]
				}
			}
			cells[r][c] = sum / float64((y1-y0)*(x1-x0))
		}
	}
	var look uint64
	for r := 0; r < rows; r++ {
		for c := 0; c < cols-1; c++ {
			look <<= 1
			if cells[r][c] < cells[r][c+1] {
				look |= 1
			}
		}
	}
	return look
}

// lookAlikes finds photos that look alike, among those no group moves: each
// group's keeper, and the photos in no group. Each photo joins the first set
// whose first photo it looks like, so that sets don't chain into each other.
// In each set the photo with the most pixels is the one to keep, and the
// others are only listed, to move if chosen.
func (ru *rules) lookAlikes(groups []group, media []*file, c *cache, r *reporter) []group {
	losing := map[*file]bool{}
	for _, g := range groups {
		for _, f := range g.losers {
			losing[f] = true
		}
	}
	var photos []*file
	for _, f := range media {
		if canLook(f) && !losing[f] {
			photos = append(photos, f)
		}
	}
	looks := make([]uint64, len(photos))
	known := make([]bool, len(photos))
	var started atomic.Int64
	parallel(photos, ru.jobs, r, func(i int, f *file, _ []byte) {
		r.progress("look", started.Add(1)-1, int64(len(photos)), f.rel)
		look, w, h, hit := c.look(f)
		if !hit {
			var err error
			if look, w, h, err = lookOf(f); err != nil {
				r.warnf("  ! cannot tell how %s looks: %v\n", f.path, err)
				return
			}
			c.putLook(f, look, w, h)
		}
		looks[i], f.w, f.h, known[i] = look, w, h, true
	})
	if r.stopped() {
		return nil
	}
	if len(photos) > 0 {
		r.progress("look", int64(len(photos)), int64(len(photos)), "")
	}

	var firsts []uint64 // the look of each set's first photo
	var sets [][]*file
	for i, f := range photos {
		if !known[i] {
			continue
		}
		k := -1
		for j, first := range firsts {
			if bits.OnesCount64(looks[i]^first) <= ru.looks {
				k = j
				break
			}
		}
		if k < 0 {
			firsts = append(firsts, looks[i])
			sets = append(sets, []*file{f})
		} else {
			sets[k] = append(sets[k], f)
		}
	}

	var alike []group
	for _, set := range sets {
		if len(set) < 2 {
			continue
		}
		sort.SliceStable(set, func(i, j int) bool {
			a, b := set[i], set[j]
			switch {
			case a.w*a.h != b.w*b.h:
				return a.w*a.h > b.w*b.h
			case a.size != b.size:
				return a.size > b.size
			}
			return ru.rank(a).less(ru.rank(b))
		})
		g := group{keeper: set[0], kind: "alike"}
		for _, f := range set[1:] {
			if movable(f) && !os.SameFile(f.info, g.keeper.info) &&
				!slices.ContainsFunc(g.losers, func(l *file) bool { return os.SameFile(l.info, f.info) }) {
				g.losers = append(g.losers, f)
			}
		}
		if len(g.losers) > 0 {
			alike = append(alike, g)
		}
	}
	return alike
}
