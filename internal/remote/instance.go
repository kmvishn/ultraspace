package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tab is one page of the remote browser, as shown in the tab strip.
type Tab struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Loading bool   `json:"loading"`
	CanBack bool   `json:"can_back"`
	CanFwd  bool   `json:"can_fwd"`

	session string
}

type viewport struct {
	W   int     `json:"w"`
	H   int     `json:"h"`
	DPR float64 `json:"dpr"`
}

// instance is one running Chrome, owned by one ultraspace user.
type instance struct {
	m      *Manager
	userID int64
	dir    string // profile and saved tabs live here

	cmd   *exec.Cmd
	conn  *cdp
	proxy *egressProxy
	log   *slog.Logger

	mu        sync.Mutex
	tabs      map[string]*Tab
	order     []string
	active    string
	casting   string // session currently screencasting
	vp        viewport
	clients   map[*client]struct{}
	idle      *time.Timer
	dialogs   map[string]string // session -> dialog message currently open
	downloads map[string]string // guid -> file name
	stopped   chan struct{}
	stopOnce  sync.Once
}

type savedTabs struct {
	URLs   []string `json:"urls"`
	Active int      `json:"active"`
}

func (m *Manager) launch(userID int64) (*instance, error) {
	dir := filepath.Join(m.opts.DataDir, "browser", fmt.Sprint(userID))
	profile := filepath.Join(dir, "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	downloads := filepath.Join(m.opts.FilesRoot, "Downloads")
	if err := os.MkdirAll(downloads, 0o750); err != nil {
		return nil, err
	}
	proxy, err := startProxy(m.log)
	if err != nil {
		return nil, err
	}

	// Pipes: Chrome reads commands on fd 3 and writes replies on fd 4.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		proxy.Close()
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		proxy.Close()
		cmdR.Close()
		cmdW.Close()
		return nil, err
	}
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + profile,
		"--proxy-server=http://" + proxy.Addr(),
		"--proxy-bypass-list=<-loopback>", // send localhost through the proxy too, which refuses it
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--user-agent=" + m.userAgent,
		"--no-first-run", "--no-default-browser-check", "--password-store=basic", "--use-mock-keychain",
		"--disable-gpu", "--disable-dev-shm-usage", "--disable-extensions", "--disable-component-update",
		"--disable-background-networking", "--disable-sync", "--disable-default-apps", "--disable-breakpad",
		"--disable-features=Translate,MediaRouter,OptimizationHints,DialMediaRouteProvider,AutofillServerCommunication",
		"--renderer-process-limit=4", "--mute-audio", "--lang=en-US",
		"--window-size=1280,800",
	}
	if m.opts.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(m.opts.ChromePath, args...)
	cmd.ExtraFiles = []*os.File{cmdR, outW}
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
	cmd.Dir = dir
	// Own process group, so stopping kills Chrome's helpers too. systemd also kills
	// them if ultraspace itself goes away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logw := &lineLogger{log: m.log}
	cmd.Stdout, cmd.Stderr = logw, logw
	if err := cmd.Start(); err != nil {
		proxy.Close()
		cmdR.Close()
		cmdW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	cmdR.Close()
	outW.Close()

	in := &instance{
		m: m, userID: userID, dir: dir, cmd: cmd, conn: newCDP(outR, cmdW), proxy: proxy, log: m.log,
		tabs: map[string]*Tab{}, clients: map[*client]struct{}{}, vp: viewport{W: 1280, H: 800, DPR: 1},
		dialogs: map[string]string{}, downloads: map[string]string{}, stopped: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		m.log.Info("remote browser exited", "user", userID, "err", err)
		cmdW.Close()
		outR.Close()
		in.shutdown()
	}()
	go in.eventLoop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := in.conn.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		in.Stop()
		return nil, fmt.Errorf("chrome did not start: %w", err)
	}
	_ = in.conn.Call(ctx, "", "Browser.setDownloadBehavior", map[string]any{
		"behavior": "allow", "downloadPath": downloads, "eventsEnabled": true,
	}, nil)
	in.restoreTabs(ctx)
	m.log.Info("remote browser started", "user", userID, "pid", cmd.Process.Pid)
	return in, nil
}

