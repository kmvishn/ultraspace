package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"ultraspace/internal/sysmem"
)

// Tab is one page of the remote browser, as shown in the tab strip.
type Tab struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Loading bool   `json:"loading"`
	CanBack bool   `json:"can_back"`
	CanFwd  bool   `json:"can_fwd"`
	// Asleep tabs have been closed to save memory; they reload when opened.
	Asleep bool `json:"asleep"`

	session    string
	lastActive time.Time
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
	audio *audio
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
	waking    string            // sleeping tab currently being reloaded
	closing   bool              // shutting down: the saved tab list is final
	stopped   chan struct{}
	stopOnce  sync.Once
}

type savedTabs struct {
	URLs   []string `json:"urls"`
	Titles []string `json:"titles"`
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
	snd := m.startPulse()

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
		"--disable-gpu", "--disable-software-rasterizer", "--disable-dev-shm-usage", "--disable-extensions", "--disable-component-update",
		"--disable-background-networking", "--disable-sync", "--disable-default-apps", "--disable-breakpad",
		"--disable-features=Translate,MediaRouter,OptimizationHints,DialMediaRouteProvider,AutofillServerCommunication,BackForwardCache",
		fmt.Sprintf("--renderer-process-limit=%d", m.prof.RendererLimit), "--lang=en-US", "--autoplay-policy=no-user-gesture-required",
		"--window-size=1280,800",
	}
	if !snd.enabled {
		args = append(args, "--mute-audio") // no sound server: nothing to play into
	}
	if m.prof.Name == "tiny" {
		// A smaller young generation trades a little speed for memory.
		args = append(args, "--js-flags=--max-semi-space-size=1")
	}
	if m.opts.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(m.opts.ChromePath, args...)
	cmd.ExtraFiles = []*os.File{cmdR, outW}
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
	cmd.Env = append(cmd.Env, snd.chromeEnv()...)
	cmd.Dir = dir
	// Own process group, so stopping kills Chrome's helpers too. systemd also kills
	// them if ultraspace itself goes away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logw := &lineLogger{log: m.log}
	cmd.Stdout, cmd.Stderr = logw, logw
	if err := cmd.Start(); err != nil {
		snd.close()
		proxy.Close()
		cmdR.Close()
		cmdW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	cmdR.Close()
	outW.Close()
	sysmem.PreferOOMKill(cmd.Process.Pid, 500)

	in := &instance{
		m: m, userID: userID, dir: dir, cmd: cmd, conn: newCDP(outR, cmdW), proxy: proxy, audio: snd, log: m.log,
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
	go in.sleepLoop()

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
	m.log.Info("remote browser started", "profile", m.prof.Name, "user", userID, "pid", cmd.Process.Pid)
	return in, nil
}

// restoreTabs reopens the tabs saved when Chrome last stopped. Only the active one
// loads; the others come back asleep and load when opened, so a restart is cheap.
func (in *instance) restoreTabs(ctx context.Context) {
	var saved savedTabs
	b, err := os.ReadFile(filepath.Join(in.dir, "tabs.json"))
	if err != nil || json.Unmarshal(b, &saved) != nil || len(saved.URLs) == 0 {
		return
	}
	// Wait briefly for the initial about:blank tab, then reuse it for the active URL.
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
	if first == "" {
		return
	}
	active := min(max(saved.Active, 0), len(saved.URLs)-1)
	in.mu.Lock()
	order := make([]string, 0, len(saved.URLs))
	for i, u := range saved.URLs {
		if !allowedURL(u) && i != active {
			continue
		}
		if i == active {
			order = append(order, first)
			continue
		}
		title := ""
		if i < len(saved.Titles) {
			title = saved.Titles[i]
		}
		id := fmt.Sprintf("sleep-%d-%d", time.Now().UnixNano(), i)
		in.tabs[id] = &Tab{ID: id, URL: u, Title: title, Asleep: true}
		order = append(order, id)
	}
	in.order = order
	in.mu.Unlock()
	if allowedURL(saved.URLs[active]) {
		go in.navigateTab(first, saved.URLs[active])
	}
	in.broadcastTabs()
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
			// Record where the tab is going now, so it can sleep (and wake) correctly
			// even before Chrome reports the new address.
			in.mu.Lock()
			if t := in.tabs[id]; t != nil {
				t.URL = u
			}
			in.mu.Unlock()
			in.conn.Send(sess, "Page.navigate", map[string]string{"url": u})
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (in *instance) saveTabs() {
	in.mu.Lock()
	if in.closing {
		in.mu.Unlock()
		return
	}
	st := savedTabs{Active: 0}
	for _, id := range in.order {
		t := in.tabs[id]
		if t == nil || !allowedURL(t.URL) {
			continue
		}
		if id == in.active {
			st.Active = len(st.URLs)
		}
		st.URLs = append(st.URLs, t.URL)
		st.Titles = append(st.Titles, t.Title)
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
	in.mu.Lock()
	in.closing = true // tabs closing below must not overwrite the list just saved
	in.mu.Unlock()
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
		in.audio.close()
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
			in.tabs[ti.TargetID] = &Tab{ID: ti.TargetID, URL: ti.URL, Title: ti.Title, lastActive: time.Now()}
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
		if t := in.tabs[p.TargetID]; t != nil && t.Asleep {
			in.mu.Unlock() // put to sleep on purpose: keep its place in the tab strip
			return
		}
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
		empty := len(in.order) == 0 && !in.closing
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
			Data      []byte `json:"data"` // encoding/json decodes the base64 straight into bytes
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
		if len(p.Data) > 0 {
			in.broadcastFrame(p.Data)
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
	if t != nil && !t.Asleep {
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
	if t.Asleep {
		in.mu.Unlock()
		in.wake(targetID)
		return
	}
	now := time.Now()
	if old := in.tabs[in.active]; old != nil {
		old.lastActive = now
	}
	t.lastActive = now
	in.active = targetID
	sess, vp := t.session, in.vp
	in.mu.Unlock()
	// Shortly after a switch, put tabs beyond the profile's limit to sleep.
	defer time.AfterFunc(6*time.Second, in.enforceSleep)
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

// ---------- sleeping tabs ----------

// sleep closes a background tab but keeps it in the strip; opening it reloads the page.
func (in *instance) sleep(id string) {
	in.mu.Lock()
	t := in.tabs[id]
	if t == nil || t.Asleep || t.Loading || id == in.active || id == in.waking || t.URL == "" || time.Since(t.lastActive) < 5*time.Second {
		in.mu.Unlock()
		return
	}
	t.Asleep, t.session, t.Loading, t.CanBack, t.CanFwd = true, "", false, false, false
	in.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.conn.Call(ctx, "", "Target.closeTarget", map[string]string{"targetId": id}, nil); err != nil {
		in.mu.Lock()
		delete(in.tabs, id)
		for i, x := range in.order {
			if x == id {
				in.order = append(in.order[:i], in.order[i+1:]...)
				break
			}
		}
		in.mu.Unlock()
	}
	in.log.Debug("tab asleep", "url", t.URL)
	in.broadcastTabs()
}

// wake replaces a sleeping tab with a fresh one at the same place and loads its page.
func (in *instance) wake(id string) {
	in.mu.Lock()
	ph := in.tabs[id]
	if ph == nil || !ph.Asleep || in.waking != "" {
		in.mu.Unlock()
		return
	}
	in.waking = id
	in.active = id // keep the strip showing it as selected while it loads
	u, title := ph.URL, ph.Title
	in.mu.Unlock()
	in.broadcastTabs()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res struct {
		TargetID string `json:"targetId"`
	}
	err := in.conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &res)
	in.mu.Lock()
	in.waking = ""
	if err != nil {
		in.mu.Unlock()
		return
	}
	// The created-event may or may not have been handled yet; either way, put the new
	// tab where the sleeping one was.
	nt := in.tabs[res.TargetID]
	if nt == nil {
		nt = &Tab{ID: res.TargetID}
		in.tabs[res.TargetID] = nt
	}
	nt.URL, nt.Title, nt.lastActive = u, title, time.Now()
	order := make([]string, 0, len(in.order))
	for _, x := range in.order {
		switch x {
		case res.TargetID:
			continue
		case id:
			order = append(order, res.TargetID)
		default:
			order = append(order, x)
		}
	}
	in.order = order
	delete(in.tabs, id)
	in.mu.Unlock()
	in.activate(res.TargetID)
	if allowedURL(u) {
		in.navigateTab(res.TargetID, u)
	}
}

// enforceSleep puts background tabs to sleep: beyond the profile's live-tab limit,
// after the idle period, or all of them when the host is short of memory.
func (in *instance) enforceSleep() {
	prof := in.m.prof
	low := false
	if mem := sysmem.Read(); mem.Available > 0 && mem.Available < 100<<20 {
		low = true
	}
	in.mu.Lock()
	var bg []*Tab
	for _, id := range in.order {
		if t := in.tabs[id]; t != nil && !t.Asleep && id != in.active && id != in.waking {
			bg = append(bg, t)
		}
	}
	in.mu.Unlock()
	sort.Slice(bg, func(i, j int) bool { return bg[i].lastActive.Before(bg[j].lastActive) })
	keep := max(prof.LiveTabs-1, 0)
	idle := time.Duration(prof.SleepAfterMin) * time.Minute
	for i, t := range bg {
		over := len(bg)-i > keep
		if low || over || time.Since(t.lastActive) > idle {
			in.sleep(t.ID)
		}
	}
}

func (in *instance) sleepLoop() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-in.stopped:
			return
		case <-tick.C:
			in.enforceSleep()
		}
	}
}

// closeTab closes a tab; sleeping tabs have no page to close, so they are just dropped.
func (in *instance) closeTab(id string) {
	in.mu.Lock()
	t := in.tabs[id]
	if t != nil && t.Asleep {
		delete(in.tabs, id)
		for i, x := range in.order {
			if x == id {
				in.order = append(in.order[:i], in.order[i+1:]...)
				break
			}
		}
		in.mu.Unlock()
		in.broadcastTabs()
		go in.saveTabs()
		return
	}
	in.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = in.conn.Call(ctx, "", "Target.closeTarget", map[string]string{"targetId": id}, nil)
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
	msg := prefixed(msgFrame, img)
	in.mu.Lock()
	defer in.mu.Unlock()
	for c := range in.clients {
		c.sendFrame(msg)
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
	c.sendJSON(map[string]any{"t": "audio", "available": in.audio.enabled})
	c.sendJSON(in.snapshot())
	if active != "" {
		in.startCast(active)
	}
}

func (in *instance) removeClient(c *client) {
	in.audio.setListening(c, false)
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
