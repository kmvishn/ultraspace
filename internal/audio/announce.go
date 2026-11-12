package audio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ultraspace/internal/httpx"
)

const (
	announceMaxRunes = 400
	announceMaxSinks = 8
	announceMaxBytes = 4 << 20
)

// AnnounceText speaks text on the chosen speakers. Music pauses for the message
// and then continues. This does not change which speakers music uses.
// Voice is one eSpeak voice the announcement can be spoken in.
type Voice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Language string `json:"language"`
	Gender   string `json:"gender,omitempty"`
}

// Mic is a microphone on this machine, not a speaker monitor.
type Mic struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

const (
	speechRateDefault = 160
	speechRateMin     = 80
	speechRateMax     = 260
)

// speakingRate is words per minute for eSpeak. Zero means the usual pace.
func speakingRate(wpm int) (int, error) {
	if wpm == 0 {
		return speechRateDefault, nil
	}
	if wpm < speechRateMin || wpm > speechRateMax {
		return 0, errors.New("choose a speaking speed")
	}
	return wpm, nil
}

func (a *Audio) AnnounceText(ctx context.Context, text, voice string, sinks []string, wpm int) error {
	text, err := cleanAnnouncement(text)
	if err != nil {
		return err
	}
	wpm, err = speakingRate(wpm)
	if err != nil {
		return err
	}
	bin, err := a.findSpeechBin()
	if err != nil {
		return err
	}
	voice, err = a.checkedVoice(ctx, voice)
	if err != nil {
		return err
	}
	return a.withSpeakers(ctx, sinks, func(ctx context.Context, chosen []string) error {
		f, err := os.CreateTemp("", "ultraspace-announce-*.wav")
		if err != nil {
			return err
		}
		name := f.Name()
		_ = f.Close()
		defer os.Remove(name)
		args := []string{"-w", name, "-s", strconv.Itoa(wpm)}
		if voice != "" {
			args = append(args, "-v", voice)
		}
		args = append(args, "--", text)
		out, err := a.runner.CombinedOutput(ctx, bin, args...)
		if err != nil {
			return fmt.Errorf("could not speak that message (%s)", strings.TrimSpace(string(out)))
		}
		return a.playOnSinks(ctx, name, chosen)
	})
}

// AnnounceFile plays a short recording on the chosen speakers.
func (a *Audio) AnnounceFile(ctx context.Context, path string, sinks []string) error {
	return a.withSpeakers(ctx, sinks, func(ctx context.Context, chosen []string) error {
		return a.playOnSinks(ctx, path, chosen)
	})
}

func (a *Audio) withSpeakers(ctx context.Context, sinks []string, play func(context.Context, []string) error) error {
	chosen, err := a.chosenSinks(ctx, sinks)
	if err != nil {
		return err
	}
	if !a.announce.TryLock() {
		return errors.New("an announcement is already playing")
	}
	defer a.announce.Unlock()
	resume := a.holdMusic(ctx)
	defer resume()
	return play(ctx, chosen)
}

func (a *Audio) holdMusic(ctx context.Context) func() {
	if a.player == nil {
		return func() {}
	}
	st, err := a.player.GetStatus(ctx)
	if err != nil || st == nil || !st.Playing || st.Paused {
		return func() {}
	}
	if err := a.player.Pause(ctx); err != nil {
		a.log.Warn("pause music for announcement", "err", err)
		return func() {}
	}
	return func() {
		if err := a.player.Resume(context.Background()); err != nil {
			a.log.Warn("resume music after announcement", "err", err)
		}
	}
}

func (a *Audio) chosenSinks(ctx context.Context, sinks []string) ([]string, error) {
	st, err := a.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil || !st.Available {
		return nil, errors.New("no speakers are available")
	}
	known := map[string]bool{}
	for _, s := range st.Sinks {
		known[s.Name] = true
	}
	var chosen []string
	seen := map[string]bool{}
	for _, name := range sinks {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if !known[name] || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00 ") {
			return nil, errors.New("choose a speaker that is connected")
		}
		seen[name] = true
		chosen = append(chosen, name)
	}
	if len(chosen) == 0 {
		return nil, errors.New("choose at least one speaker")
	}
	if len(chosen) > announceMaxSinks {
		return nil, errors.New("too many speakers")
	}
	return chosen, nil
}