// restoreTabs reopens the tabs saved when Chrome last stopped.
func (in *instance) restoreTabs(ctx context.Context) {
	var saved savedTabs
	b, err := os.ReadFile(filepath.Join(in.dir, "tabs.json"))
	if err != nil || json.Unmarshal(b, &saved) != nil || len(saved.URLs) == 0 {
		return
	}
	// Wait briefly for the initial about:blank tab, then reuse it for the first URL.
	var first string
	for i := 0; i < 50 && first == ""; i++ {
		in.mu.Lock()
		if len(in.order) > 0 {
			first = in.order[0]
		}
		in.mu.Unlock()
		if first == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	var ids []string
	for i, u := range saved.URLs {
		if !allowedURL(u) {
			continue
		}
		if i == 0 && first != "" {
			ids = append(ids, first)
			in.mu.Lock()
			t := in.tabs[first]
			in.mu.Unlock()
			if t != nil {
				go in.navigateTab(first, u)
			}
			continue
		}
		var res struct {
			TargetID string `json:"targetId"`
		}
		if err := in.conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &res); err == nil {
			ids = append(ids, res.TargetID)
			go in.navigateTab(res.TargetID, u)
		}
	}
	if saved.Active >= 0 && saved.Active < len(ids) {
		in.activate(ids[saved.Active])
	}
}

