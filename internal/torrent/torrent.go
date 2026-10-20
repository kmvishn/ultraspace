// Package torrent is the BitTorrent client behind the Torrents app, and the engine that
// lets the video player (and the Stremio app) play a torrent while it downloads.
//
// The engine (anacrolix/torrent) runs inside ultraspace but only while there is work:
// it starts when a torrent is added, resumed or played, and stops a couple of minutes
// after the last one finishes or pauses. Downloads land in Files → Torrents.
//
// Trackers, web seeds and peers are limited to public internet addresses (the same rule
// as the server browser), so a crafted magnet link cannot make the server talk to itself,
// the LAN or the cloud metadata service.
package torrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/types"

	"ultraspace/internal/browser"
	"ultraspace/internal/mediasrc"
	"ultraspace/internal/sysmem"
)

// Options configure the manager.
type Options struct {
	Dir       string  // downloads (inside Files): <files_root>/Torrents
	FilesPath string  // the same folder as Files shows it: "/Torrents"
	StateDir  string  // <data_dir>/torrents: the list, .torrent files, piece completion
	Port      int     // incoming peer port; 0 picks a random one
	SeedRatio float64 // stop seeding at this upload/download ratio; 0 = never seed, <0 = forever
	Profile   sysmem.Profile
	Log       *slog.Logger

	allowPrivate bool // tests only: let the engine reach loopback web seeds
}

const (
	maxTorrents   = 300
	streamIdle    = 10 * time.Minute // a streamed torrent pauses this long after the player stops
	engineIdle    = 2 * time.Minute  // the engine stops this long after the last active torrent
	tick          = 2 * time.Second
	waitForInfo   = 90 * time.Second
	stateFileName = "torrents.json"
)

var errNotFound = &mediasrc.StatusError{Code: http.StatusNotFound, Msg: "no such torrent"}

