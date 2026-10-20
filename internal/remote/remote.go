// Package remote runs a real Chrome on the server for the Browser app. Chrome starts
// when a browser window connects, streams the active tab as JPEG frames over a
// WebSocket, takes mouse and keyboard input back, and stops after a quiet period.
// Its profile (cookies, logins) and open tabs persist on the server, so they follow
// the user to every device.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"ultraspace/internal/sysmem"

	"ultraspace/internal/auth"
	"ultraspace/internal/httpx"
)

type Options struct {
	ChromePath string
	DataDir    string
	FilesRoot  string
	Idle       time.Duration
	NoSandbox  bool
	Quality    int
	// Profile is "auto" (from the host's RAM), "tiny", "small" or "normal".
	Profile string
	// AudioInput replaces ffmpeg's PulseAudio input (tests use a generated tone).
	AudioInput []string
}

type Manager struct {
	opts      Options
	prof      sysmem.Profile
	log       *slog.Logger
	userAgent string
	major     string
	pulseBin  string // sound needs both; without them the browser is silent
	ffmpegBin string

	mu        sync.Mutex
	instances map[int64]*instance
	starting  map[int64]chan struct{}
}

func New(opts Options, log *slog.Logger) *Manager {
	prof := sysmem.Pick(opts.Profile)
	if opts.Idle <= 0 {
		opts.Idle = time.Duration(prof.BrowserIdle) * time.Minute
	}
	if opts.Quality <= 0 || opts.Quality > 100 {
		opts.Quality = 70
	}
	m := &Manager{opts: opts, prof: prof, log: log, instances: map[int64]*instance{}, starting: map[int64]chan struct{}{}}
	m.major = m.chromeMajor()
	m.pulseBin, _ = exec.LookPath("pulseaudio")
	m.ffmpegBin, _ = exec.LookPath("ffmpeg")
	m.userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + m.major + ".0.0.0 Safari/537.36"
	return m
}

var versionRE = regexp.MustCompile(`\b(\d+)\.\d+\.\d+\.\d+\b`)

// chromeMajor reads the installed version, so the user agent matches the real engine
// (the headless build would otherwise announce itself as "HeadlessChrome").
func (m *Manager) chromeMajor() string {
	if !m.Available() {
		return "140"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.opts.ChromePath, "--version").Output()
	if mm := versionRE.FindStringSubmatch(string(out)); err == nil && mm != nil {
		return mm[1]
	}
	return "140"
}

func (m *Manager) Available() bool {
	st, err := os.Stat(m.opts.ChromePath)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// get returns the user's running browser, starting one if needed.
func (m *Manager) get(userID int64) (*instance, error) {
	for {
		m.mu.Lock()
		if in := m.instances[userID]; in != nil {
			m.mu.Unlock()
			return in, nil
		}
		if wait, ok := m.starting[userID]; ok {
			m.mu.Unlock()
			<-wait
			continue
		}
		// Chrome needs room to start; failing clearly beats pushing the host into swap.
		if mem := sysmem.Read(); mem.Total > 0 && mem.Available < uint64(m.prof.MinFreeMB)<<20 {
			m.mu.Unlock()
			return nil, fmt.Errorf("only %d MB of memory is free on the server; close VS Code or other programs and try again", mem.Available>>20)
		}
		done := make(chan struct{})
		m.starting[userID] = done
		m.mu.Unlock()

		in, err := m.launch(userID)
		m.mu.Lock()
		delete(m.starting, userID)
		if err == nil {
			m.instances[userID] = in
		}
		m.mu.Unlock()
		close(done)
		return in, err
	}
}

func (m *Manager) forget(in *instance) {
	m.mu.Lock()
	if m.instances[in.userID] == in {
		delete(m.instances, in.userID)
	}
	m.mu.Unlock()
}

// Running reports whether the user's browser is up.
func (m *Manager) Running(userID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[userID] != nil
}

// Shutdown stops every browser; called when the server exits.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	list := make([]*instance, 0, len(m.instances))
	for _, in := range m.instances {
		list = append(list, in)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, in := range list {
		wg.Add(1)
		go func() { defer wg.Done(); in.Stop() }()
	}
	wg.Wait()
}

func (m *Manager) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /api/remote/status", require(http.HandlerFunc(m.handleStatus)))
	mux.Handle("GET /api/remote/ws", require(http.HandlerFunc(m.handleWS)))
	mux.Handle("POST /api/remote/stop", require(http.HandlerFunc(m.handleStop)))
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"available": m.Available(), "running": m.Running(u.ID)})
}

