// Package youtube is a lightweight, account-free YouTube client in the
// spirit of NewPipe: it talks to YouTube's internal Innertube endpoints,
// keeps nothing on Google's side, and hands playable streams to the caller.
// Playback always goes through the server, because the stream addresses
// YouTube hands out are tied to the machine that asked for them.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ultraspace/internal/mediasrc"
)

// webKey is the public key YouTube's own web client ships with; it identifies
// the client, not a user, and no account is involved.
const webKey = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"

const apiBase = "https://www.youtube.com/youtubei/v1"

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{6,20}$`)

func validID(id string) bool { return idRE.MatchString(id) }

// Result is one item in a listing: a video, a Short, or a playlist.
// For playlists, ID is the playlist id and Thumb is a video id whose
// thumbnail represents it.
type Result struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "video", "short" or "playlist"
	Title     string `json:"title"`
	Channel   string `json:"channel"`
	ChannelID string `json:"channel_id"`
	Duration  string `json:"duration"`
	Views     string `json:"views"`
	Published string `json:"published"`
	Live      bool   `json:"live"`
	Count     string `json:"count,omitempty"`
	Thumb     string `json:"thumb,omitempty"`
}

// Format is one playable stream.
type Format struct {
	Itag      int
	Mime      string
	URL       string
	Bitrate   int
	Width     int
	Height    int
	AudioOnly bool
	VideoOnly bool
}

// Info is everything the watch page needs.
type Info struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Channel     string   `json:"channel"`
	ChannelID   string   `json:"channel_id"`
	Description string   `json:"description"`
	DurationSec int      `json:"duration_sec"`
	Views       string   `json:"views"`
	Live        bool     `json:"live"`
	Formats     []Format `json:"-"`
}

type Client struct {
	log   *slog.Logger
	http  *http.Client
	mu    sync.Mutex
	cache map[string]cachedInfo
	feeds map[string]cachedFeed

	// Enqueue, when set, queues resolved streams into Files (downloads).
	Enqueue EnqueueFunc
}

type cachedInfo struct {
	info *Info
	at   time.Time
}

type cachedFeed struct {
	title   string
	results []Result
	at      time.Time
}

func New(log *slog.Logger) *Client {
	return &Client{log: log, http: mediasrc.PublicClient(),
		cache: map[string]cachedInfo{}, feeds: map[string]cachedFeed{}}
}

// innertubeClient is one client identity to try, in order. YouTube moves
// these around; having several keeps playback working when one is refused.
type innertubeClient struct {
	name string
	body map[string]any
	ua   string
}

var playerClients = []innertubeClient{
	{
		name: "ANDROID",
		body: map[string]any{
			"clientName": "ANDROID", "clientVersion": "20.10.38",
			"deviceMake": "Google", "deviceModel": "Pixel 8",
			"androidSdkVersion": 34, "osName": "Android", "osVersion": "14",
			"hl": "en", "gl": "US",
		},
		ua: "com.google.android.youtube/20.10.38 (Linux; U; Android 14) gzip",
	},
	{
		name: "ANDROID_VR",
		body: map[string]any{
			"clientName": "ANDROID_VR", "clientVersion": "1.65.10",
			"deviceMake": "Oculus", "deviceModel": "Quest 3",
			"androidSdkVersion": 32, "osName": "Android", "osVersion": "12L",
			"hl": "en", "gl": "US",
		},
		ua: "com.google.android.youtube/1.65.10 (Linux; U; Android 12L) gzip",
	},
	{
		name: "IOS",
		body: map[string]any{
			"clientName": "IOS", "clientVersion": "20.10.4",
			"deviceMake": "Apple", "deviceModel": "iPhone16,2",
			"osName": "iOS", "osVersion": "18.3.2.22D82",
			"hl": "en", "gl": "US",
		},
		ua: "com.google.ios.youtube/20.10.4 (iPhone16,2; U; CPU iOS 18_3_2) gzip",
	},
}

func (c *Client) post(ctx context.Context, endpoint string, client map[string]any, extra map[string]any) ([]byte, error) {
	payload := map[string]any{"context": map[string]any{"client": client}}
	for k, v := range extra {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/"+endpoint+"?key="+webKey, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) ultraspace")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", endpoint, res.StatusCode)
	}
	return body, nil
}

// ---------- search ----------

func (c *Client) Search(ctx context.Context, query, region string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("type something to search for")
	}
	if len(query) > 200 {
		return nil, errors.New("search is too long")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	raw, err := c.post(ctx, "search", webClient(region), map[string]any{"query": query})
	if err != nil {
		return nil, errors.New("could not reach YouTube")
	}
	return parseSearch(raw), nil
}

// ---------- regions, trending, playlists, channels ----------

var regionRE = regexp.MustCompile(`^[A-Za-z]{2}$`)

// cleanRegion normalises a country code; anything odd falls back to US.
func cleanRegion(r string) string {
	if r = strings.ToUpper(strings.TrimSpace(r)); regionRE.MatchString(r) {
		return r
	}
	return "US"
}

func webClient(region string) map[string]any {
	return map[string]any{
		"clientName": "WEB", "clientVersion": "2.20250312.04.00",
		"hl": "en", "gl": cleanRegion(region),
	}
}

func remixClient(region string) map[string]any {
	return map[string]any{
		"clientName": "WEB_REMIX", "clientVersion": "1.20250310.01.00",
		"hl": "en", "gl": cleanRegion(region),
	}
}

// Trending returns the region's music chart as videos. YouTube retired the
// old trending page, but its charts still answer per country, and a chart is
// what "trending in India" means now. Results are cached for a few hours.
func (c *Client) Trending(ctx context.Context, region string) (string, []Result, error) {
	region = cleanRegion(region)
	c.mu.Lock()
	if hit, ok := c.feeds[region]; ok && time.Since(hit.at) < 6*time.Hour {
		c.mu.Unlock()
		return hit.title, hit.results, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	title, results, err := c.trending(ctx, region)
	if err != nil || len(results) == 0 {
		title, results = "Popular in "+region, nil
		if s, serr := c.Search(ctx, "top hits", region); serr == nil {
			results = s
		}
		if len(results) == 0 {
			if err == nil {
				err = errors.New("nothing trending right now")
			}
			return "", nil, err
		}
	}
	c.mu.Lock()
	c.feeds[region] = cachedFeed{title: title, results: results, at: time.Now()}
	c.mu.Unlock()
	return title, results, nil
}

func (c *Client) trending(ctx context.Context, region string) (string, []Result, error) {
	raw, err := c.post(ctx, "browse", remixClient(region), map[string]any{"browseId": "FEmusic_charts"})
	if err != nil {
		return "", nil, err
	}
	for _, pid := range findStrings(raw, "playlistId", "PL") {
		p, err := c.Playlist(ctx, pid)
		if err == nil && len(p.Results) > 0 {
			if p.Title == "" {
				p.Title = "Trending in " + region
			}
			return p.Title, p.Results, nil
		}
	}
	return "", nil, errors.New("no chart for this region")
}

// PlaylistInfo is one YouTube playlist and its first page of videos.
type PlaylistInfo struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Count   string   `json:"count"`
	Results []Result `json:"results"`
}

var playlistIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,70}$`)

