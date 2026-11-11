package files

import (
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// audioTag is what Music shows instead of guessing from the file name.
type audioTag struct {
	Title  string
	Artist string
	Album  string
	Track  int
}

func (t audioTag) merge(u audioTag) audioTag {
	if t.Title == "" {
		t.Title = u.Title
	}
	if t.Artist == "" {
		t.Artist = u.Artist
	}
	if t.Album == "" {
		t.Album = u.Album
	}
	if t.Track == 0 {
		t.Track = u.Track
	}
	return t
}

func readAudioTags(r io.ReadSeeker, size int64) audioTag {
	var t audioTag
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return t
	}
	t = t.merge(readID3v2(r))
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return t
	}
	t = t.merge(readFLAC(r))
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return t
	}
	t = t.merge(readMP4(r, size))
	if t.Title == "" && t.Artist == "" && t.Album == "" {
		t = t.merge(readID3v1(r, size))
	}
	t.Title = strings.TrimSpace(t.Title)
	t.Artist = strings.TrimSpace(t.Artist)
	t.Album = strings.TrimSpace(t.Album)
	return t
}

func readID3v2(r io.ReadSeeker) audioTag {
	var hdr [10]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || string(hdr[:3]) != "ID3" {
		return audioTag{}
	}
	ver := hdr[3]
	if (ver != 3 && ver != 4) || hdr[5]&0x80 != 0 {
		return audioTag{}
	}
	remain := syncsafe(hdr[6:10])
	var t audioTag
	for remain >= 10 {
		var fh [10]byte
		if _, err := io.ReadFull(r, fh[:]); err != nil {
			break
		}
		remain -= 10
		if fh[0] == 0 {
			break
		}
		var fsize int
		if ver == 4 {
			fsize = syncsafe(fh[4:8])
		} else {
			fsize = int(binary.BigEndian.Uint32(fh[4:8]))
		}
		if fsize < 0 || fsize > remain {
			break
		}
		id := string(fh[:4])
		if (id == "TIT2" || id == "TPE1" || id == "TALB" || id == "TRCK") && fsize > 0 && fsize < 4096 {
			buf := make([]byte, fsize)
			if _, err := io.ReadFull(r, buf); err != nil {
				break
			}
			text := decodeID3Text(buf)
			switch id {
			case "TIT2":
				t.Title = text
			case "TPE1":
				t.Artist = text
			case "TALB":
				t.Album = text
			case "TRCK":
				t.Track = trackNum(text)
			}
		} else if _, err := r.Seek(int64(fsize), io.SeekCurrent); err != nil {
			break
		}
		remain -= fsize
	}
	return t
}

func readID3v1(r io.ReadSeeker, size int64) audioTag {
	if size < 128 {
		return audioTag{}
	}
	if _, err := r.Seek(size-128, io.SeekStart); err != nil {
		return audioTag{}
	}
	buf := make([]byte, 128)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf[:3]) != "TAG" {
		return audioTag{}
	}
	t := audioTag{
		Title:  trimPad(buf[3:33]),
		Artist: trimPad(buf[33:63]),
		Album:  trimPad(buf[63:93]),
	}
	if buf[125] == 0 && buf[126] != 0 {
		t.Track = int(buf[126])
	}
	return t
}

func readFLAC(r io.ReadSeeker) audioTag {
	var mag [4]byte
	if _, err := io.ReadFull(r, mag[:]); err != nil || string(mag[:]) != "fLaC" {
		return audioTag{}
	}
	for range 16 {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return audioTag{}
		}
		last := hdr[0]&0x80 != 0
		sz := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if hdr[0]&0x7f == 4 {
			if sz < 0 || sz > 1<<20 {
				return audioTag{}
			}
			buf := make([]byte, sz)
			if _, err := io.ReadFull(r, buf); err != nil {
				return audioTag{}
			}
			return parseVorbis(buf)
		}
		if _, err := r.Seek(int64(sz), io.SeekCurrent); err != nil {
			return audioTag{}
		}
		if last {
			break
		}
	}
	return audioTag{}
}

