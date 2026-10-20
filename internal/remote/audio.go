package remote

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"ultraspace/internal/sysmem"
)

// Sound for the server browser. Chrome plays into a private PulseAudio null sink;
// while at least one window has sound switched on, ffmpeg records the sink's monitor
// and encodes AAC in fragmented MP4, which every browser can play through Media
// Source Extensions. The init segment is kept, so a window that switches sound on
// later starts cleanly at the next fragment.

// Binary WebSocket messages start with one of these bytes.
const (
	msgFrame     = 1 // JPEG screen frame
	msgAudioInit = 2 // fMP4 init segment (ftyp+moov)
	msgAudioFrag = 3 // fMP4 media fragment (moof+mdat)
)

type audio struct {
	m       *Manager
	log     *slog.Logger
	dir     string // pulse runtime dir (short path: Unix sockets allow ~107 bytes)
	pulse   *exec.Cmd
	enabled bool

	mu        sync.Mutex
	ffmpeg    *exec.Cmd
	init      []byte
	listeners map[*client]bool
	stopTimer *time.Timer
}

// startPulse runs a private sound server for this browser. It returns false (and the
// browser stays silent) when PulseAudio or ffmpeg is not installed.
func (m *Manager) startPulse() *audio {
	a := &audio{listeners: map[*client]bool{}, log: m.log, m: m}
	if m.pulseBin == "" || m.ffmpegBin == "" {
		return a
	}
	dir, err := os.MkdirTemp("", "us-pulse-")
	if err != nil {
		return a
	}
	a.dir = dir
	sock := filepath.Join(dir, "native")
	cmd := exec.Command(m.pulseBin,
		"-n", "--daemonize=no", "--exit-idle-time=-1", "--use-pid-file=no", "--disable-shm=yes",
		"--realtime=no", "--high-priority=no", "--log-target=stderr", "--log-level=error",
		"--load=module-null-sink sink_name=us sink_properties=device.description=ultraspace rate=48000",
		"--load=module-native-protocol-unix socket="+sock+" auth-anonymous=1",
	)
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_RUNTIME_DIR="+dir, "PULSE_RUNTIME_PATH="+dir,
		"PULSE_STATE_PATH="+filepath.Join(dir, "state"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = &lineLogger{log: m.log}
	if err := cmd.Start(); err != nil {
		m.log.Warn("sound: pulseaudio did not start", "err", err)
		os.RemoveAll(dir)
		return a
	}
	go func() { _ = cmd.Wait() }()
	for i := 0; i < 40; i++ { // wait for the socket
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			a.pulse, a.enabled = cmd, true
			sysmem.PreferOOMKill(cmd.Process.Pid, 500)
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.log.Warn("sound: pulseaudio socket did not appear")
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	os.RemoveAll(dir)
	return a
}

// chromeEnv points Chrome at the private sound server.
func (a *audio) chromeEnv() []string {
	if !a.enabled {
		return nil
	}
	return []string{"PULSE_SERVER=unix:" + filepath.Join(a.dir, "native"), "PULSE_SINK=us"}
}

func (a *audio) close() {
	a.mu.Lock()
	a.stopLocked()
	a.mu.Unlock()
	if a.pulse != nil && a.pulse.Process != nil {
		_ = syscall.Kill(-a.pulse.Process.Pid, syscall.SIGKILL)
	}
	if a.dir != "" {
		os.RemoveAll(a.dir)
	}
}

// setListening turns sound on or off for one window.
func (a *audio) setListening(c *client, on bool) {
	if !a.enabled {
		if on {
			c.sendJSON(map[string]any{"t": "audio", "available": false})
		}
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if on {
		a.listeners[c] = true
		if a.stopTimer != nil {
			a.stopTimer.Stop()
			a.stopTimer = nil
		}
		if a.ffmpeg == nil {
			a.startLocked()
		} else if a.init != nil {
			c.sendAudio(prefixed(msgAudioInit, a.init), true)
		}
		return
	}
	delete(a.listeners, c)
	if len(a.listeners) == 0 && a.ffmpeg != nil && a.stopTimer == nil {
		// Keep the encoder a moment, so toggling or reconnecting is instant.
		a.stopTimer = time.AfterFunc(10*time.Second, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.listeners) == 0 {
				a.stopLocked()
			}
			a.stopTimer = nil
		})
	}
}

func (a *audio) startLocked() {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if src := a.m.opts.AudioInput; len(src) > 0 {
		args = append(args, src...) // tests feed a generated tone instead of PulseAudio
	} else {
		args = append(args, "-f", "pulse", "-server", "unix:"+filepath.Join(a.dir, "native"), "-fragment_size", "3840", "-i", "us.monitor")
	}
	args = append(args, "-ac", "2", "-ar", "48000", "-c:a", "aac", "-b:a", "128k",
		"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-frag_duration", "200000", "pipe:1")
	cmd := exec.Command(a.m.ffmpegBin, args...)
	cmd.Stderr = &lineLogger{log: a.log}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		a.log.Warn("sound: ffmpeg did not start", "err", err)
		return
	}
	sysmem.PreferOOMKill(cmd.Process.Pid, 500)
	a.ffmpeg, a.init = cmd, nil
	go a.pump(cmd, out)
}

func (a *audio) stopLocked() {
	if a.ffmpeg != nil && a.ffmpeg.Process != nil {
		_ = syscall.Kill(-a.ffmpeg.Process.Pid, syscall.SIGKILL)
	}
	a.ffmpeg, a.init = nil, nil
}

// pump splits ffmpeg's fragmented MP4 into the init segment and media fragments.
func (a *audio) pump(cmd *exec.Cmd, r io.Reader) {
	defer func() { _ = cmd.Wait() }()
	br := bufio.NewReaderSize(r, 64<<10)
	var init, frag []byte
	initDone := false
	for {
		box, typ, err := readBox(br)
		if err != nil {
			return
		}
		switch {
		case !initDone && (typ == "ftyp" || typ == "moov"):
			init = append(init, box...)
			if typ == "moov" {
				initDone = true
				a.mu.Lock()
				if a.ffmpeg != cmd {
					a.mu.Unlock()
					return
				}
				a.init = init
				msg := prefixed(msgAudioInit, init)
				for c := range a.listeners {
					c.sendAudio(msg, true)
				}
				a.mu.Unlock()
			}
		case typ == "moof":
			frag = append(frag[:0], box...)
		case typ == "mdat" && len(frag) > 0:
			msg := prefixed(msgAudioFrag, append(frag, box...))
			frag = frag[:0]
			a.mu.Lock()
			if a.ffmpeg != cmd {
				a.mu.Unlock()
				return
			}
			for c := range a.listeners {
				c.sendAudio(msg, false)
			}
			a.mu.Unlock()
		}
	}
}

func readBox(r *bufio.Reader) ([]byte, string, error) {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, "", err
	}
	size := uint64(binary.BigEndian.Uint32(hdr))
	typ := string(hdr[4:8])
	head := hdr
	if size == 1 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, "", err
		}
		size = binary.BigEndian.Uint64(ext)
		head = append(hdr, ext...)
	}
	if size < uint64(len(head)) || size > 16<<20 {
		return nil, "", errors.New("bad mp4 box")
	}
	box := make([]byte, size)
	copy(box, head)
	if _, err := io.ReadFull(r, box[len(head):]); err != nil {
		return nil, "", fmt.Errorf("short mp4 box: %w", err)
	}
	return box, typ, nil
}

func prefixed(kind byte, b []byte) []byte {
	out := make([]byte, len(b)+1)
	out[0] = kind
	copy(out[1:], b)
	return out
}
