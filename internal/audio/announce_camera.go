package audio

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"ultraspace/internal/httpx"
)

// Camera is a video device on this machine, such as a USB webcam.
type Camera struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var videoID = regexp.MustCompile(`^video[0-9]+$`)

func keepCamera(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" || strings.Contains(low, "metadata") {
		return false
	}
	return true
}

func listCameras() []Camera {
	entries, err := os.ReadDir("/sys/class/video4linux")
	if err != nil {
		return nil
	}
	var cams []Camera
	for _, entry := range entries {
		id := entry.Name()
		if !videoID.MatchString(id) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/sys/class/video4linux", id, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(raw))
		if !keepCamera(name) {
			continue
		}
		cams = append(cams, Camera{ID: id, Name: name})
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].ID < cams[j].ID })
	return cams
}

func knownCamera(id string) (Camera, error) {
	id = strings.TrimSpace(id)
	if !videoID.MatchString(id) {
		return Camera{}, errNoCamera
	}
	for _, cam := range listCameras() {
		if cam.ID == id {
			return cam, nil
		}
	}
	return Camera{}, errNoCamera
}

var errNoCamera = errString("choose a camera that is connected")

type errString string

func (e errString) Error() string { return string(e) }

// handleAnnounceCapabilities says whether this machine has anything Intercom
// can use: a speaker, a microphone or a camera. A VPS normally has none.
func (a *Audio) handleAnnounceCapabilities(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	speakers := 0
	if st, err := a.GetStatus(ctx); err == nil && st != nil {
		speakers = len(st.Sinks)
	}
	mics := 0
	if list, err := a.Mics(ctx); err == nil {
		mics = len(list)
	}
	cameras := len(listCameras())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"speakers": speakers, "microphones": mics, "cameras": cameras,
		"available": speakers+mics+cameras > 0,
	})
}

func (a *Audio) handleAnnounceCameras(w http.ResponseWriter, r *http.Request) {
	cams := listCameras()
	if cams == nil {
		cams = []Camera{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"cameras": cams})
}

// handleAnnounceCamera streams one webcam as a series of JPEG frames.
// Listening and announcements keep their own locks, so the picture stays up.
func (a *Audio) handleAnnounceCamera(w http.ResponseWriter, r *http.Request) {
	cam, err := knownCamera(r.URL.Query().Get("device"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		httpx.Error(w, http.StatusBadRequest, "the camera needs ffmpeg")
		return
	}
	if !a.camera.TryLock() {
		httpx.Error(w, http.StatusConflict, "someone is already watching the camera")
		return
	}
	defer a.camera.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-f", "v4l2", "-i", "/dev/"+cam.ID,
		"-an",
		"-vf", "scale=960:-2", "-r", "15",
		"-c:v", "mjpeg", "-q:v", "8",
		"-f", "mpjpeg",
		"pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not open the camera")
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		a.log.Warn("camera", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "could not open the camera")
		return
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	buf := make([]byte, 32*1024)
	n, readErr := stdout.Read(buf)
	if n == 0 {
		a.log.Warn("camera", "err", readErr, "out", strings.TrimSpace(stderr.String()))
		httpx.Error(w, http.StatusBadGateway, "could not open the camera")
		return
	}
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=ffmpeg")
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