func parseVorbis(b []byte) audioTag {
	if len(b) < 4 {
		return audioTag{}
	}
	n := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	if n < 0 || n > len(b) {
		return audioTag{}
	}
	b = b[n:]
	if len(b) < 4 {
		return audioTag{}
	}
	count := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	var t audioTag
	for range count {
		if len(b) < 4 {
			break
		}
		ln := int(binary.LittleEndian.Uint32(b))
		b = b[4:]
		if ln < 0 || ln > len(b) {
			break
		}
		k, v, ok := strings.Cut(string(b[:ln]), "=")
		b = b[ln:]
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case "TITLE":
			t.Title = v
		case "ARTIST":
			t.Artist = v
		case "ALBUM":
			t.Album = v
		case "TRACKNUMBER":
			t.Track = trackNum(v)
		}
	}
	return t
}

func readMP4(r io.ReadSeeker, size int64) audioTag {
	if size < 16 {
		return audioTag{}
	}
	return walkMP4(r, 0, size, 0)
}

func walkMP4(r io.ReadSeeker, start, end int64, depth int) audioTag {
	var t audioTag
	if depth > 6 {
		return t
	}
	for start+8 <= end {
		if _, err := r.Seek(start, io.SeekStart); err != nil {
			return t
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return t
		}
		sz := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:])
		hdrLen := int64(8)
		if sz == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return t
			}
			sz = int64(binary.BigEndian.Uint64(ext[:]))
			hdrLen = 16
		}
		if sz < hdrLen || start+sz > end {
			break
		}
		childStart := start + hdrLen
		childEnd := start + sz
		switch typ {
		case "moov", "udta", "ilst":
			t = t.merge(walkMP4(r, childStart, childEnd, depth+1))
		case "meta":
			if childStart+4 < childEnd {
				t = t.merge(walkMP4(r, childStart+4, childEnd, depth+1))
			}
		case "\xa9nam":
			t.Title = mp4Text(r, childStart, childEnd)
		case "\xa9ART":
			t.Artist = mp4Text(r, childStart, childEnd)
		case "\xa9alb":
			t.Album = mp4Text(r, childStart, childEnd)
		case "trkn":
			t.Track = mp4Track(r, childStart, childEnd)
		}
		start += sz
	}
	return t
}

func mp4Text(r io.ReadSeeker, start, end int64) string {
	payload := mp4Data(r, start, end)
	return strings.TrimSpace(string(payload))
}

func mp4Track(r io.ReadSeeker, start, end int64) int {
	payload := mp4Data(r, start, end)
	if len(payload) < 4 {
		return 0
	}
	return int(binary.BigEndian.Uint16(payload[2:4]))
}

// mp4Data returns the payload of the first "data" child atom.
func mp4Data(r io.ReadSeeker, start, end int64) []byte {
	for start+8 <= end {
		if _, err := r.Seek(start, io.SeekStart); err != nil {
			return nil
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil
		}
		sz := int64(binary.BigEndian.Uint32(hdr[:4]))
		if sz < 8 || start+sz > end {
			return nil
		}
		if string(hdr[4:]) == "data" && sz >= 16 {
			buf := make([]byte, sz-16)
			if _, err := r.Seek(start+16, io.SeekStart); err != nil {
				return nil
			}
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil
			}
			return buf
		}
		start += sz
	}
	return nil
}

func syncsafe(b []byte) int {
	n := 0
	for _, c := range b {
		n = n<<7 | int(c&0x7f)
	}
	return n
}

func decodeID3Text(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc, rest := b[0], b[1:]
	switch enc {
	case 0:
		return trimPad(rest)
	case 1:
		return trimPad([]byte(decodeUTF16(rest, true)))
	case 2:
		return trimPad([]byte(decodeUTF16(rest, false)))
	default:
		return trimPad(rest)
	}
}

func decodeUTF16(b []byte, bom bool) string {
	if bom {
		if len(b) < 2 {
			return ""
		}
		if b[0] == 0xFF && b[1] == 0xFE {
			b = b[2:]
			return utf16LE(b)
		}
		if b[0] == 0xFE && b[1] == 0xFF {
			b = b[2:]
			return utf16BE(b)
		}
	}
	return utf16BE(b)
}

func utf16BE(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

func utf16LE(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

func trimPad(b []byte) string {
	return strings.Trim(string(b), "\x00 ")
}

func trackNum(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "/"); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 9999 {
		return 0
	}
	return n
}