// item is one torrent in the list. Exported fields are saved to torrents.json.
type item struct {
	Hash     string    `json:"hash"`
	Magnet   string    `json:"magnet,omitempty"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Have     int64     `json:"have"` // bytes complete when last seen
	Added    time.Time `json:"added"`
	Finished time.Time `json:"finished,omitzero"`
	Paused   bool      `json:"paused"`
	Stream   bool      `json:"stream"` // added to play: only played files download
	Skip     []int     `json:"skip,omitempty"`
	Only     *int      `json:"only,omitempty"` // download just this file, once the file list is known
	Down     int64     `json:"downloaded"`     // payload bytes, earlier sessions
	Up       int64     `json:"uploaded"`

	t        *torrent.Torrent
	readers  int
	lastRead time.Time
	lastDown int64 // session counters at the last tick
	lastUp   int64
	rateDown float64
	rateUp   float64
	err      string
}

type Manager struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	cl       *torrent.Client
	store    storage.ClientImplCloser
	items    map[string]*item
	idleFrom time.Time
	dirty    bool
}

func New(opts Options) (*Manager, error) {
	if err := os.MkdirAll(opts.StateDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, log: opts.Log.With("component", "torrent"), items: map[string]*item{}}
	if b, err := os.ReadFile(filepath.Join(opts.StateDir, stateFileName)); err == nil {
		var list []*item
		if err := json.Unmarshal(b, &list); err != nil {
			m.log.Warn("torrent list unreadable; starting empty", "err", err)
		}
		for _, it := range list {
			if _, err := parseHash(it.Hash); err == nil {
				m.items[it.Hash] = it
			}
		}
	}
	return m, nil
}

// Run keeps speeds, completion, seeding and idle shutdown up to date until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	for _, it := range m.items {
		if !it.Paused && it.Finished.IsZero() && !it.Stream {
			m.attachLocked(it)
		}
	}
	m.mu.Unlock()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			m.update()
		}
	}
}

// Close stops the engine and saves the list.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		m.detachLocked(it)
	}
	m.stopLocked()
	m.saveLocked()
}

// ---------- engine ----------

func (m *Manager) startLocked() error {
	if m.cl != nil {
		return nil
	}
	pc, err := storage.NewBoltPieceCompletion(m.opts.StateDir)
	if err != nil {
		return fmt.Errorf("piece database: %w", err)
	}
	st := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: m.opts.Dir, PieceCompletion: pc})
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = m.opts.Dir
	cfg.DefaultStorage = st
	cfg.ListenPort = m.opts.Port
	cfg.NoDefaultPortForwarding = true
	cfg.Seed = m.opts.SeedRatio != 0
	cfg.DisableWebtorrent = true // WebRTC would cost memory for few extra peers
	cfg.Slogger = slog.New(warnOnly{m.log.Handler()})
	cfg.HTTPUserAgent = "ultraspace"
	conns, half := 50, 25
	switch m.opts.Profile.Name {
	case "tiny":
		conns, half = 20, 8
	case "small":
		conns, half = 35, 15
	}
	cfg.EstablishedConnsPerTorrent = conns
	cfg.HalfOpenConnsPerTorrent = half
	cfg.TotalHalfOpenConns = half * 3
	cfg.TorrentPeersHighWater = conns * 8
	// Public addresses only, for every kind of connection the engine makes.
	if !m.opts.allowPrivate {
		m.restrictToPublic(cfg)
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		st.Close()
		return fmt.Errorf("start torrent engine: %w", err)
	}
	m.cl, m.store = cl, st
	m.idleFrom = time.Now()
	m.log.Info("torrent engine started", "port", cl.LocalPort())
	return nil
}

// restrictToPublic limits trackers, web seeds, metadata sources and peers to public addresses.
func (m *Manager) restrictToPublic(cfg *torrent.ClientConfig) {
	dial := browser.PublicDialer()
	cfg.HTTPDialContext = dial.DialContext
	cfg.TrackerDialContext = dial.DialContext
	cfg.TrackerListenPacket = func(network, addr string) (net.PacketConn, error) {
		pc, err := net.ListenPacket(network, addr)
		if err != nil {
			return nil, err
		}
		return publicPackets{pc}, nil
	}
	cfg.WebTransport = mediasrc.PublicClient().Transport
	cfg.MetainfoSourcesClient = mediasrc.PublicClient()
	cfg.IPBlocklist = notPublic{}
}

func (m *Manager) stopLocked() {
	if m.cl == nil {
		return
	}
	m.cl.Close()
	m.store.Close()
	m.cl, m.store = nil, nil
	m.log.Info("torrent engine stopped")
}

// attachLocked puts a torrent into the engine (starting it if needed).
func (m *Manager) attachLocked(it *item) error {
	if it.t != nil {
		return nil
	}
	if err := m.startLocked(); err != nil {
		it.err = err.Error()
		return err
	}
	var t *torrent.Torrent
	var err error
	if mi, merr := metainfo.LoadFromFile(m.metaPath(it.Hash)); merr == nil {
		t, err = m.cl.AddTorrent(mi)
	} else {
		t, err = m.cl.AddMagnet(it.Magnet)
	}
	if err != nil {
		it.err = err.Error()
		return err
	}
	it.t, it.err = t, ""
	it.lastDown, it.lastUp = 0, 0
	go m.onInfo(it, t)
	return nil
}

// onInfo runs once the torrent's metadata is known: saves it and sets what to download.
func (m *Manager) onInfo(it *item, t *torrent.Torrent) {
	select {
	case <-t.GotInfo():
	case <-t.Closed():
		return
	}
	mi := t.Metainfo()
	if _, err := os.Stat(m.metaPath(it.Hash)); err != nil {
		if f, err := os.CreateTemp(m.opts.StateDir, ".meta-*"); err == nil {
			werr := mi.Write(f)
			if errors.Join(werr, f.Close()) == nil {
				_ = os.Rename(f.Name(), m.metaPath(it.Hash))
			} else {
				os.Remove(f.Name())
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.t != t {
		return
	}
	it.Name, it.Size = t.Name(), t.Length()
	m.applyWantedLocked(it)
	m.dirty = true
}

// applyWantedLocked sets file priorities: all but skipped files, or (streams) none until played.
func (m *Manager) applyWantedLocked(it *item) {
	if it.t == nil || it.t.Info() == nil {
		return
	}
	if it.Only != nil {
		only := *it.Only
		if only == -1 {
			only = largestPlayable(it.t.Files())
		}
		it.Skip = nil
		for i := range it.t.Files() {
			if i != only {
				it.Skip = append(it.Skip, i)
			}
		}
		it.Only = nil
		m.dirty = true
	}
	for i, f := range it.t.Files() {
		switch {
		case it.Stream:
			// Readers fetch what is watched; the played file is set to download in Resolve.
		case slices.Contains(it.Skip, i):
			f.SetPriority(types.PiecePriorityNone)
		default:
			f.SetPriority(types.PiecePriorityNormal)
		}
	}
}

func (m *Manager) detachLocked(it *item) {
	if it.t == nil {
		return
	}
	s := it.t.Stats()
	it.Down += s.BytesReadUsefulData.Int64()
	it.Up += s.BytesWrittenData.Int64()
	if it.t.Info() != nil {
		it.Have = it.t.BytesCompleted()
	}
	it.t.Drop()
	it.t = nil
	it.rateDown, it.rateUp = 0, 0
	m.dirty = true
}

func (m *Manager) update() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	active := false
	for _, it := range m.items {
		if it.t == nil {
			continue
		}
		s := it.t.Stats()
		down, up := s.BytesReadUsefulData.Int64(), s.BytesWrittenData.Int64()
		it.rateDown = 0.6*it.rateDown + 0.4*float64(down-it.lastDown)/tick.Seconds()
		it.rateUp = 0.6*it.rateUp + 0.4*float64(up-it.lastUp)/tick.Seconds()
		it.lastDown, it.lastUp = down, up
		if it.t.Info() != nil {
			it.Have = it.t.BytesCompleted()
			if it.Finished.IsZero() && !it.Stream && wantedMissing(it) == 0 {
				it.Finished = now
				m.dirty = true
				m.log.Info("torrent finished", "name", it.Name)
			}
		}
		switch {
		case it.readers > 0:
		case it.Stream && now.Sub(it.lastRead) > streamIdle:
			it.Paused = true // stop fetching a film nobody is watching
			m.detachLocked(it)
			continue
		case !it.Finished.IsZero() && !m.seeding(it):
			m.detachLocked(it)
			continue
		}
		active = true
	}
	if active {
		m.idleFrom = now
	} else if m.cl != nil && now.Sub(m.idleFrom) > engineIdle {
		m.stopLocked()
	}
	if m.dirty {
		m.saveLocked()
	}
}

// wantedMissing is how many bytes of the wanted (not skipped) files are still missing.
func wantedMissing(it *item) int64 {
	var n int64
	for i, f := range it.t.Files() {
		if !slices.Contains(it.Skip, i) {
			n += f.Length() - f.BytesCompleted()
		}
	}
	return n
}

// seeding reports whether a finished torrent should keep uploading.
func (m *Manager) seeding(it *item) bool {
	r := m.opts.SeedRatio
	switch {
	case r == 0:
		return false
	case r < 0:
		return true
	}
	up := it.Up
	if it.t != nil {
		s := it.t.Stats()
		up += s.BytesWrittenData.Int64()
	}
	size := it.Size
	if size <= 0 {
		return false
	}
	return float64(up)/float64(size) < r
}

func (m *Manager) metaPath(hash string) string {
	return filepath.Join(m.opts.StateDir, hash+".torrent")
}

func (m *Manager) saveLocked() {
	list := make([]*item, 0, len(m.items))
	for _, it := range m.items {
		list = append(list, it)
	}
	slices.SortFunc(list, func(a, b *item) int { return a.Added.Compare(b.Added) })
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return
	}
	tmp := filepath.Join(m.opts.StateDir, "."+stateFileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o640); err == nil && os.Rename(tmp, filepath.Join(m.opts.StateDir, stateFileName)) == nil {
		m.dirty = false
	}
}

// ---------- adding and controlling torrents ----------

func parseHash(s string) (metainfo.Hash, error) {
	var h metainfo.Hash
	if len(s) != 40 {
		return h, errors.New("bad info hash")
	}
	err := h.FromHexString(s)
	return h, err
}

// AddMagnet adds a magnet link (or returns the torrent that is already there).
func (m *Manager) AddMagnet(uri string, stream bool) (string, error) {
	mag, err := metainfo.ParseMagnetUri(uri)
	if err != nil {
		return "", &mediasrc.StatusError{Code: http.StatusBadRequest, Msg: "that is not a magnet link"}
	}
	hash := mag.InfoHash.HexString()
	return hash, m.add(&item{Hash: hash, Magnet: uri, Name: mag.DisplayName, Stream: stream})
}

// AddFile adds a .torrent file's contents.
func (m *Manager) AddFile(data []byte, stream bool) (string, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return "", &mediasrc.StatusError{Code: http.StatusBadRequest, Msg: "that is not a .torrent file"}
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return "", &mediasrc.StatusError{Code: http.StatusBadRequest, Msg: "that .torrent file is damaged"}
	}
	hash := mi.HashInfoBytes().HexString()
	if err := os.WriteFile(m.metaPath(hash), data, 0o640); err != nil {
		return "", err
	}
	return hash, m.add(&item{Hash: hash, Magnet: mi.Magnet(nil, &info).String(), Name: info.BestName(), Size: info.TotalLength(), Stream: stream})
}

func (m *Manager) add(it *item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.items[it.Hash]; old != nil {
		// Asking to download a torrent that was only streamed turns it into a download.
		if !it.Stream && old.Stream {
			old.Stream, old.Paused = false, false
			m.applyWantedLocked(old)
			m.dirty = true
		}
		if !it.Stream && old.Paused {
			old.Paused = false
			m.dirty = true
		}
		if !old.Paused && (old.Finished.IsZero() || m.seeding(old)) && !old.Stream {
			return m.attachLocked(old)
		}
		return nil
	}
	if len(m.items) >= maxTorrents {
		return &mediasrc.StatusError{Code: http.StatusConflict, Msg: "too many torrents; remove some first"}
	}
	it.Added = time.Now()
	m.items[it.Hash] = it
	m.dirty = true
	if it.Stream {
		it.lastRead = time.Now() // keep it for a while even if playback never starts
	}
	return m.attachLocked(it)
}

// Do runs an action: pause, resume, remove or delete (remove with its files).
func (m *Manager) Do(hash, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[hash]
	if it == nil {
		return errNotFound
	}
	switch action {
	case "pause":
		it.Paused = true
		m.detachLocked(it)
	case "resume":
		it.Paused = false
		if it.Stream {
			// Resuming a streamed torrent from the Torrents app means: download it.
			it.Stream = false
		}
		it.Finished = time.Time{}
		m.dirty = true
		return m.attachLocked(it)
	case "remove", "delete":
		if it.readers > 0 {
			return &mediasrc.StatusError{Code: http.StatusConflict, Msg: "it is playing; close the player first"}
		}
		m.detachLocked(it)
		delete(m.items, hash)
		os.Remove(m.metaPath(hash))
		m.dirty = true
		if action == "delete" {
			return m.deleteData(it.Name)
		}
	default:
		return &mediasrc.StatusError{Code: http.StatusBadRequest, Msg: "unknown action"}
	}
	return nil
}

// deleteData removes a torrent's files from Files → Torrents, never anything outside it.
func (m *Manager) deleteData(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return nil
	}
	root, err := os.OpenRoot(m.opts.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return errors.Join(root.RemoveAll(name), root.RemoveAll(name+".part"))
}

// DownloadOnly turns a torrent into a download of one file (-1: the largest video),
// for example one episode out of a season pack found through Stremio.
func (m *Manager) DownloadOnly(hash string, idx int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[hash]
	if it == nil {
		return errNotFound
	}
	it.Only = &idx
	it.Stream, it.Paused, it.Finished = false, false, time.Time{}
	m.dirty = true
	m.applyWantedLocked(it)
	return m.attachLocked(it)
}

// SetSkip sets which files of a torrent not to download.
func (m *Manager) SetSkip(hash string, skip []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[hash]
	if it == nil {
		return errNotFound
	}
	it.Skip = slices.Compact(slices.Sorted(slices.Values(skip)))
	if it.Stream {
		it.Stream = false // choosing files means downloading them
	}
	if len(it.Skip) == 0 {
		it.Skip = nil
	}
	it.Finished = time.Time{}
	m.dirty = true
	m.applyWantedLocked(it)
	if !it.Paused {
		return m.attachLocked(it)
	}
	return nil
}

// ---------- reporting ----------

// Summary is a torrent as the app lists it.
type Summary struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	Path     string  `json:"path"` // in Files
	Size     int64   `json:"size"`
	Have     int64   `json:"have"`
	Progress float64 `json:"progress"`
	State    string  `json:"state"` // metadata, downloading, seeding, done, paused, error
	Down     float64 `json:"down"`  // bytes/s
	Up       float64 `json:"up"`
	Peers    int     `json:"peers"`
	Seeders  int     `json:"seeders"`
	Ratio    float64 `json:"ratio"`
	Stream   bool    `json:"stream"`
	Playing  bool    `json:"playing"`
	Added    int64   `json:"added"`
	Error    string  `json:"error,omitempty"`
}

func (m *Manager) summary(it *item) Summary {
	s := Summary{Hash: it.Hash, Name: it.Name, Size: it.Size, Have: it.Have, Stream: it.Stream,
		Playing: it.readers > 0, Added: it.Added.UnixMilli(), Error: it.err, Down: it.rateDown, Up: it.rateUp}
	if s.Name == "" {
		s.Name = it.Hash
	} else {
		s.Path = m.opts.FilesPath + "/" + it.Name
	}
	up := it.Up
	switch {
	case it.err != "":
		s.State = "error"
	case it.t != nil && it.t.Info() == nil:
		s.State = "metadata"
	case it.t != nil:
		st := it.t.Stats()
		s.Peers, s.Seeders = st.ActivePeers, st.ConnectedSeeders
		up += st.BytesWrittenData.Int64()
		if it.Finished.IsZero() {
			s.State = "downloading"
		} else {
			s.State = "seeding"
		}
	case it.Paused:
		s.State = "paused"
	case !it.Finished.IsZero():
		s.State = "done"
	default:
		s.State = "paused"
	}
	if s.Size > 0 {
		s.Progress = min(1, float64(s.Have)/float64(s.Size))
		s.Ratio = float64(up) / float64(s.Size)
	}
	return s
}

// List returns all torrents, newest first, and engine totals.
func (m *Manager) List() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Summary, 0, len(m.items))
	var down, up float64
	for _, it := range m.items {
		s := m.summary(it)
		down += s.Down
		up += s.Up
		list = append(list, s)
	}
	slices.SortFunc(list, func(a, b Summary) int { return int(b.Added - a.Added) })
	port := 0
	if m.cl != nil {
		port = m.cl.LocalPort()
	}
	return map[string]any{"torrents": list, "running": m.cl != nil, "down": down, "up": up, "port": port}
}

// FileInfo is one file inside a torrent.
type FileInfo struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Have     int64  `json:"have"`
	Wanted   bool   `json:"wanted"`
	Playable bool   `json:"playable"`
}

// Detail returns one torrent with its files and trackers.
func (m *Manager) Detail(hash string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[hash]
	if it == nil {
		return nil, errNotFound
	}
	files := []FileInfo{}
	trackers := []string{}
	if it.t != nil && it.t.Info() != nil {
		for i, f := range it.t.Files() {
			wanted := !slices.Contains(it.Skip, i)
			if it.Stream {
				wanted = f.Priority() != types.PiecePriorityNone // only what was played
			}
			files = append(files, FileInfo{Index: i, Path: f.DisplayPath(), Size: f.Length(), Have: f.BytesCompleted(),
				Wanted: wanted, Playable: isPlayable(f.DisplayPath())})
		}
	} else if mi, err := metainfo.LoadFromFile(m.metaPath(hash)); err == nil {
		if info, err := mi.UnmarshalInfo(); err == nil {
			for i, f := range info.UpvertedFiles() {
				p := strings.Join(f.BestPath(), "/")
				if len(info.Files) == 0 {
					p = info.BestName()
				}
				files = append(files, FileInfo{Index: i, Path: p, Size: f.Length, Wanted: !slices.Contains(it.Skip, i), Playable: isPlayable(p)})
			}
		}
	}
	if mag, err := metainfo.ParseMagnetUri(it.Magnet); err == nil {
		trackers = mag.Trackers
	}
	return map[string]any{"torrent": m.summary(it), "files": files, "trackers": trackers, "magnet": it.Magnet}, nil
}

var playableExts = []string{".mp4", ".m4v", ".mkv", ".webm", ".avi", ".mov", ".wmv", ".flv", ".mpg", ".mpeg", ".ts", ".m2ts", ".mts", ".3gp", ".ogv", ".vob", ".divx", ".mp3", ".flac", ".m4a", ".ogg", ".opus", ".wav"}
var subtitleExts = []string{".srt", ".vtt", ".ass", ".ssa"}

// largestPlayable is the index of the biggest video or audio file, or -1.
func largestPlayable(files []*torrent.File) int {
	idx := -1
	for i, f := range files {
		if isPlayable(f.DisplayPath()) && (idx < 0 || f.Length() > files[idx].Length()) {
			idx = i
		}
	}
	return idx
}

func isPlayable(name string) bool {
	return slices.Contains(playableExts, strings.ToLower(path.Ext(name)))
}

// ---------- playing a torrent while it downloads ----------

// Resolve is the media source resolver for "torrent:<hash>:<file index>" (index -1 = the
// largest video). The player reads it with Range requests; the engine fetches the pieces
// being read first, then the rest of that file.
func (m *Manager) Resolve(ctx context.Context, ref string) (*mediasrc.Source, error) {
	hash, idxStr, _ := strings.Cut(ref, ":")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < -1 {
		return nil, &mediasrc.StatusError{Code: http.StatusBadRequest, Msg: "bad torrent file"}
	}
	m.mu.Lock()
	it := m.items[hash]
	if it == nil {
		m.mu.Unlock()
		return nil, errNotFound
	}
	if it.Paused {
		it.Paused = false
		m.dirty = true
	}
	it.lastRead = time.Now()
	if err := m.attachLocked(it); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	t := it.t
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, waitForInfo)
	defer cancel()
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return nil, &mediasrc.StatusError{Code: http.StatusGatewayTimeout, Msg: "could not get this torrent's details (no peers yet)"}
	}
	files := t.Files()
	if idx == -1 {
		idx = largestPlayable(files)
	}
	if idx < 0 || idx >= len(files) {
		return nil, &mediasrc.StatusError{Code: http.StatusNotFound, Msg: "no playable file in this torrent"}
	}
	f := files[idx]
	name := path.Base(f.DisplayPath())
	m.mu.Lock()
	if it.Stream && it.t == t {
		f.SetPriority(types.PiecePriorityNormal) // keep the film coming while it plays
	}
	m.mu.Unlock()

	var subs []mediasrc.Subtitle
	base := strings.TrimSuffix(name, path.Ext(name))
	for i, sf := range files {
		p := sf.DisplayPath()
		if !slices.Contains(subtitleExts, strings.ToLower(path.Ext(p))) {
			continue
		}
		sname := path.Base(p)
		lang := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(sname, base), path.Ext(sname)), ". _-")
		if lang == strings.TrimSuffix(sname, path.Ext(sname)) {
			lang = "" // not named after the video
		}
		subs = append(subs, mediasrc.Subtitle{Src: fmt.Sprintf("torrent:%s:%d", hash, i), Lang: lang, Title: p})
	}
	readahead := int64(16 << 20)
	if m.opts.Profile.Name == "tiny" {
		readahead = 6 << 20
	}
	serve := func(w http.ResponseWriter, r *http.Request) {
		rd := f.NewReader()
		rd.SetContext(r.Context())
		rd.SetReadahead(readahead)
		defer rd.Close()
		m.reading(it, 1)
		defer m.reading(it, -1)
		w.Header().Set("Content-Type", mediasrc.ContentType(name))
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "", time.Time{}, rd)
	}
	return &mediasrc.Source{Name: name, Serve: serve, Slow: true, Subtitles: subs}, nil
}

func (m *Manager) reading(it *item, d int) {
	m.mu.Lock()
	it.readers += d
	it.lastRead = time.Now()
	m.mu.Unlock()
}

// ---------- network filters ----------

// notPublic blocks peers at loopback, private, link-local and other non-public addresses.
type notPublic struct{}

func (notPublic) Lookup(ip net.IP) (iplist.Range, bool) {
	a, ok := netip.AddrFromSlice(ip)
	if ok && browser.IsPublic(a) {
		return iplist.Range{}, false
	}
	return iplist.Range{First: ip, Last: ip, Description: "not a public address"}, true
}

func (notPublic) NumRanges() int { return 1 }

// publicPackets drops UDP tracker packets addressed to non-public addresses.
type publicPackets struct{ net.PacketConn }

func (p publicPackets) WriteTo(b []byte, addr net.Addr) (int, error) {
	if ua, ok := addr.(*net.UDPAddr); ok {
		if a, ok := netip.AddrFromSlice(ua.IP); ok && browser.IsPublic(a) {
			return p.PacketConn.WriteTo(b, addr)
		}
	}
	return 0, browser.ErrBlockedAddr
}

// warnOnly passes the engine's warnings and errors to our log, not its chatter.
type warnOnly struct{ slog.Handler }

func (h warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}
func (h warnOnly) WithAttrs(a []slog.Attr) slog.Handler { return warnOnly{h.Handler.WithAttrs(a)} }
func (h warnOnly) WithGroup(n string) slog.Handler      { return warnOnly{h.Handler.WithGroup(n)} }