func (in *instance) navigateTab(id, u string) {
	for i := 0; i < 50; i++ {
		in.mu.Lock()
		t := in.tabs[id]
		sess := ""
		if t != nil {
			sess = t.session
		}
		in.mu.Unlock()
		if sess != "" {
			in.conn.Send(sess, "Page.navigate", map[string]string{"url": u})
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (in *instance) saveTabs() {
	in.mu.Lock()
	st := savedTabs{Active: 0}
	for i, id := range in.order {
		t := in.tabs[id]
		if t == nil || !allowedURL(t.URL) {
			continue
		}
		if id == in.active {
			st.Active = len(st.URLs)
		}
		_ = i
		st.URLs = append(st.URLs, t.URL)
	}
	in.mu.Unlock()
	b, _ := json.Marshal(st)
	tmp := filepath.Join(in.dir, "tabs.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(in.dir, "tabs.json"))
	}
}

func allowedURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// Stop closes Chrome gracefully (so cookies are flushed), then kills it.
func (in *instance) Stop() {
	in.saveTabs()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = in.conn.Call(ctx, "", "Browser.close", nil, nil)
	cancel()
	select {
	case <-in.stopped:
	case <-time.After(5 * time.Second):
		if in.cmd.Process != nil {
			_ = syscall.Kill(-in.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	in.shutdown()
}

func (in *instance) shutdown() {
	in.stopOnce.Do(func() {
		in.proxy.Close()
		if in.cmd.Process != nil {
			_ = syscall.Kill(-in.cmd.Process.Pid, syscall.SIGKILL)
		}
		in.mu.Lock()
		if in.idle != nil {
			in.idle.Stop()
		}
		clients := make([]*client, 0, len(in.clients))
		for c := range in.clients {
			clients = append(clients, c)
		}
		in.mu.Unlock()
		for _, c := range clients {
			c.sendJSON(map[string]any{"t": "status", "state": "stopped"})
			c.close()
		}
		in.m.forget(in)
		close(in.stopped)
	})
}

// ---------- events ----------

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId"`
}

func (in *instance) eventLoop() {
	for ev := range in.conn.events {
		in.handle(ev)
	}
}

func (in *instance) handle(ev cdpMessage) {
	switch ev.Method {
	case "Target.targetCreated":
		var p struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		ti := p.TargetInfo
		in.mu.Lock()
		if _, ok := in.tabs[ti.TargetID]; !ok {
			in.tabs[ti.TargetID] = &Tab{ID: ti.TargetID, URL: ti.URL, Title: ti.Title}
			in.order = append(in.order, ti.TargetID)
		}
		makeActive := in.active == "" || ti.OpenerID != ""
		in.mu.Unlock()
		go in.attach(ti.TargetID)
		if makeActive {
			// Never block the event loop on a call: replies arrive through the same pipe.
			go in.activate(ti.TargetID)
		}
		in.broadcastTabs()

	case "Target.targetInfoChanged":
		var p struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		in.mu.Lock()
		if t := in.tabs[p.TargetInfo.TargetID]; t != nil {
			t.URL, t.Title = p.TargetInfo.URL, p.TargetInfo.Title
		}
		in.mu.Unlock()
		in.broadcastTabs()
		go in.saveTabs()

	case "Target.targetDestroyed":
		var p struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		in.mu.Lock()
		_, existed := in.tabs[p.TargetID]
		delete(in.tabs, p.TargetID)
		for i, id := range in.order {
			if id == p.TargetID {
				in.order = append(in.order[:i], in.order[i+1:]...)
				break
			}
		}
		next := ""
		if in.active == p.TargetID {
			in.active, in.casting = "", ""
			if len(in.order) > 0 {
				next = in.order[len(in.order)-1]
			}
		}
		empty := len(in.order) == 0
		in.mu.Unlock()
		if !existed {
			return
		}
		if next != "" {
			go in.activate(next)
		}
		if empty {
			// Never leave the browser without a tab.
			go in.newTab("about:blank")
		}
		in.broadcastTabs()
		go in.saveTabs()

	case "Page.screencastFrame":
		var p struct {
			Data      string `json:"data"`
			SessionID int    `json:"sessionId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		in.conn.Send(ev.SessionID, "Page.screencastFrameAck", map[string]int{"sessionId": p.SessionID})
		in.mu.Lock()
		current := ev.SessionID == in.casting
		in.mu.Unlock()
		if !current {
			return
		}
		img, err := base64.StdEncoding.DecodeString(p.Data)
		if err == nil {
			in.broadcastFrame(img)
		}

	case "Page.frameStartedLoading", "Page.frameStoppedLoading":
		var p struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		in.mu.Lock()
		t := in.tabs[p.FrameID] // the main frame shares the target's id
		if t != nil {
			t.Loading = ev.Method == "Page.frameStartedLoading"
		}
		in.mu.Unlock()
		if t != nil {
			in.broadcastTabs()
			if !t.Loading {
				go in.refreshHistory(t.ID)
			}
		}

	case "Page.frameNavigated":
		var p struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if json.Unmarshal(ev.Params, &p) == nil && p.Frame.ParentID == "" {
			go in.refreshHistory(p.Frame.ID)
		}

	case "Page.javascriptDialogOpening":
		var p struct {
			Message       string `json:"message"`
			Type          string `json:"type"`
			DefaultPrompt string `json:"defaultPrompt"`
			URL           string `json:"url"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		in.mu.Lock()
		in.dialogs[ev.SessionID] = p.Message
		n := len(in.clients)
		in.mu.Unlock()
		if n == 0 {
			in.conn.Send(ev.SessionID, "Page.handleJavaScriptDialog", map[string]any{"accept": p.Type == "beforeunload"})
			return
		}
		in.broadcastJSON(map[string]any{"t": "dialog", "session": ev.SessionID, "type": p.Type, "message": p.Message, "default": p.DefaultPrompt, "url": p.URL})

	case "Page.javascriptDialogClosed":
		in.mu.Lock()
		delete(in.dialogs, ev.SessionID)
		in.mu.Unlock()
		in.broadcastJSON(map[string]any{"t": "dialog-closed", "session": ev.SessionID})

	case "Browser.downloadWillBegin":
		var p struct {
			GUID              string `json:"guid"`
			SuggestedFilename string `json:"suggestedFilename"`
		}
		if json.Unmarshal(ev.Params, &p) == nil {
			in.mu.Lock()
			in.downloads[p.GUID] = p.SuggestedFilename
			in.mu.Unlock()
			in.broadcastJSON(map[string]any{"t": "download", "state": "started", "name": p.SuggestedFilename})
		}

	case "Browser.downloadProgress":
		var p struct {
			GUID  string `json:"guid"`
			State string `json:"state"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.State == "inProgress" {
			return
		}
		in.mu.Lock()
		name := in.downloads[p.GUID]
		delete(in.downloads, p.GUID)
		in.mu.Unlock()
		in.broadcastJSON(map[string]any{"t": "download", "state": p.State, "name": name})
	}
}

func (in *instance) attach(targetID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := in.conn.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &res); err != nil {
		return
	}
	sess := res.SessionID
	_ = in.conn.Call(ctx, sess, "Page.enable", nil, nil)
	in.applyIdentity(ctx, sess)
	in.mu.Lock()
	vp := in.vp
	in.mu.Unlock()
	in.applyViewport(ctx, sess, vp)
	// Only now is the tab usable: navigations wait for the session, so they run with
	// the identity and viewport above already applied.
	in.mu.Lock()
	t := in.tabs[targetID]
	if t != nil {
		t.session = sess
	}
	isActive := in.active == targetID
	in.mu.Unlock()
	if t == nil {
		return
	}
	if isActive {
		in.startCast(targetID)
	}
	in.refreshHistory(targetID)
}

// applyIdentity presents the tab as regular desktop Chrome, including the client-hint
// headers, which would otherwise announce "HeadlessChrome" and get the browser treated as a bot.
func (in *instance) applyIdentity(ctx context.Context, sess string) {
	major := in.m.major
	brands := []map[string]string{
		{"brand": "Google Chrome", "version": major}, {"brand": "Chromium", "version": major}, {"brand": "Not.A/Brand", "version": "99"},
	}
	_ = in.conn.Call(ctx, sess, "Emulation.setUserAgentOverride", map[string]any{
		"userAgent": in.m.userAgent, "acceptLanguage": "en-US,en;q=0.9", "platform": "Linux x86_64",
		"userAgentMetadata": map[string]any{
			"brands": brands, "fullVersionList": brands, "platform": "Linux", "platformVersion": "6.8.0",
			"architecture": "x86", "model": "", "mobile": false, "bitness": "64", "wow64": false,
		},
	}, nil)
}

func (in *instance) applyViewport(ctx context.Context, sess string, vp viewport) {
	_ = in.conn.Call(ctx, sess, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": vp.W, "height": vp.H, "deviceScaleFactor": vp.DPR, "mobile": vp.W < 600,
	}, nil)
	if vp.W < 600 {
		_ = in.conn.Call(ctx, sess, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true, "maxTouchPoints": 5}, nil)
	} else {
		_ = in.conn.Call(ctx, sess, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": false}, nil)
	}
}

// startCast streams the given tab; only one tab streams at a time.
func (in *instance) startCast(targetID string) {
	in.mu.Lock()
	t := in.tabs[targetID]
	if t == nil || t.session == "" || in.active != targetID || len(in.clients) == 0 {
		in.mu.Unlock()
		return
	}
	old := in.casting
	in.casting = t.session
	sess, vp := t.session, in.vp
	in.mu.Unlock()
	// Restarting also forces a fresh frame at the new size; a static page would
	// otherwise keep showing a frame captured mid-resize.
	if old != "" {
		in.conn.Send(old, "Page.stopScreencast", nil)
	}
	in.conn.Send(sess, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": in.m.opts.Quality,
		"maxWidth": int(float64(vp.W) * vp.DPR), "maxHeight": int(float64(vp.H) * vp.DPR), "everyNthFrame": 1,
	})
}

func (in *instance) stopCast() {
	in.mu.Lock()
	old := in.casting
	in.casting = ""
	in.mu.Unlock()
	if old != "" {
		in.conn.Send(old, "Page.stopScreencast", nil)
	}
}

func (in *instance) activate(targetID string) {
	in.mu.Lock()
	t := in.tabs[targetID]
	if t == nil {
		in.mu.Unlock()
		return
	}
	in.active = targetID
	sess, vp := t.session, in.vp
	in.mu.Unlock()
	in.conn.Send("", "Target.activateTarget", map[string]string{"targetId": targetID})
	if sess != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		in.applyViewport(ctx, sess, vp)
		cancel()
		in.startCast(targetID)
		in.refreshHistory(targetID)
	}
	in.broadcastTabs()
}

func (in *instance) refreshHistory(targetID string) {
	in.mu.Lock()
	t := in.tabs[targetID]
	sess := ""
	if t != nil {
		sess = t.session
	}
	in.mu.Unlock()
	if sess == "" {
		return
	}
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
	in.mu.Lock()
	if t := in.tabs[targetID]; t != nil {
		t.CanBack = h.CurrentIndex > 0
		t.CanFwd = h.CurrentIndex < len(h.Entries)-1
	}
	in.mu.Unlock()
	in.broadcastTabs()
}

func (in *instance) newTab(u string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Open blank first and navigate once the tab is set up, so even the first request
	// carries the right identity.
	var res struct {
		TargetID string `json:"targetId"`
	}
	if err := in.conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &res); err == nil {
		in.activate(res.TargetID)
		if u != "about:blank" {
			in.navigateTab(res.TargetID, u)
		}
	}
}

func (in *instance) snapshot() map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	tabs := make([]Tab, 0, len(in.order))
	for _, id := range in.order {
		if t := in.tabs[id]; t != nil {
			tabs = append(tabs, *t)
		}
	}
	return map[string]any{"t": "tabs", "tabs": tabs, "active": in.active, "viewport": in.vp}
}

func (in *instance) broadcastTabs() { in.broadcastJSON(in.snapshot()) }

func (in *instance) broadcastJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for c := range in.clients {
		c.sendText(b)
	}
}

func (in *instance) broadcastFrame(img []byte) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for c := range in.clients {
		c.sendFrame(img)
	}
}

// ---------- clients ----------

func (in *instance) addClient(c *client) {
	in.mu.Lock()
	in.clients[c] = struct{}{}
	if in.idle != nil {
		in.idle.Stop()
		in.idle = nil
	}
	active := in.active
	in.mu.Unlock()
	c.sendJSON(map[string]any{"t": "status", "state": "ready"})
	c.sendJSON(in.snapshot())
	if active != "" {
		in.startCast(active)
	}
}

func (in *instance) removeClient(c *client) {
	in.mu.Lock()
	delete(in.clients, c)
	left := len(in.clients)
	if left == 0 {
		in.idle = time.AfterFunc(in.m.opts.Idle, func() {
			in.mu.Lock()
			still := len(in.clients)
			in.mu.Unlock()
			if still == 0 {
				in.log.Info("remote browser idle; stopping", "user", in.userID)
				in.Stop()
			}
		})
	}
	in.mu.Unlock()
	if left == 0 {
		in.stopCast()
	}
}

func (in *instance) setViewport(vp viewport) {
	if vp.W < 200 || vp.H < 150 || vp.W > 3840 || vp.H > 2160 {
		return
	}
	if vp.DPR < 1 || vp.DPR > 2 {
		vp.DPR = 1
	}
	in.mu.Lock()
	if in.vp == vp {
		in.mu.Unlock()
		return
	}
	in.vp = vp
	active := in.active
	var sess string
	if t := in.tabs[active]; t != nil {
		sess = t.session
	}
	in.mu.Unlock()
	if sess != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		in.applyViewport(ctx, sess, vp)
		cancel()
		in.startCast(active)
	}
	in.broadcastTabs()
}

func (in *instance) activeSession() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if t := in.tabs[in.active]; t != nil {
		return t.session
	}
	return ""
}

// selectionText returns the text selected in the active tab, for copying to the user's clipboard.
func (in *instance) selectionText() (string, error) {
	sess := in.activeSession()
	if sess == "" {
		return "", errors.New("no page")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	const js = `(() => { const a = document.activeElement;
		if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA') && typeof a.selectionStart === 'number')
			return a.value.slice(a.selectionStart, a.selectionEnd);
		return String(getSelection()); })()`
	err := in.conn.Call(ctx, sess, "Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true}, &res)
	return res.Result.Value, err
}

// lineLogger sends Chrome's stderr to the debug log.
type lineLogger struct{ log *slog.Logger }

func (l *lineLogger) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line != "" {
			l.log.Debug("chrome", "out", line)
		}
	}
	return len(p), nil
}
