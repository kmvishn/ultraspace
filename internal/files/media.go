package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/httpx"
	"ultraspace/internal/mediasrc"
)

// The Video Player plays anything ffmpeg can read. For each file it picks the
// cheapest way that the visitor's browser can play:
//
//	direct     the file as it is (MP4/WebM with browser codecs), with native seeking
//	remux      the same video and audio in fragmented MP4 (e.g. MKV): almost free
//	remux+aac  video copied, audio converted (AC3, DTS, …): cheap
//	transcode  video converted to H.264 (AVI/Xvid, WMV, MPEG-2, HEVC elsewhere): heavy
//
// ffmpeg reads the file through a descriptor opened via os.Root, so a symlink in
// the library can never make it read anything outside.

var (
	ffmpegBin, _  = exec.LookPath("ffmpeg")
	ffprobeBin, _ = exec.LookPath("ffprobe")
)

var videoExtsAll = []string{".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".wmv", ".flv", ".mpg", ".mpeg", ".ts", ".m2ts", ".mts", ".3gp", ".ogv", ".vob", ".divx", ".asf"}

var textSubCodecs = map[string]bool{"subrip": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true}

// one conversion at a time per user: starting another (a seek, a new file) stops the old one
var streams = struct {
	sync.Mutex
	cancel map[string]context.CancelFunc
}{cancel: map[string]context.CancelFunc{}}

func (f *Files) registerMedia(handle func(string, http.HandlerFunc)) {
	handle("GET /api/files/probe", f.handleProbe)
	handle("GET /api/files/stream", f.handleStream)
	handle("GET /api/files/subtitle", f.handleSubtitle)
}

type probeStream struct {
	Index     int               `json:"index"`
	Type      string            `json:"codec_type"`
	Codec     string            `json:"codec_name"`
	Tag       string            `json:"codec_tag_string"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Channels  int               `json:"channels"`
	PixFmt    string            `json:"pix_fmt"`
	Tags      map[string]string `json:"tags"`
	Dispo     map[string]int    `json:"disposition"`
	FrameRate string            `json:"avg_frame_rate"`
}

type probeOut struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// input is what ffmpeg reads: a library file, passed as descriptor 3, or a media source
// (a torrent file, an addon stream) behind a loopback URL.
type input struct {
	name    string
	rel     string // library path; empty for a source
	file    *os.File
	url     string
	release func()
	src     *mediasrc.Source
}

// command returns ffmpeg/ffprobe with INPUT in args replaced by this input.
func (in *input) command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	args = slices.Clone(args)
	for i, a := range args {
		if a == "INPUT" {
			if in.file != nil {
				args[i] = "/proc/self/fd/3"
			} else {
				args[i] = in.url
			}
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if in.file != nil {
		cmd.ExtraFiles = []*os.File{in.file}
	}
	return cmd
}

func (in *input) Close() {
	if in.file != nil {
		in.file.Close()
	}
	if in.release != nil {
		in.release()
	}
}

// openLibrary opens a file in Files through os.Root, so a symlink can never lead outside.
func (f *Files) openLibrary(rel string) (*input, error) {
	file, err := f.root.Open(rel)
	if err != nil {
		return nil, err
	}
	if st, err := file.Stat(); err != nil || st.IsDir() {
		file.Close()
		return nil, errBadPath
	}
	return &input{name: path.Base(rel), rel: rel, file: file}, nil
}

// openInput opens ?path= (a file in Files) or ?src= (a media source).
func (f *Files) openInput(ctx context.Context, q url.Values) (*input, error) {
	if src := q.Get("src"); src != "" {
		if f.sources == nil {
			return nil, mediasrc.ErrUnknown
		}
		s, err := f.sources.Resolve(ctx, src)
		if err != nil {
			return nil, err
		}
		u, release, err := f.sources.Input(s)
		if err != nil {
			return nil, err
		}
		return &input{name: s.Name, url: u, release: release, src: s}, nil
	}
	rel, err := rel(q.Get("path"))
	if err != nil || rel == "." {
		return nil, errBadPath
	}
	return f.openLibrary(rel)
}

func (f *Files) failInput(w http.ResponseWriter, q url.Values, err error) {
	if q.Get("src") != "" {
		mediasrc.Fail(w, err)
		return
	}
	f.fail(w, err)
}

func (f *Files) probe(ctx context.Context, in *input) (*probeOut, error) {
	if ffprobeBin == "" {
		return nil, errors.New("ffprobe is not installed")
	}
	limit := 20 * time.Second
	if in.src != nil && in.src.Slow {
		limit = 2 * time.Minute // a torrent may need to find peers first
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := in.command(ctx, ffprobeBin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", "INPUT").Output()
	if err != nil {
		if ctx.Err() != nil && in.src != nil {
			return nil, fmt.Errorf("the stream did not start in time (no peers or a slow source)")
		}
		return nil, fmt.Errorf("this file could not be read as a video")
	}
	var p probeOut
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type caps struct{ hevc, av1 bool }

func parseCaps(s string) caps {
	var c caps
	for _, x := range strings.Split(s, ",") {
		switch x {
		case "hevc":
			c.hevc = true
		case "av1":
			c.av1 = true
		}
	}
	return c
}

func (c caps) video(codec string) bool {
	switch codec {
	case "h264", "vp8", "vp9":
		return true
	case "hevc":
		return c.hevc
	case "av1":
		return c.av1
	}
	return false
}

// audio codecs browsers decode, and which of them may go into MP4 unchanged.
var browserAudio = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
var mp4Audio = map[string]bool{"aac": true, "mp3": true, "opus": true, "flac": true}

type plan struct {
	Mode       string `json:"mode"` // direct, remux, transcode
	CopyAudio  bool   `json:"-"`
	Reason     string `json:"reason"`
	Video      string `json:"video_codec"`
	Audio      string `json:"audio_codec"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	TagHVC1    bool   `json:"-"`
	audioIndex int
}

func decide(p *probeOut, c caps, audioN int) plan {
	var v *probeStream
	var audios []*probeStream
	for i := range p.Streams {
		s := &p.Streams[i]
		switch s.Type {
		case "video":
			if v == nil && s.Dispo["attached_pic"] == 0 {
				v = s
			}
		case "audio":
			audios = append(audios, s)
		}
	}
	pl := plan{audioIndex: audioN}
	var a *probeStream
	if audioN >= 0 && audioN < len(audios) {
		a = audios[audioN]
	}
	if a != nil {
		pl.Audio = a.Codec
	}
	if v == nil {
		pl.Mode, pl.Reason = "transcode", "audio only"
		pl.CopyAudio = a != nil && mp4Audio[a.Codec]
		return pl
	}
	pl.Video, pl.Width, pl.Height = v.Codec, v.Width, v.Height
	mp4 := strings.Contains(p.Format.Name, "mp4") || strings.Contains(p.Format.Name, "mov")
	webm := p.Format.Name == "matroska,webm" && (v.Codec == "vp8" || v.Codec == "vp9" || v.Codec == "av1") && (a == nil || a.Codec == "opus" || a.Codec == "vorbis")
	videoOK := c.video(v.Codec) && (v.PixFmt == "" || v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p" || v.PixFmt == "yuv420p10le" && v.Codec != "h264")
	audioOK := a == nil || browserAudio[a.Codec]
	switch {
	case videoOK && audioOK && audioN == 0 && (webm || mp4 && !(v.Codec == "hevc" && v.Tag == "hev1")):
		pl.Mode, pl.Reason = "direct", "plays as it is"
	case videoOK:
		pl.Mode = "remux"
		pl.CopyAudio = a == nil || mp4Audio[a.Codec]
		pl.TagHVC1 = v.Codec == "hevc"
		if pl.CopyAudio {
			pl.Reason = "repackaged for the browser"
		} else {
			pl.Reason = "audio converted (" + a.Codec + ")"
		}
	default:
		pl.Mode, pl.Reason = "transcode", "video converted ("+v.Codec+")"
		pl.CopyAudio = a == nil || mp4Audio[a.Codec] && a.Codec != "flac"
	}
	return pl
}

type subtitle struct {
	Kind  string `json:"kind"` // embedded, file (next to the video) or src (a media source)
	Index int    `json:"index"`
	Path  string `json:"path,omitempty"`
	Src   string `json:"src,omitempty"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
}

type audioTrack struct {
	Lang     string `json:"lang"`
	Title    string `json:"title"`
	Codec    string `json:"codec"`
	Channels int    `json:"channels"`
	Default  bool   `json:"default"`
}

func (f *Files) handleProbe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in, err := f.openInput(r.Context(), q)
	if err != nil {
		f.failInput(w, q, err)
		return
	}
	defer in.Close()
	if ffprobeBin == "" || ffmpegBin == "" {
		// Without ffmpeg the player can still try the file directly.
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"plan": plan{Mode: "direct", Reason: "ffmpeg is not installed on the server"}, "ffmpeg": false, "name": in.name})
		return
	}
	p, err := f.probe(r.Context(), in)
	if err != nil {
		httpx.Error(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	audioN, _ := strconv.Atoi(r.URL.Query().Get("audio"))
	pl := decide(p, parseCaps(r.URL.Query().Get("caps")), audioN)
	dur, _ := strconv.ParseFloat(p.Format.Duration, 64)

	audios := []audioTrack{}
	subs := []subtitle{}
	n := 0
	for _, s := range p.Streams {
		switch s.Type {
		case "audio":
			audios = append(audios, audioTrack{Lang: s.Tags["language"], Title: s.Tags["title"], Codec: s.Codec, Channels: s.Channels, Default: s.Dispo["default"] == 1})
		case "subtitle":
			if textSubCodecs[s.Codec] {
				subs = append(subs, subtitle{Kind: "embedded", Index: n, Lang: s.Tags["language"], Title: s.Tags["title"]})
			}
			n++
		}
	}
	if in.src != nil {
		for _, s := range in.src.Subtitles {
			subs = append(subs, subtitle{Kind: "src", Src: s.Src, Lang: s.Lang, Title: s.Title})
		}
	}
	// Subtitle files next to the video: "Movie.srt", "Movie.en.srt", "Movie.English.vtt"…
	rel := in.rel
	dir, base := path.Dir(rel), strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	if entries, err := fs.ReadDir(f.root.FS(), dir); rel != "" && err == nil {
		for _, e := range entries {
			name := e.Name()
			ext := strings.ToLower(path.Ext(name))
			if e.IsDir() || (ext != ".srt" && ext != ".vtt" && ext != ".ass" && ext != ".ssa") || !strings.HasPrefix(name, base) {
				continue
			}
			lang := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(name, base), path.Ext(name)), ". _-")
			subs = append(subs, subtitle{Kind: "file", Path: "/" + path.Join(dir, name), Lang: lang, Title: name})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"plan": pl, "duration": dur, "container": p.Format.Name, "audio": audios, "subtitles": subs, "ffmpeg": true, "name": in.name,
	})
}

// handleStream sends the file as fragmented MP4, converted as needed, starting at t seconds.
func (f *Files) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ffmpegBin == "" {
		httpx.Error(w, http.StatusNotImplemented, "ffmpeg is not installed on the server")
		return
	}
	in, err := f.openInput(r.Context(), q)
	if err != nil {
		f.failInput(w, q, err)
		return
	}
	defer in.Close()
	p, err := f.probe(r.Context(), in)
	if err != nil {
		httpx.Error(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	audioN, _ := strconv.Atoi(q.Get("audio"))
	pl := decide(p, parseCaps(q.Get("caps")), audioN)
	if q.Get("mode") == "transcode" {
		pl.Mode = "transcode" // the user picked a quality
	}
	start, _ := strconv.ParseFloat(q.Get("t"), 64)
	height, _ := strconv.Atoi(q.Get("h"))
	if !slices.Contains([]int{360, 480, 720, 1080}, height) {
		height = 720
	}

	// A newer stream for the same user (seek, other file) replaces this one.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	key := r.RemoteAddr
	if u := r.Header.Get("Cookie"); u != "" {
		key = u
	}
	streams.Lock()
	if old := streams.cancel[key]; old != nil {
		old()
	}
	streams.cancel[key] = cancel
	streams.Unlock()
	defer func() {
		streams.Lock()
		if streams.cancel[key] != nil {
			delete(streams.cancel, key)
		}
		streams.Unlock()
	}()

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	args = append(args, "-i", "INPUT", "-map", "0:v:0?", "-map", fmt.Sprintf("0:a:%d?", max(audioN, 0)), "-sn", "-dn")
	if pl.Mode == "transcode" {
		args = append(args, "-vf", fmt.Sprintf("scale=-2:'min(%d,ih)':flags=fast_bilinear,format=yuv420p", height),
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-crf", "26", "-g", "48", "-threads", "2")
	} else {
		args = append(args, "-c:v", "copy")
		if pl.TagHVC1 {
			args = append(args, "-tag:v", "hvc1")
		}
	}
	if pl.CopyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
	}
	args = append(args, "-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-frag_duration", "1000000", "pipe:1")

	cmd := in.command(ctx, ffmpegBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Stream-Mode", pl.Mode)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		f.log.Warn("video stream", "name", in.name, "mode", pl.Mode, "err", err, "ffmpeg", strings.TrimSpace(stderr.String()))
	}
}

// handleSubtitle returns a subtitle track (embedded stream or a file) as WebVTT.
func (f *Files) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ffmpegBin == "" {
		httpx.Error(w, http.StatusNotImplemented, "ffmpeg is not installed on the server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	in, err := f.openInput(ctx, q)
	if err != nil {
		f.failInput(w, q, err)
		return
	}
	defer in.Close()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-sub_charenc", "UTF-8", "-i", "INPUT"}
	if s := q.Get("stream"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "bad stream")
			return
		}
		args = []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", "INPUT", "-map", fmt.Sprintf("0:s:%d", n)}
	}
	args = append(args, "-f", "webvtt", "pipe:1")
	out, err := in.command(ctx, ffmpegBin, args...).Output()
	if err != nil && q.Get("stream") == "" {
		// Not UTF-8: let ffmpeg guess the encoding instead.
		out, err = in.command(ctx, ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", "INPUT", "-f", "webvtt", "pipe:1").Output()
	}
	if err != nil {
		httpx.Error(w, http.StatusUnsupportedMediaType, "could not read these subtitles")
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(out)
}

// videoThumb grabs a frame a few seconds in.
func (f *Files) videoThumb(ctx context.Context, rel string, size int) ([]byte, error) {
	if ffmpegBin == "" {
		return nil, errors.New("no ffmpeg")
	}
	in, err := f.openLibrary(rel)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	for _, at := range []string{"5", "0"} {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := in.command(cctx, ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error",
			"-ss", at, "-i", "INPUT", "-frames:v", "1",
			"-vf", fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", size, size), "-f", "image2", "-c:v", "mjpeg", "-q:v", "5", "pipe:1").Output()
		cancel()
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("no frame")
}
