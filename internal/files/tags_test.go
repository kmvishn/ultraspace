package files

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestID3v2Tags(t *testing.T) {
	payload := id3Text("TIT2", "Night Drive") + id3Text("TPE1", "Ada") + id3Text("TALB", "Cities") + id3Text("TRCK", "4/10")
	body := id3Header(payload) + payload
	got := readAudioTags(bytes.NewReader([]byte(body)), int64(len(body)))
	if got.Title != "Night Drive" || got.Artist != "Ada" || got.Album != "Cities" || got.Track != 4 {
		t.Fatalf("id3v2 = %+v", got)
	}
}

func TestFLACTags(t *testing.T) {
	comment := vorbisComment("Night Drive", "Ada", "Cities", "2")
	var block []byte
	block = append(block, 4|0x80, byte(len(comment)>>16), byte(len(comment)>>8), byte(len(comment)))
	block = append(block, comment...)
	body := append([]byte("fLaC"), block...)
	got := readAudioTags(bytes.NewReader(body), int64(len(body)))
	if got.Title != "Night Drive" || got.Artist != "Ada" || got.Album != "Cities" || got.Track != 2 {
		t.Fatalf("flac = %+v", got)
	}
}

func TestMP4Tags(t *testing.T) {
	item := func(typ, text string) []byte {
		data := mp4Atom("data", append([]byte{0, 0, 0, 1, 0, 0, 0, 0}, text...))
		return mp4Atom(typ, data)
	}
	ilst := mp4Atom("ilst", append(append(item("\xa9nam", "Night Drive"), item("\xa9ART", "Ada")...), append(item("\xa9alb", "Cities"), mp4Atom("trkn", mp4Atom("data", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 10}))...)...))
	meta := mp4Atom("meta", append([]byte{0, 0, 0, 0}, ilst...))
	moov := mp4Atom("moov", mp4Atom("udta", meta))
	body := append(mp4Atom("ftyp", []byte("M4A ")), moov...)
	got := readAudioTags(bytes.NewReader(body), int64(len(body)))
	if got.Title != "Night Drive" || got.Artist != "Ada" || got.Album != "Cities" || got.Track != 3 {
		t.Fatalf("mp4 = %+v", got)
	}
}

func id3Header(payload string) string {
	n := len(payload)
	size := []byte{byte(n >> 21), byte(n >> 14), byte(n >> 7), byte(n)}
	return string(append([]byte{'I', 'D', '3', 3, 0, 0}, size...))
}

func id3Text(id, text string) string {
	body := append([]byte{3}, text...)
	sz := make([]byte, 4)
	binary.BigEndian.PutUint32(sz, uint32(len(body)))
	return id + string(sz) + "\x00\x00" + string(body)
}

func vorbisComment(title, artist, album, track string) []byte {
	fields := []string{"TITLE=" + title, "ARTIST=" + artist, "ALBUM=" + album, "TRACKNUMBER=" + track}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0))
	binary.Write(&b, binary.LittleEndian, uint32(len(fields)))
	for _, f := range fields {
		binary.Write(&b, binary.LittleEndian, uint32(len(f)))
		b.WriteString(f)
	}
	return b.Bytes()
}

func mp4Atom(typ string, payload []byte) []byte {
	sz := make([]byte, 4)
	binary.BigEndian.PutUint32(sz, uint32(8+len(payload)))
	return append(append(sz, typ...), payload...)
}
