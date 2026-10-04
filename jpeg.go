package main

import (
	"encoding/binary"
	"errors"
	"io"
)

// jpegInfo is what reading the structure of a JPEG tells.
type jpegInfo struct {
	picture       []section // the bytes that make up the picture: all but its tags
	pictureSize   int64
	tags          int64 // bytes of tags: comments and APPn segments, but for APP2 and Adobe's APP14
	width, height int
	orientation   int     // from EXIF: how to turn the picture upright, 1 when it is
	thumb         section // the EXIF thumbnail, a small JPEG of its own, if any
}

// section is a stretch of a file.
type section struct{ off, n int64 }

var errNotJPEG = errors.New("not a JPEG")

// isJPEG reports whether a file is a JPEG, by its extension.
func isJPEG(f *file) bool {
	switch extension(f.rel) {
	case "jpg", "jpeg", "jpe", "jfif":
		return true
	}
	return false
}

// readJPEG reads the structure of a JPEG of size bytes, up to the scan that
// starts its picture data. It never reads more than the segments' headers,
// and the EXIF segment.
func readJPEG(r io.ReaderAt, size int64) (jpegInfo, error) {
	info := jpegInfo{orientation: 1}
	var b [5]byte
	at := func(off int64, n int) ([]byte, bool) {
		if off < 0 || off+int64(n) > size {
			return nil, false
		}
		_, err := r.ReadAt(b[:n], off)
		return b[:n], err == nil
	}
	keep := func(off, n int64) {
		if k := len(info.picture); k > 0 && info.picture[k-1].off+info.picture[k-1].n == off {
			info.picture[k-1].n += n
		} else {
			info.picture = append(info.picture, section{off, n})
		}
		info.pictureSize += n
	}

	if head, ok := at(0, 2); !ok || head[0] != 0xFF || head[1] != 0xD8 {
		return info, errNotJPEG
	}
	keep(0, 2)
	for off := int64(2); ; {
		// A marker: 0xFF, any number of 0xFF fill bytes, then its code.
		code := byte(0xFF)
		end := off
		for code == 0xFF {
			c, ok := at(end, 1)
			if !ok || (end == off && c[0] != 0xFF) {
				return info, errNotJPEG
			}
			if end > off {
				code = c[0]
			}
			end++
		}
		switch {
		case code == 0xDA: // start of scan: the picture data, to the end of the file
			keep(off, size-off)
			return info, nil
		case code == 0xD9: // end of image, before any scan
			keep(off, size-off)
			return info, nil
		case code == 0x01 || 0xD0 <= code && code <= 0xD7: // markers without a length
			keep(off, end-off)
			off = end
			continue
		}
		l, ok := at(end, 2)
		if !ok {
			return info, errNotJPEG
		}
		n := int64(binary.BigEndian.Uint16(l))
		if n < 2 || end+n > size {
			return info, errNotJPEG
		}
		next := end + n
		// APP2 holds colour profiles and extra pictures, and APP14 how colours
		// are coded: those change how the picture looks, so they belong to it.
		if 0xE0 <= code && code <= 0xEF && code != 0xE2 && code != 0xEE || code == 0xFE {
			info.tags += next - off
			if code == 0xE1 && info.thumb.n == 0 && info.orientation == 1 {
				info.readExif(r, end+2, n-2)
			}
		} else {
			keep(off, next-off)
			if isSOF(code) && n >= 7 {
				if d, ok := at(end+3, 4); ok {
					info.height = int(binary.BigEndian.Uint16(d[0:2]))
					info.width = int(binary.BigEndian.Uint16(d[2:4]))
				}
			}
		}
		off = next
	}
}

// isSOF reports whether a marker starts a frame, which gives the picture's size.
func isSOF(code byte) bool {
	return 0xC0 <= code && code <= 0xCF && code != 0xC4 && code != 0xC8 && code != 0xCC
}

// readExif reads the orientation and the thumbnail from an APP1 segment of n
// bytes at off, if it holds EXIF: "Exif\0\0", then a TIFF structure whose
// first directory has the orientation, and its second the thumbnail.
func (info *jpegInfo) readExif(r io.ReaderAt, off, n int64) {
	if n < 14 || n > 1<<16 {
		return
	}
	seg := make([]byte, n)
	if _, err := r.ReadAt(seg, off); err != nil || string(seg[:6]) != "Exif\x00\x00" {
		return
	}
	tiff := seg[6:]
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return
	}
	// entries reads a directory: its entries by tag, with their values, and
	// where the next directory starts.
	entries := func(at uint32) (map[uint16]uint32, uint32) {
		if int64(at)+2 > int64(len(tiff)) {
			return nil, 0
		}
		count := int64(order.Uint16(tiff[at:]))
		start := int64(at) + 2
		if start+count*12+4 > int64(len(tiff)) {
			return nil, 0
		}
		values := map[uint16]uint32{}
		for i := int64(0); i < count; i++ {
			e := tiff[start+i*12:]
			tag, kind := order.Uint16(e[0:]), order.Uint16(e[2:])
			switch kind {
			case 3: // SHORT
				values[tag] = uint32(order.Uint16(e[8:]))
			case 4: // LONG
				values[tag] = order.Uint32(e[8:])
			}
		}
		return values, order.Uint32(tiff[start+count*12:])
	}
	ifd0, next := entries(order.Uint32(tiff[4:]))
	if o := ifd0[0x0112]; 1 <= o && o <= 8 {
		info.orientation = int(o)
	}
	if next == 0 {
		return
	}
	ifd1, _ := entries(next)
	start, length := int64(ifd1[0x0201]), int64(ifd1[0x0202])
	if start > 0 && length > 0 && start+length <= int64(len(tiff)) {
		info.thumb = section{off + 6 + start, length}
	}
}

// pictureReader reads a JPEG's picture: its bytes without its tags.
type pictureReader struct {
	r    io.ReaderAt
	secs []section
}

func (p pictureReader) ReadAt(b []byte, off int64) (int, error) {
	n := 0
	for _, s := range p.secs {
		if n == len(b) {
			break
		}
		if off >= s.n {
			off -= s.n
			continue
		}
		want := min(int64(len(b)-n), s.n-off)
		m, err := p.r.ReadAt(b[n:n+int(want)], s.off+off)
		n += m
		if err != nil && !(err == io.EOF && int64(m) == want) {
			return n, err
		}
		off = 0
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}
