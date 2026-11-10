package audio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type PlayRequest struct {
	Path   string   `json:"path"`
	Src    string   `json:"src"`
	Title  string   `json:"title"`
	Artist string   `json:"artist"`
	Album  string   `json:"album"`
	Seek   float64  `json:"seek"`
	Sinks  []string `json:"sinks,omitempty"`
}

type SeekRequest struct {
	Time float64 `json:"time"`
}

type PlayerStatus struct {
	Playing     bool    `json:"playing"`
	Paused      bool    `json:"paused"`
	CurrentTime float64 `json:"current_time"`
	Duration    float64 `json:"duration"`
	Path        string  `json:"path,omitempty"`
	Src         string  `json:"src,omitempty"`
	Title       string  `json:"title,omitempty"`
	Artist      string  `json:"artist,omitempty"`
	Album       string  `json:"album,omitempty"`
}

// StreamResolver resolves a media source URL into a playable URL.
type StreamResolver func(ctx context.Context, userID int64, src string) (string, error)

// Player manages server-side background audio playback through mpv over IPC.
type Player struct {
	mu             sync.Mutex
	log            *slog.Logger
	filesRoot      string
	socketPath     string
	cmd            *exec.Cmd
	status         PlayerStatus
	streamResolver StreamResolver
	lastPlayTime   time.Time
	audioDevice    string
}

// NewPlayer creates a new server Player.
func NewPlayer(log *slog.Logger, filesRoot string) *Player {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("ultraspace-mpv-%d.sock", os.Getpid()))
	return &Player{
		log:        log,
		filesRoot:  filesRoot,
		socketPath: sock,
	}
}

// SetAudioDevice sets the audio output device (pulse/<sink_name>) for mpv.
func (p *Player) SetAudioDevice(sinkName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	target := "pulse"
	if sinkName != "" {
		target = "pulse/" + sinkName
	}
	p.audioDevice = target
	if p.cmd == nil || !p.ping() {
		return nil
	}
	_, err := p.sendCommand("set_property", "audio-device", target)
	return err
}

// SetStreamResolver assigns a resolver function for streaming providers like Navidrome.
func (p *Player) SetStreamResolver(fn StreamResolver) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamResolver = fn
}

