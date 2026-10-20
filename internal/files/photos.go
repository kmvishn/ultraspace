package files

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // register decoders
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"ultraspace/internal/httpx"
)

// Thumbnails are made one at a time and cached on disk: decoding a 12-megapixel photo
// briefly needs ~20-50 MB, which must never happen several times at once on a small host.
var thumbSlot = make(chan struct{}, 1)

const maxThumbPixels = 50_000_000

var thumbSizes = map[int]bool{256: true, 512: true}

var photoExts = []string{".jpg", ".jpeg", ".png", ".gif", ".webp"}
var videoExts = []string{".mp4", ".webm", ".mov", ".m4v"}

// photoInfo is cached per file and revalidated by modification time and size.
type photoInfo struct {
	mod         time.Time
	size        int64
	taken       time.Time
	w, h        int
	orientation int
}

type photoCache struct {
	mu sync.Mutex
	m  map[string]photoInfo
}

func (f *Files) registerPhotos(handle func(string, http.HandlerFunc)) {
	handle("GET /api/files/thumb", f.handleThumb)
	handle("GET /api/files/photos", f.handlePhotos)
}

type photo struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mod   time.Time `json:"mtime"`
	Taken time.Time `json:"taken"` // EXIF date taken, else the file's time
	W     int       `json:"w,omitempty"`
	H     int       `json:"h,omitempty"`
	Video bool      `json:"video,omitempty"`
}

// handlePhotos lists every photo and video in the library with its date, newest first.
func (f *Files) handlePhotos(w http.ResponseWriter, r *http.Request) {
	out := []photo{}
	seen := map[string]bool{}
	err := fs.WalkDir(f.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(path.Ext(d.Name()))
		isPhoto, isVideo := slices.Contains(photoExts, ext), slices.Contains(videoExts, ext)
		if !isPhoto && !isVideo || len(out) >= 20000 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		ph := photo{Path: "/" + p, Name: d.Name(), Size: info.Size(), Mod: info.ModTime(), Taken: info.ModTime(), Video: isVideo}
		if isPhoto {
			if pi, ok := f.photoInfo(p, info); ok {
				if !pi.taken.IsZero() {
					ph.Taken = pi.taken
				}
				ph.W, ph.H = pi.w, pi.h
				if pi.orientation >= 5 { // rotated 90°: width and height swap on screen
					ph.W, ph.H = pi.h, pi.w
				}
			}
		}
		seen[p] = true
		out = append(out, ph)
		return nil
	})
	if err != nil {
		f.fail(w, err)
		return
	}
	f.photos.mu.Lock()
	for k := range f.photos.m {
		if !seen[k] {
			delete(f.photos.m, k)
		}
	}
	f.photos.mu.Unlock()
	slices.SortFunc(out, func(a, b photo) int { return b.Taken.Compare(a.Taken) })
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"photos": out})
}

// skipDir leaves out hidden folders and dependency trees from library scans.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__"
}

func (f *Files) photoInfo(p string, info fs.FileInfo) (photoInfo, bool) {
	f.photos.mu.Lock()
	pi, ok := f.photos.m[p]
	f.photos.mu.Unlock()
	if ok && pi.mod.Equal(info.ModTime()) && pi.size == info.Size() {
		return pi, true
	}
	file, err := f.root.Open(p)
	if err != nil {
		return photoInfo{}, false
	}
	defer file.Close()
	head := make([]byte, 256<<10)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	pi = photoInfo{mod: info.ModTime(), size: info.Size(), orientation: 1}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		pi.w, pi.h = cfg.Width, cfg.Height
	}
	if ex := readExif(head); ex.ok {
		pi.taken, pi.orientation = ex.taken, ex.orientation
	}
	f.photos.mu.Lock()
	if f.photos.m == nil {
		f.photos.m = map[string]photoInfo{}
	}
	f.photos.m[p] = pi
	f.photos.mu.Unlock()
	return pi, true
}