// playlistIDFrom accepts a playlist URL, a watch URL with list=, or a bare id.
func playlistIDFrom(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "list="); i >= 0 {
		ref = ref[i+5:]
		if j := strings.IndexAny(ref, "&#? "); j >= 0 {
			ref = ref[:j]
		}
	}
	return ref, playlistIDRE.MatchString(ref)
}

// Playlist reads a YouTube playlist (including a channel's uploads list and
// the region charts) through the same browse endpoint the web client uses.
func (c *Client) Playlist(ctx context.Context, ref string) (*PlaylistInfo, error) {
	id, ok := playlistIDFrom(ref)
	if !ok {
		return nil, errors.New("that does not look like a playlist link")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.post(ctx, "browse", webClient("US"), map[string]any{"browseId": "VL" + id})
	if err != nil {
		return nil, errors.New("could not reach YouTube")
	}
	out := &PlaylistInfo{ID: id, Results: parseSearch(raw)}
	if out.Results == nil {
		out.Results = []Result{}
	}
	var root any
	if json.Unmarshal(raw, &root) == nil {
		walkRenderers(root, "playlistHeaderRenderer", func(m map[string]any) {
			if out.Title == "" {
				out.Title = simpleText(m["title"])
				out.Count = runsText(m["numVideosText"])
			}
		})
	}
	return out, nil
}

// ChannelInfo is a channel's latest uploads.
type ChannelInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Results []Result `json:"results"`
}