func (p *Player) ping() bool {
	conn, err := net.DialTimeout("unix", p.socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (p *Player) ensureRunning() error {
	if p.cmd != nil && p.cmd.Process != nil {
		if p.ping() {
			return nil
		}
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd = nil
	}
	_ = os.Remove(p.socketPath)

	mpvPath, err := exec.LookPath("mpv")
	if err != nil {
		return errors.New("server audio player (mpv) not installed")
	}

	cmd := exec.Command(mpvPath,
		"--no-video",
		"--idle=yes",
		"--ao=pulse",
		"--audio-client-name=ultraspace",
		fmt.Sprintf("--input-ipc-server=%s", p.socketPath),
	)
	cmd.Env = os.Environ()
	if os.Getenv("PULSE_SERVER") == "" {
		if _, err := os.Stat("/var/run/pulse/native"); err == nil {
			cmd.Env = append(cmd.Env, "PULSE_SERVER=/var/run/pulse/native")
		}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	p.cmd = cmd

	// Wait up to 2 seconds for mpv socket to become available
	start := time.Now()
	for time.Since(start) < 2*time.Second {
		if p.ping() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return errors.New("mpv socket startup timed out")
}

func (p *Player) sendCommand(cmdArgs ...any) (any, error) {
	conn, err := net.DialTimeout("unix", p.socketPath, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	reqPayload := map[string]any{"command": cmdArgs}
	b, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')

	if _, err := conn.Write(b); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		// Skip event notifications, wait for command reply containing "error"
		if bytes.Contains(line, []byte(`"error"`)) {
			var resp struct {
				Data  any    `json:"data"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(line, &resp); err != nil {
				return nil, err
			}
			if resp.Error != "success" && resp.Error != "" {
				return nil, fmt.Errorf("mpv: %s", resp.Error)
			}
			return resp.Data, nil
		}
	}
}

// Play starts playing a track on server hardware.
func (p *Player) Play(ctx context.Context, req PlayRequest, userID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureRunning(); err != nil {
		return err
	}

	target := ""

	// 1. Try local file path on disk
	if req.Path != "" {
		if st, err := os.Stat(req.Path); err == nil && !st.IsDir() {
			target = req.Path
		} else if p.filesRoot != "" {
			clean := filepath.Clean("/" + req.Path)
			candidate := filepath.Join(p.filesRoot, clean)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				target = candidate
			}
		}
	}

	// 2. Try resolving from Src
	if target == "" && req.Src != "" {
		src := strings.TrimSpace(req.Src)
		if strings.Contains(src, "navidrome") && p.streamResolver != nil {
			resolved, err := p.streamResolver(ctx, userID, src)
			if err == nil && resolved != "" {
				target = resolved
			} else {
				p.log.Warn("resolve navidrome stream failed", "src", src, "err", err)
			}
		} else if strings.Contains(src, "/api/files/download") {
			if u, err := url.Parse(src); err == nil {
				filePath := u.Query().Get("path")
				if filePath != "" {
					if st, err := os.Stat(filePath); err == nil && !st.IsDir() {
						target = filePath
					} else if p.filesRoot != "" {
						clean := filepath.Clean("/" + filePath)
						candidate := filepath.Join(p.filesRoot, clean)
						if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
							target = candidate
						}
					}
				}
			}
		} else if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
			target = src
		}

		// Fallback for relative or absolute paths directly on disk
		if target == "" {
			if st, err := os.Stat(src); err == nil && !st.IsDir() {
				target = src
			} else if p.filesRoot != "" {
				clean := filepath.Clean("/" + src)
				candidate := filepath.Join(p.filesRoot, clean)
				if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
					target = candidate
				}
			}
		}
	}

	if target == "" {
		return fmt.Errorf("cannot resolve audio source for: %s", req.Title)
	}

	p.log.Info("player starting playback", "title", req.Title)

	if p.audioDevice != "" {
		_, _ = p.sendCommand("set_property", "audio-device", p.audioDevice)
	}

	if _, err := p.sendCommand("loadfile", target, "replace"); err != nil {
		return fmt.Errorf("loadfile: %w", err)
	}

	if req.Seek > 0 {
		_, _ = p.sendCommand("seek", req.Seek, "absolute")
	}
	_, _ = p.sendCommand("set_property", "pause", false)

	p.lastPlayTime = time.Now()
	p.status = PlayerStatus{
		Playing:     true,
		Paused:      false,
		CurrentTime: req.Seek,
		Path:        req.Path,
		Src:         req.Src,
		Title:       req.Title,
		Artist:      req.Artist,
		Album:       req.Album,
	}
	return nil
}

// Pause pauses active playback.
func (p *Player) Pause(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || !p.ping() {
		p.status.Playing = false
		p.status.Paused = false
		return nil
	}

	_, err := p.sendCommand("set_property", "pause", true)
	if err != nil {
		return err
	}
	p.status.Paused = true
	p.status.Playing = false
	return nil
}

// Resume unpauses playback.
func (p *Player) Resume(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || !p.ping() {
		return errors.New("no media active")
	}

	_, err := p.sendCommand("set_property", "pause", false)
	if err != nil {
		return err
	}
	p.status.Paused = false
	p.status.Playing = true
	return nil
}

// Seek moves playback position.
func (p *Player) Seek(ctx context.Context, seconds float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || !p.ping() {
		return errors.New("no media active")
	}

	_, err := p.sendCommand("seek", seconds, "absolute")
	if err != nil {
		return err
	}
	p.status.CurrentTime = seconds
	return nil
}

// Stop stops playback.
func (p *Player) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || !p.ping() {
		p.status.Playing = false
		p.status.Paused = false
		p.status.CurrentTime = 0
		return nil
	}

	_, _ = p.sendCommand("stop")
	p.status.Playing = false
	p.status.Paused = false
	p.status.CurrentTime = 0
	return nil
}

// GetStatus returns the current playback state and time position.
func (p *Player) GetStatus(ctx context.Context) (*PlayerStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || !p.ping() {
		p.status.Playing = false
		p.status.Paused = false
		cp := p.status
		return &cp, nil
	}

	// Check if mpv is idle (file finished or stopped)
	idleVal, err := p.sendCommand("get_property", "idle-active")
	if err == nil {
		if isIdle, ok := idleVal.(bool); ok && isIdle {
			// If we initiated playback within the last 3.5 seconds, mpv may still be connecting/buffering
			if time.Since(p.lastPlayTime) < 3500*time.Millisecond && p.status.Playing {
				cp := p.status
				return &cp, nil
			}
			p.status.Playing = false
			p.status.Paused = false
			cp := p.status
			return &cp, nil
		}
	}

	// Query pause status
	pauseVal, err := p.sendCommand("get_property", "pause")
	if err == nil {
		if isPaused, ok := pauseVal.(bool); ok {
			p.status.Paused = isPaused
			p.status.Playing = !isPaused
		}
	}

	// Query playback time
	timeVal, err := p.sendCommand("get_property", "time-pos")
	if err == nil {
		if t, ok := timeVal.(float64); ok {
			p.status.CurrentTime = t
		}
	}

	// Query total duration
	durVal, err := p.sendCommand("get_property", "duration")
	if err == nil {
		if d, ok := durVal.(float64); ok {
			p.status.Duration = d
		}
	}

	cp := p.status
	return &cp, nil
}

// Close terminates mpv and removes the socket file.
func (p *Player) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd != nil && p.cmd.Process != nil {
		_, _ = p.sendCommand("quit")
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd = nil
	}
	_ = os.Remove(p.socketPath)
}