func (m *Manager) handleStop(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m.mu.Lock()
	in := m.instances[u.ID]
	m.mu.Unlock()
	if in != nil {
		in.Stop()
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Manager) handleWS(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if !m.Available() {
		httpx.Error(w, http.StatusServiceUnavailable, "the remote browser is not installed on this server")
		return
	}
	// Accept checks that the Origin matches the Host, which blocks cross-site sockets.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := newClient(ws)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go c.writeLoop(ctx)

	c.sendJSON(map[string]any{"t": "status", "state": "starting"})
	in, err := m.get(u.ID)
	if err != nil {
		m.log.Error("start remote browser", "err", err)
		c.sendJSON(map[string]any{"t": "error", "message": "The browser could not start: " + err.Error()})
		time.Sleep(200 * time.Millisecond)
		ws.Close(websocket.StatusInternalError, "start failed")
		return
	}
	in.addClient(c)
	defer in.removeClient(c)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		in.input(c, data)
	}
}

// ---------- input from a client ----------

type inMsg struct {
	T          string  `json:"t"`
	Type       string  `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	DX         float64 `json:"dx"`
	DY         float64 `json:"dy"`
	Button     string  `json:"button"`
	Buttons    int     `json:"buttons"`
	ClickCount int     `json:"clickCount"`
	Mods       int     `json:"mods"`
	Key        string  `json:"key"`
	Code       string  `json:"code"`
	KeyCode    int     `json:"keyCode"`
	Text       string  `json:"text"`
	Location   int     `json:"location"`
	Repeat     bool    `json:"repeat"`
	URL        string  `json:"url"`
	ID         string  `json:"id"`
	W          int     `json:"w"`
	H          int     `json:"h"`
	DPR        float64 `json:"dpr"`
	Accept     bool    `json:"accept"`
	Session    string  `json:"session"`
	On         bool    `json:"on"`
}

var mouseTypes = map[string]bool{"mousePressed": true, "mouseReleased": true, "mouseMoved": true}
var keyTypes = map[string]bool{"keyDown": true, "keyUp": true, "rawKeyDown": true, "char": true}
var buttons = map[string]bool{"none": true, "left": true, "middle": true, "right": true, "back": true, "forward": true}

func (in *instance) input(c *client, data []byte) {
	var msg inMsg
	if json.Unmarshal(data, &msg) != nil {
		return
	}
	sess := in.activeSession()
	switch msg.T {
	case "mouse":
		if sess == "" || !mouseTypes[msg.Type] || !buttons[msg.Button] {
			return
		}
		in.conn.Send(sess, "Input.dispatchMouseEvent", map[string]any{
			"type": msg.Type, "x": msg.X, "y": msg.Y, "button": msg.Button, "buttons": msg.Buttons,
			"clickCount": msg.ClickCount, "modifiers": msg.Mods & 15,
		})
	case "wheel":
		if sess == "" {
			return
		}
		in.conn.Send(sess, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": msg.X, "y": msg.Y, "deltaX": msg.DX, "deltaY": msg.DY, "modifiers": msg.Mods & 15,
		})
	case "key":
		if sess == "" || !keyTypes[msg.Type] || len(msg.Key) > 32 || len(msg.Text) > 8 {
			return
		}
		p := map[string]any{
			"type": msg.Type, "modifiers": msg.Mods & 15, "key": msg.Key, "code": msg.Code,
			"windowsVirtualKeyCode": msg.KeyCode, "nativeVirtualKeyCode": msg.KeyCode,
			"autoRepeat": msg.Repeat, "location": msg.Location,
		}
		if msg.Text != "" {
			p["text"], p["unmodifiedText"] = msg.Text, msg.Text
		}
		in.conn.Send(sess, "Input.dispatchKeyEvent", p)
	case "text":
		if sess == "" || len(msg.Text) > 100_000 {
			return
		}
		in.conn.Send(sess, "Input.insertText", map[string]string{"text": msg.Text})
	case "navigate":
		u := strings.TrimSpace(msg.URL)
		if sess == "" || !(allowedURL(u) || u == "about:blank") {
			return
		}
		in.conn.Send(sess, "Page.navigate", map[string]string{"url": u})
	case "back", "forward":
		if sess != "" {
			go in.history(sess, msg.T == "back")
		}
	case "reload":
		if sess != "" {
			in.conn.Send(sess, "Page.reload", map[string]any{})
		}
	case "stop":
		if sess != "" {
			in.conn.Send(sess, "Page.stopLoading", nil)
		}
	case "newtab":
		u := strings.TrimSpace(msg.URL)
		if !(allowedURL(u) || u == "") {
			return
		}
		if u == "" {
			u = "about:blank"
		}
		go in.newTab(u)
	case "close":
		go in.closeTab(msg.ID)
	case "activate":
		go in.activate(msg.ID)
	case "viewport":
		go in.setViewport(viewport{W: msg.W, H: msg.H, DPR: msg.DPR})
	case "copy":
		go func() {
			text, err := in.selectionText()
			if err == nil && text != "" {
				c.sendJSON(map[string]any{"t": "copy", "text": text})
			}
		}()
	case "audio":
		in.audio.setListening(c, msg.On)
	case "dialog":
		in.conn.Send(msg.Session, "Page.handleJavaScriptDialog", map[string]any{"accept": msg.Accept, "promptText": msg.Text})
	}
}

func (in *instance) history(sess string, back bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if in.conn.Call(ctx, sess, "Page.getNavigationHistory", nil, &h) != nil {
		return
	}
	i := h.CurrentIndex + 1
	if back {
		i = h.CurrentIndex - 1
	}
	if i < 0 || i >= len(h.Entries) {
		return
	}
	in.conn.Send(sess, "Page.navigateToHistoryEntry", map[string]int{"entryId": h.Entries[i].ID})
}

// ---------- one connected window ----------

type client struct {
	ws     *websocket.Conn
	text   chan []byte
	frame  chan []byte // holds at most the newest frame
	audio  chan []byte
	closed chan struct{}
	once   sync.Once
}

func newClient(ws *websocket.Conn) *client {
	return &client{ws: ws, text: make(chan []byte, 128), frame: make(chan []byte, 1), audio: make(chan []byte, 64), closed: make(chan struct{})}
}

func (c *client) sendText(b []byte) {
	select {
	case c.text <- b:
	case <-c.closed:
	default: // a client this far behind will resync from the next tabs message
	}
}

func (c *client) sendJSON(v any) {
	if b, err := json.Marshal(v); err == nil {
		c.sendText(b)
	}
}

// sendFrame replaces any frame the client has not taken yet: slow links get fewer
// frames instead of growing latency.
func (c *client) sendFrame(img []byte) {
	for {
		select {
		case c.frame <- img:
			return
		default:
		}
		select {
		case <-c.frame:
		default:
		}
	}
}

// sendAudio queues sound. Init segments must arrive (wait briefly); a fragment for a
// client that has fallen behind is dropped, which costs a tiny gap, not latency.
func (c *client) sendAudio(b []byte, must bool) {
	if must {
		select {
		case c.audio <- b:
		case <-c.closed:
		case <-time.After(2 * time.Second):
		}
		return
	}
	select {
	case c.audio <- b:
	default:
	}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.closed)
		c.ws.Close(websocket.StatusGoingAway, "browser stopped")
	})
}

func (c *client) writeLoop(ctx context.Context) {
	defer c.close()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case b := <-c.text:
			err = c.write(ctx, websocket.MessageText, b)
		case img := <-c.frame:
			err = c.write(ctx, websocket.MessageBinary, img)
		case b := <-c.audio:
			err = c.write(ctx, websocket.MessageBinary, b)
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err = c.ws.Ping(pctx)
			cancel()
		}
		if err != nil {
			return
		}
	}
}

func (c *client) write(ctx context.Context, typ websocket.MessageType, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.ws.Write(wctx, typ, b)
}