func (a *Audio) playOnSinks(ctx context.Context, path string, sinks []string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for _, sink := range sinks {
		wg.Add(1)
		go func(sink string) {
			defer wg.Done()
			out, err := a.runner.CombinedOutput(ctx, "mpv", "--no-terminal", "--no-video", "--really-quiet", "--audio-device=pulse/"+sink, path)
			if err != nil {
				a.log.Warn("announcement playback", "sink", sink, "err", err, "out", strings.TrimSpace(string(out)))
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}(sink)
	}
	wg.Wait()
	if failed == len(sinks) {
		return errors.New("could not play on the chosen speakers")
	}
	return nil
}

func (a *Audio) findSpeechBin() (string, error) {
	if a.findSpeech != nil {
		return a.findSpeech()
	}
	for _, name := range []string{"espeak-ng", "espeak"} {
		if path, err := exec.LookPath(name); err == nil && path != "" {
			return name, nil
		}
	}
	return "", errors.New("text announcements need espeak-ng")
}

func cleanAnnouncement(text string) (string, error) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", errors.New("write a message")
	}
	if utf8.RuneCountInString(text) > announceMaxRunes {
		return "", errors.New("message is too long")
	}
	return text, nil
}

func (a *Audio) handleAnnounceText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string   `json:"text"`
		Voice string   `json:"voice"`
		Sinks []string `json:"sinks"`
		Speed int      `json:"speed"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := a.AnnounceText(ctx, req.Text, req.Voice, req.Sinks, req.Speed); err != nil {
		a.log.Warn("text announcement", "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "played"})
}

func (a *Audio) handleAnnounceVoice(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, announceMaxBytes+8192)
	if err := r.ParseMultipartForm(announceMaxBytes); err != nil {
		httpx.Error(w, http.StatusBadRequest, "recording is too large")
		return
	}
	file, hdr, err := r.FormFile("audio")
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "recording is missing")
		return
	}
	defer file.Close()
	ext, err := voiceExt(hdr.Header.Get("Content-Type"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	tmp, err := os.CreateTemp("", "ultraspace-announce-*"+ext)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not store the recording")
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, io.LimitReader(file, announceMaxBytes+1)); err != nil {
		_ = tmp.Close()
		httpx.Error(w, http.StatusBadRequest, "could not read the recording")
		return
	}
	if err := tmp.Close(); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not store the recording")
		return
	}
	info, err := os.Stat(name)
	if err != nil || info.Size() == 0 || info.Size() > announceMaxBytes {
		httpx.Error(w, http.StatusBadRequest, "recording is empty or too large")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	sinks := r.MultipartForm.Value["sink"]
	if err := a.AnnounceFile(ctx, name, sinks); err != nil {
		a.log.Warn("voice announcement", "err", err)
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "played"})
}

func voiceExt(contentType string) (string, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "audio/webm":
		return ".webm", nil
	case "audio/ogg":
		return ".ogg", nil
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a", nil
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav", nil
	case "audio/mpeg", "audio/mp3":
		return ".mp3", nil
	default:
		return "", errors.New("that recording format is not supported")
	}
}

var voiceLine = regexp.MustCompile(`^\s*\d+\s+(\S+)\s+(\S+)\s+(\S+)`)
var variantClone = regexp.MustCompile(`\d+$`)

func humanVoice(name string) string {
	if name == "Half-LifeAnnouncementSystem" {
		return "Announcer"
	}
	if strings.EqualFold(name, "aunty") {
		return "Auntie"
	}
	var b strings.Builder
	prev := rune(0)
	for i, r := range name {
		if r == '_' || r == '-' {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), " ") {
				b.WriteByte(' ')
			}
			prev = r
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' && prev >= 'a' && prev <= 'z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prev = r
	}
	parts := strings.Fields(b.String())
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
	}
	return strings.Join(parts, " ")
}

// spokenVoice keeps a named speaking voice and skips language entries,
// numbered clones, and synthesizer presets.
func spokenVoice(name string) bool {
	low := strings.ToLower(name)
	if strings.Contains(low, "test") {
		return false
	}
	if variantClone.MatchString(name) {
		return false
	}
	switch {
	case strings.HasPrefix(low, "male"), strings.HasPrefix(low, "klatt"), strings.HasPrefix(low, "robosoft"), strings.HasPrefix(low, "ricishay"):
		return false
	case strings.HasPrefix(low, "female") && low != "female_whisper":
		return false
	default:
		return name != ""
	}
}

func variantFilename(line string) string {
	i := strings.Index(line, "!v/")
	if i < 0 {
		return ""
	}
	file := strings.TrimSpace(line[i+len("!v/"):])
	if j := strings.Index(file, "("); j >= 0 {
		file = strings.TrimSpace(file[:j])
	}
	return file
}

func voiceGender(field string) string {
	switch {
	case field == "F" || strings.HasSuffix(field, "/F"):
		return "female"
	case field == "M" || strings.HasSuffix(field, "/M"):
		return "male"
	default:
		return ""
	}
}

func parseVoices(out []byte) []Voice {
	var voices []Voice
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		m := voiceLine.FindStringSubmatch(scanner.Text())
		if m == nil || strings.EqualFold(m[1], "Language") {
			continue
		}
		if !strings.EqualFold(m[1], "variant") || !spokenVoice(m[3]) {
			continue
		}
		file := variantFilename(scanner.Text())
		if file == "" || strings.ContainsAny(file, "\r\n\x00+") {
			continue
		}
		id := "en+" + file
		if seen[id] {
			continue
		}
		seen[id] = true
		voices = append(voices, Voice{ID: id, Name: humanVoice(m[3]), Gender: voiceGender(m[2])})
	}
	sort.Slice(voices, func(i, j int) bool {
		if voices[i].Gender != voices[j].Gender {
			return voices[i].Gender == "female"
		}
		return voices[i].Name < voices[j].Name
	})
	return voices
}

func (a *Audio) Voices(ctx context.Context) ([]Voice, error) {
	bin, err := a.findSpeechBin()
	if err != nil {
		return nil, err
	}
	out, err := a.runner.CombinedOutput(ctx, bin, "--voices=variant")
	if err != nil {
		return nil, errors.New("could not list voices")
	}
	return parseVoices(out), nil
}

func (a *Audio) checkedVoice(ctx context.Context, voice string) (string, error) {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return "", nil
	}
	voices, err := a.Voices(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range voices {
		if v.ID == voice {
			return voice, nil
		}
	}
	return "", errors.New("choose a voice")
}

func parseMics(out []byte) []Mic {
	var mics []Mic
	var name, desc string
	flush := func() {
		if name == "" || strings.HasSuffix(name, ".monitor") || strings.Contains(strings.ToLower(desc), "monitor of") {
			name, desc = "", ""
			return
		}
		if desc == "" {
			desc = name
		}
		mics = append(mics, Mic{Name: name, Description: desc})
		name, desc = "", ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Source #") {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Name":
			name = strings.TrimSpace(v)
		case "Description":
			desc = strings.TrimSpace(v)
		}
	}
	flush()
	return mics
}

func (a *Audio) Mics(ctx context.Context) ([]Mic, error) {
	out, err := a.runner.CombinedOutput(ctx, "pactl", "list", "sources")
	if err != nil {
		return nil, errors.New("could not list microphones")
	}
	return parseMics(out), nil
}

func (a *Audio) knownMic(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00 ") {
		return errors.New("choose a microphone")
	}
	mics, err := a.Mics(ctx)
	if err != nil {
		return err
	}
	for _, m := range mics {
		if m.Name == name {
			return nil
		}
	}
	return errors.New("choose a microphone that is connected")
}

func (a *Audio) handleAnnounceVoices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	voices, err := a.Voices(ctx)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"voices": []Voice{}, "error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"voices": voices})
}

func (a *Audio) handleAnnounceMics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	mics, err := a.Mics(ctx)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"mics": []Mic{}, "error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mics": mics})
}

// handleAnnounceListen streams the chosen server microphone to the browser.
func (a *Audio) handleAnnounceListen(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if err := a.knownMic(r.Context(), source); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		httpx.Error(w, http.StatusBadRequest, "talk back needs ffmpeg")
		return
	}
	if !a.listen.TryLock() {
		httpx.Error(w, http.StatusConflict, "someone is already listening")
		return
	}
	defer a.listen.Unlock()

	cmd := exec.CommandContext(r.Context(), "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-f", "pulse", "-i", source,
		"-ac", "1", "-ar", "16000",
		"-f", "webm", "-c:a", "libopus", "-b:a", "32k",
		"-flush_packets", "1",
		"pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not open the microphone")
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		a.log.Warn("talk back", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "could not open the microphone")
		return
	}
	defer func() { _ = cmd.Wait() }()

	buf := make([]byte, 4096)
	n, readErr := stdout.Read(buf)
	if n == 0 {
		a.log.Warn("talk back", "err", readErr, "out", strings.TrimSpace(stderr.String()))
		httpx.Error(w, http.StatusBadGateway, "could not open the microphone")
		return
	}
	w.Header().Set("Content-Type", "audio/webm")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	if _, err := w.Write(buf[:n]); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