// handleThumb returns a cached JPEG thumbnail whose longest side is size pixels.
func (f *Files) handleThumb(w http.ResponseWriter, r *http.Request) {
	p, err := rel(r.URL.Query().Get("path"))
	if err != nil || p == "." {
		f.fail(w, errBadPath)
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if !thumbSizes[size] {
		size = 256
	}
	info, err := f.root.Stat(p)
	if err != nil {
		f.fail(w, err)
		return
	}
	if info.IsDir() || !slices.Contains(photoExts, strings.ToLower(path.Ext(p))) {
		httpx.Error(w, http.StatusUnsupportedMediaType, "no thumbnail for this file")
		return
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", p, size, info.ModTime().UnixNano(), info.Size())))
	key := hex.EncodeToString(sum[:12])
	etag := `"` + key + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=604800")
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	cached := filepath.Join(f.thumbDir, key[:2], key+".jpg")
	if b, err := os.ReadFile(cached); err == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(b)
		return
	}
	b, err := f.makeThumb(r, p, info, size)
	if err != nil {
		f.log.Debug("thumbnail", "path", p, "err", err)
		httpx.Error(w, http.StatusUnsupportedMediaType, "cannot make a thumbnail for this image")
		return
	}
	if f.thumbDir != "" && os.MkdirAll(filepath.Dir(cached), 0o750) == nil {
		tmp := cached + ".tmp"
		if os.WriteFile(tmp, b, 0o640) == nil {
			_ = os.Rename(tmp, cached)
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(b)
}

func (f *Files) makeThumb(r *http.Request, p string, info fs.FileInfo, size int) ([]byte, error) {
	select {
	case thumbSlot <- struct{}{}:
		defer func() { <-thumbSlot }()
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	file, err := f.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	br := bufio.NewReaderSize(file, 64<<10)
	head, _ := br.Peek(256 << 10)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > maxThumbPixels {
		return nil, errors.New("image too large")
	}
	orientation := 1
	if ex := readExif(head); ex.ok {
		orientation = ex.orientation
	}
	src, _, err := image.Decode(br)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	scale := float64(size) / float64(max(b.Dx(), b.Dy()))
	if scale > 1 {
		scale = 1
	}
	dw, dh := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src) // transparent PNGs on white
	xdraw.BiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	src = nil
	out := orient(dst, orientation)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > 4_000_000 {
		debug.FreeOSMemory() // hand the decode buffer back right away on small hosts
	}
	return buf.Bytes(), nil
}

// orient applies an EXIF orientation (1-8) so thumbnails appear upright.
func orient(img *image.RGBA, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			dst.SetRGBA(nx, ny, img.RGBAAt(x, y))
		}
	}
	return dst
}

type exifInfo struct {
	ok          bool
	taken       time.Time
	orientation int
}

// readExif finds the date taken and orientation in a JPEG's EXIF block.
func readExif(b []byte) exifInfo {
	res := exifInfo{orientation: 1}
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return res
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return res
		}
		marker := b[i+1]
		seg := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xDA || seg < 2 || i+2+seg > len(b) {
			return res
		}
		data := b[i+4 : i+2+seg]
		if marker == 0xE1 && len(data) > 14 && string(data[:6]) == "Exif\x00\x00" {
			return parseTIFF(data[6:])
		}
		i += 2 + seg
	}
	return res
}

func parseTIFF(t []byte) exifInfo {
	res := exifInfo{orientation: 1}
	if len(t) < 8 {
		return res
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return res
	}
	res.ok = true
	var dateTime, original string
	exifIFD := uint32(0)
	readIFD := func(off uint32, visit func(tag, typ uint16, count, val uint32, raw []byte)) {
		if int(off)+2 > len(t) {
			return
		}
		n := int(bo.Uint16(t[off:]))
		for k := 0; k < n; k++ {
			e := int(off) + 2 + 12*k
			if e+12 > len(t) {
				return
			}
			visit(bo.Uint16(t[e:]), bo.Uint16(t[e+2:]), bo.Uint32(t[e+4:]), bo.Uint32(t[e+8:]), t[e+8:e+12])
		}
	}
	str := func(count, val uint32) string {
		if count < 19 || int(val)+int(count) > len(t) {
			return ""
		}
		return strings.TrimRight(string(t[val:val+count]), "\x00 ")
	}
	readIFD(bo.Uint32(t[4:]), func(tag, typ uint16, count, val uint32, raw []byte) {
		switch tag {
		case 0x0112: // Orientation (SHORT, stored in the first two bytes)
			if o := int(bo.Uint16(raw)); o >= 1 && o <= 8 {
				res.orientation = o
			}
		case 0x0132:
			dateTime = str(count, val)
		case 0x8769:
			exifIFD = val
		}
	})
	if exifIFD > 0 {
		readIFD(exifIFD, func(tag, typ uint16, count, val uint32, raw []byte) {
			if tag == 0x9003 {
				original = str(count, val)
			}
		})
	}
	for _, s := range []string{original, dateTime} {
		if tm, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local); err == nil && tm.Year() > 1970 {
			res.taken = tm
			break
		}
	}
	return res
}