// channelVideosParams selects the Videos tab of a channel page.
const channelVideosParams = "EgZ2aWRlb3PyBgQKAjoA"

func (c *Client) Channel(ctx context.Context, id string) (*ChannelInfo, error) {
	if !strings.HasPrefix(id, "UC") || !playlistIDRE.MatchString(id) {
		return nil, errors.New("that is not a channel id")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.post(ctx, "browse", webClient("US"), map[string]any{
		"browseId": id, "params": channelVideosParams,
	})
	if err != nil {
		return nil, errors.New("could not reach YouTube")
	}
	out := &ChannelInfo{ID: id, Results: parseSearch(raw)}
	if out.Results == nil {
		out.Results = []Result{}
	}
	var root any
	if json.Unmarshal(raw, &root) == nil {
		walkRenderers(root, "channelMetadataRenderer", func(m map[string]any) {
			out.Name, _ = m["title"].(string)
		})
	}
	for i := range out.Results {
		if out.Results[i].Channel == "" {
			out.Results[i].Channel, out.Results[i].ChannelID = out.Name, id
		}
	}
	return out, nil
}

// Related lists what YouTube would put in "Up next" for a video.
func (c *Client) Related(ctx context.Context, id string) ([]Result, error) {
	if !validID(id) {
		return nil, errors.New("that is not a YouTube video id")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	raw, err := c.post(ctx, "next", webClient("US"), map[string]any{"videoId": id})
	if err != nil {
		return nil, errors.New("could not reach YouTube")
	}
	out := parseSearch(raw)
	kept := out[:0]
	for _, r := range out {
		if r.ID != id && r.Kind != "playlist" {
			kept = append(kept, r)
		}
	}
	if len(kept) > 25 {
		kept = kept[:25]
	}
	return kept, nil
}

// findStrings collects string values of one key, in document order, filtered
// by prefix ("" keeps all). Used to spot chart playlist ids in a browse page.
func findStrings(raw []byte, key, prefix string) []string {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if k == key {
					if s, ok := x.(string); ok && strings.HasPrefix(s, prefix) && !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(root)
	return out
}

// parseSearch walks the response for videos, wherever they sit. Older
// responses carry videoRenderer objects; newer ones (home, channels, "up
// next") carry lockupViewModel objects instead. Both are read.
func parseSearch(raw []byte) []Result {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	var out []Result
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if vr, ok := t["videoRenderer"].(map[string]any); ok {
				if r, ok := readRenderer(vr); ok && !seen[r.Kind+r.ID] {
					seen[r.Kind+r.ID] = true
					out = append(out, r)
				}
			}
			if lk, ok := t["lockupViewModel"].(map[string]any); ok {
				if r, ok := readLockup(lk); ok && !seen[r.Kind+r.ID] {
					seen[r.Kind+r.ID] = true
					out = append(out, r)
				}
			}
			if sk, ok := t["shortsLockupViewModel"].(map[string]any); ok {
				if r, ok := readShortsLockup(sk); ok && !seen[r.Kind+r.ID] {
					seen[r.Kind+r.ID] = true
					out = append(out, r)
				}
			}
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(root)
	return out
}

// walkRenderers calls fn for every object stored under key, anywhere in v.
func walkRenderers(v any, key string, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == key {
				if m, ok := x.(map[string]any); ok {
					fn(m)
				}
			}
			walkRenderers(x, key, fn)
		}
	case []any:
		for _, x := range t {
			walkRenderers(x, key, fn)
		}
	}
}

func mapAt(m map[string]any, path ...string) map[string]any {
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

var thumbIDRE = regexp.MustCompile(`/vi/([A-Za-z0-9_-]{6,20})/`)

// readLockup reads one lockupViewModel (the current card format): a video,
// a Short, or a playlist.
func readLockup(lk map[string]any) (Result, bool) {
	kind := "video"
	switch t, _ := lk["contentType"].(string); t {
	case "", "LOCKUP_CONTENT_TYPE_VIDEO":
	case "LOCKUP_CONTENT_TYPE_SHORTS":
		kind = "short"
	case "LOCKUP_CONTENT_TYPE_PLAYLIST":
		kind = "playlist"
	default:
		return Result{}, false
	}
	id, _ := lk["contentId"].(string)
	if kind == "playlist" {
		if _, ok := playlistIDFrom(id); !ok {
			return Result{}, false
		}
	} else if !validID(id) {
		return Result{}, false
	}
	r := Result{ID: id, Kind: kind}
	meta := mapAt(lk, "metadata", "lockupMetadataViewModel")
	if title := mapAt(meta, "title"); title != nil {
		r.Title, _ = title["content"].(string)
	}
	rows := []any{}
	if cm := mapAt(meta, "metadata", "contentMetadataViewModel"); cm != nil {
		rows, _ = cm["metadataRows"].([]any)
	}
	var parts []string
	for _, row := range rows {
		rm, _ := row.(map[string]any)
		for _, p := range sliceOf(rm["metadataParts"]) {
			pm, _ := p.(map[string]any)
			if txt := mapAt(pm, "text"); txt != nil {
				if s, _ := txt["content"].(string); s != "" {
					parts = append(parts, s)
				}
			}
		}
	}
	for _, p := range parts {
		switch {
		case strings.Contains(p, "watching"):
			r.Live, r.Views = true, p
		case strings.Contains(p, "views"):
			if r.Views == "" {
				r.Views = p
			}
		case strings.Contains(p, "ago") || strings.HasPrefix(p, "Streamed"):
			if r.Published == "" {
				r.Published = p
			}
		case r.Channel == "":
			r.Channel = p
		}
	}
	// Channel id, when the card links to the channel.
	walkRenderers(lk, "browseEndpoint", func(m map[string]any) {
		if r.ChannelID == "" {
			if s, _ := m["browseId"].(string); strings.HasPrefix(s, "UC") {
				r.ChannelID = s
			}
		}
	})
	// Duration or LIVE badge on the thumbnail.
	walkRenderers(lk, "thumbnailBadgeViewModel", func(m map[string]any) {
		s, _ := m["text"].(string)
		switch {
		case s == "LIVE":
			r.Live = true
		case r.Duration == "" && strings.Contains(s, ":"):
			r.Duration = s
		}
	})
	if kind == "playlist" {
		// The card thumbnail is a collage; borrow its first video's id.
		if raw, err := json.Marshal(lk); err == nil {
			if m := thumbIDRE.FindSubmatch(raw); m != nil {
				r.Thumb = string(m[1])
			}
		}
	}
	return r, r.Title != ""
}

var shortViewsRE = regexp.MustCompile(`^(.*), ([\d.,]+ ?[KMB]? ?(?:million|billion|thousand)? ?views) - play Short$`)

// readShortsLockup reads one Short from a search shelf: it carries no clean
// title field, so title and views come from its accessibility text.
func readShortsLockup(sk map[string]any) (Result, bool) {
	var id string
	walkRenderers(sk, "reelWatchEndpoint", func(m map[string]any) {
		if id == "" {
			id, _ = m["videoId"].(string)
		}
	})
	if id == "" {
		if eid, _ := sk["entityId"].(string); strings.HasPrefix(eid, "shorts-shelf-item-") {
			id = strings.TrimPrefix(eid, "shorts-shelf-item-")
		}
	}
	if !validID(id) {
		return Result{}, false
	}
	r := Result{ID: id, Kind: "short"}
	if text, _ := sk["accessibilityText"].(string); text != "" {
		if m := shortViewsRE.FindStringSubmatch(text); m != nil {
			r.Title, r.Views = m[1], m[2]
		} else {
			r.Title = strings.TrimSuffix(text, " - play Short")
		}
	}
	return r, r.Title != ""
}

func sliceOf(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func readRenderer(vr map[string]any) (Result, bool) {
	id, _ := vr["videoId"].(string)
	if !validID(id) {
		return Result{}, false
	}
	r := Result{ID: id, Kind: "video", Title: runsText(vr["title"]), Duration: simpleText(vr["lengthText"]),
		Views: simpleText(vr["viewCountText"]), Published: simpleText(vr["publishedTimeText"])}
	if nav, ok := vr["navigationEndpoint"].(map[string]any); ok {
		if _, isShort := nav["reelWatchEndpoint"]; isShort {
			r.Kind = "short"
		}
	}
	if byline, ok := vr["shortBylineText"].(map[string]any); ok {
		if runs, ok := byline["runs"].([]any); ok && len(runs) > 0 {
			if first, ok := runs[0].(map[string]any); ok {
				r.Channel, _ = first["text"].(string)
				if nav, ok := first["navigationEndpoint"].(map[string]any); ok {
					if browse, ok := nav["browseEndpoint"].(map[string]any); ok {
						r.ChannelID, _ = browse["browseId"].(string)
					}
				}
			}
		}
	}
	if badges, ok := vr["badges"].([]any); ok {
		for _, b := range badges {
			if s, ok := b.(map[string]any)["metadataBadgeRenderer"].(map[string]any); ok {
				if label, _ := s["label"].(string); strings.EqualFold(label, "LIVE") {
					r.Live = true
				}
			}
		}
	}
	if _, ok := vr["lengthText"]; !ok {
		// No length usually means a live stream.
		if overlays, ok := vr["thumbnailOverlays"].([]any); ok {
			for _, o := range overlays {
				if s, ok := o.(map[string]any)["thumbnailOverlayTimeStatusRenderer"].(map[string]any); ok {
					if style, _ := s["style"].(string); style == "LIVE" {
						r.Live = true
					}
				}
			}
		}
	}
	return r, r.Title != ""
}

func runsText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m["simpleText"].(string); ok {
		return s
	}
	var sb strings.Builder
	if runs, ok := m["runs"].([]any); ok {
		for _, r := range runs {
			if rm, ok := r.(map[string]any); ok {
				if t, ok := rm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
	}
	return sb.String()
}

func simpleText(v any) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["simpleText"].(string); ok {
			return s
		}
	}
	return runsText(v)
}

// ---------- subscription feed (public RSS, like NewPipe) ----------

type rssFeed struct {
	Title   string     `xml:"title"`
	Entries []rssEntry `xml:"entry"`
}

type rssEntry struct {
	VideoID   string    `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
	Title     string    `xml:"title"`
	Published time.Time `xml:"published"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

// Feed merges the newest uploads of the given channels, newest first. It
// reads each channel's public RSS feed, so nothing is signed in anywhere.
func (c *Client) Feed(ctx context.Context, channels []string) ([]Result, error) {
	seen := map[string]bool{}
	var ids []string
	for _, id := range channels {
		if strings.HasPrefix(id, "UC") && playlistIDRE.MatchString(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []Result{}, nil
	}
	if len(ids) > 40 {
		ids = ids[:40]
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var mu sync.Mutex
	var all []datedResult
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries, err := c.channelRSS(ctx, id)
			if err != nil {
				c.log.Debug("tube feed channel", "channel", id, "err", err)
				return
			}
			mu.Lock()
			all = append(all, entries...)
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	out := make([]Result, 0, len(all))
	for _, d := range all {
		r := d.r
		r.Published = relTime(d.at)
		out = append(out, r)
		if len(out) >= 60 {
			break
		}
	}
	return out, nil
}

type datedResult struct {
	r  Result
	at time.Time
}

func (c *Client) channelRSS(ctx context.Context, id string) ([]datedResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.youtube.com/feeds/videos.xml?channel_id="+id, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed answered %d", res.StatusCode)
	}
	var feed rssFeed
	if err := xml.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&feed); err != nil {
		return nil, err
	}
	out := make([]datedResult, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		if !validID(e.VideoID) || e.Title == "" {
			continue
		}
		name := e.Author.Name
		if name == "" {
			name = feed.Title
		}
		out = append(out, datedResult{Result{ID: e.VideoID, Title: e.Title, Channel: name, ChannelID: id}, e.Published})
	}
	return out, nil
}

func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// ---------- playback ----------

// Video returns details and direct stream URLs for one video, trying each
// client identity until YouTube offers playable formats.
func (c *Client) Video(ctx context.Context, id string) (*Info, error) {
	if !validID(id) {
		return nil, errors.New("that is not a YouTube video id")
	}
	c.mu.Lock()
	if hit, ok := c.cache[id]; ok && time.Since(hit.at) < 20*time.Minute {
		c.mu.Unlock()
		return hit.info, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var lastErr error
	for _, cl := range playerClients {
		raw, err := c.post(ctx, "player", cl.body, map[string]any{
			"videoId": id, "racyCheckOk": true, "contentCheckOk": true,
		})
		if err != nil {
			lastErr = err
			continue
		}
		info, err := parsePlayer(raw, id)
		if err != nil {
			lastErr = err
			continue
		}
		c.mu.Lock()
		c.cache[id] = cachedInfo{info: info, at: time.Now()}
		c.mu.Unlock()
		return info, nil
	}
	c.log.Warn("youtube player", "id", id, "err", lastErr)
	return nil, errors.New("YouTube would not offer this video for playback")
}

type playerResponse struct {
	Playability struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	StreamingData struct {
		Formats         []rawFormat `json:"formats"`
		AdaptiveFormats []rawFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`
	Details struct {
		Title         string `json:"title"`
		Author        string `json:"author"`
		ChannelID     string `json:"channelId"`
		ShortDesc     string `json:"shortDescription"`
		LengthSeconds string `json:"lengthSeconds"`
		ViewCount     string `json:"viewCount"`
		IsLive        bool   `json:"isLive"`
		IsLiveContent bool   `json:"isLiveContent"`
	} `json:"videoDetails"`
}

type rawFormat struct {
	Itag    int    `json:"itag"`
	Mime    string `json:"mimeType"`
	URL     string `json:"url"`
	Bitrate int    `json:"bitrate"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

func parsePlayer(raw []byte, id string) (*Info, error) {
	var pr playerResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, err
	}
	if pr.Playability.Status != "OK" {
		reason := pr.Playability.Reason
		if reason == "" {
			reason = pr.Playability.Status
		}
		return nil, errors.New(reason)
	}
	info := &Info{
		ID: id, Title: pr.Details.Title, Channel: pr.Details.Author,
		ChannelID: pr.Details.ChannelID, Description: pr.Details.ShortDesc,
		Views: pr.Details.ViewCount, Live: pr.Details.IsLive,
	}
	fmt.Sscan(pr.Details.LengthSeconds, &info.DurationSec)
	all := append(append([]rawFormat{}, pr.StreamingData.Formats...), pr.StreamingData.AdaptiveFormats...)
	for _, f := range all {
		if f.URL == "" {
			continue // needs deciphering; other clients offer direct URLs
		}
		info.Formats = append(info.Formats, Format{
			Itag: f.Itag, Mime: f.Mime, URL: f.URL, Bitrate: f.Bitrate,
			Width: f.Width, Height: f.Height,
			AudioOnly: strings.HasPrefix(f.Mime, "audio/"),
			VideoOnly: strings.HasPrefix(f.Mime, "video/") && !strings.Contains(f.Mime, "mp4a"),
		})
	}
	if pickVideo(info.Formats) == nil && pickAudio(info.Formats) == nil {
		return nil, errors.New("no playable formats")
	}
	return info, nil
}

// pickVideo prefers a muxed MP4 the browser can play as-is (720p, then 360p).
func pickVideo(formats []Format) *Format { return pickVideoUpTo(formats, 0) }

// pickVideoUpTo picks the best muxed MP4 at or below maxH (0 = any height).
func pickVideoUpTo(formats []Format, maxH int) *Format {
	var best *Format
	for i := range formats {
		f := &formats[i]
		if f.AudioOnly || f.VideoOnly || !strings.HasPrefix(f.Mime, "video/mp4") {
			continue
		}
		if maxH > 0 && f.Height > maxH {
			continue
		}
		if best == nil || f.Height > best.Height || (f.Height == best.Height && f.Bitrate > best.Bitrate) {
			best = f
		}
	}
	return best
}

// muxedHeights lists the heights available as ready-to-play muxed MP4s.
func muxedHeights(formats []Format) []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range formats {
		if !f.AudioOnly && !f.VideoOnly && strings.HasPrefix(f.Mime, "video/mp4") && f.Height > 0 && !seen[f.Height] {
			seen[f.Height] = true
			out = append(out, f.Height)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// pickAudio prefers AAC in MP4 (plays everywhere), then the best Opus.
func pickAudio(formats []Format) *Format {
	var mp4, any *Format
	for i := range formats {
		f := &formats[i]
		if !f.AudioOnly {
			continue
		}
		if any == nil || f.Bitrate > any.Bitrate {
			any = f
		}
		if strings.HasPrefix(f.Mime, "audio/mp4") && (mp4 == nil || f.Bitrate > mp4.Bitrate) {
			mp4 = f
		}
	}
	if mp4 != nil {
		return mp4
	}
	return any
}

// ---------- SponsorBlock, dislike counts, downloads ----------

// Segment is one SponsorBlock skip range, in seconds.
type Segment struct {
	Category string     `json:"category"`
	Segment  [2]float64 `json:"segment"`
}

// SponsorBlock returns the community-flagged sponsor/intro segments of a
// video from sponsor.ajay.app, the same public database LibreTube uses.
func (c *Client) SponsorBlock(ctx context.Context, id string) ([]Segment, error) {
	if !validID(id) {
		return nil, errors.New("that is not a YouTube video id")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://sponsor.ajay.app/api/skipSegments?videoID="+id+
			"&categories=%5B%22sponsor%22,%22intro%22,%22outro%22,%22selfpromo%22,%22music_offtopic%22%5D", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("could not reach SponsorBlock")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return []Segment{}, nil // nobody flagged anything in this video
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SponsorBlock answered %d", res.StatusCode)
	}
	var out []Segment
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Segment{}
	}
	return out, nil
}

// Votes is the like/dislike estimate from ReturnYouTubeDislike, the public
// archive LibreTube shows next to the like button (YouTube hides dislikes).
type Votes struct {
	Likes    int64   `json:"likes"`
	Dislikes int64   `json:"dislikes"`
	Rating   float64 `json:"rating"`
}

func (c *Client) Votes(ctx context.Context, id string) (*Votes, error) {
	if !validID(id) {
		return nil, errors.New("that is not a YouTube video id")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://returnyoutubedislikeapi.com/votes?videoId="+id, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("could not reach the dislike archive")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the dislike archive answered %d", res.StatusCode)
	}
	var out Votes
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Enqueue hands a resolved stream to the server's download manager (wired
// in server.go; nil means downloads are unavailable). It returns a job id.
type EnqueueFunc func(url, name string, headers map[string]string) (string, error)

// Download resolves a video (or just its audio) and queues it into Files.
func (c *Client) Download(ctx context.Context, id, mode string) (string, error) {
	if c.Enqueue == nil {
		return "", errors.New("downloads are not available on this server")
	}
	info, err := c.Video(ctx, id)
	if err != nil {
		return "", err
	}
	if info.Live {
		return "", errors.New("live streams cannot be downloaded")
	}
	var f *Format
	ext := ".mp4"
	if mode == "audio" {
		f = pickAudio(info.Formats)
		ext = ".m4a"
		if f != nil && strings.HasPrefix(f.Mime, "audio/webm") {
			ext = ".weba"
		}
	} else {
		f = pickVideo(info.Formats)
	}
	if f == nil {
		return "", errors.New("no downloadable stream for this video")
	}
	name := downloadName(info.Title, info.Channel) + ext
	return c.Enqueue(f.URL, name, map[string]string{
		"User-Agent": "com.google.android.youtube/20.10.38 (Linux; U; Android 14) gzip",
	})
}

// downloadName makes a tidy, filesystem-safe "Title - Channel" base name.
func downloadName(title, channel string) string {
	name := strings.TrimSpace(title)
	if channel != "" {
		name += " - " + channel
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if len(name) > 150 {
		name = strings.TrimSpace(name[:150])
	}
	if name == "" {
		name = "youtube-video"
	}
	return name
}

// sortedAudioFirst exists for tests and debugging.
func sortedAudioFirst(formats []Format) []Format {
	out := append([]Format{}, formats...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AudioOnly != out[j].AudioOnly {
			return out[i].AudioOnly
		}
		return out[i].Bitrate > out[j].Bitrate
	})
	return out
}
