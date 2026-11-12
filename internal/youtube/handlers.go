package youtube

import (
	"io"
	"net/http"
	"strconv"

	"ultraspace/internal/httpx"
)

// Register wires the Tube endpoints into the router.
func (c *Client) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /api/youtube/search", auth(http.HandlerFunc(c.handleSearch)))
	mux.Handle("GET /api/youtube/trending", auth(http.HandlerFunc(c.handleTrending)))
	mux.Handle("GET /api/youtube/playlist", auth(http.HandlerFunc(c.handlePlaylist)))
	mux.Handle("GET /api/youtube/channel", auth(http.HandlerFunc(c.handleChannel)))
	mux.Handle("GET /api/youtube/related", auth(http.HandlerFunc(c.handleRelated)))
	mux.Handle("POST /api/youtube/feed", auth(http.HandlerFunc(c.handleFeed)))
	mux.Handle("GET /api/youtube/sponsorblock", auth(http.HandlerFunc(c.handleSponsorBlock)))
	mux.Handle("GET /api/youtube/votes", auth(http.HandlerFunc(c.handleVotes)))
	mux.Handle("POST /api/youtube/download", auth(http.HandlerFunc(c.handleDownload)))
	mux.Handle("GET /api/youtube/video", auth(http.HandlerFunc(c.handleVideo)))
	mux.Handle("GET /api/youtube/stream", auth(http.HandlerFunc(c.handleStream)))
	mux.Handle("GET /api/youtube/thumb", auth(http.HandlerFunc(c.handleThumb)))
}

func (c *Client) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	results, err := c.Search(r.Context(), q.Get("q"), q.Get("region"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	writeResults(w, results)
}

func (c *Client) handleTrending(w http.ResponseWriter, r *http.Request) {
	title, results, err := c.Trending(r.Context(), r.URL.Query().Get("region"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"title": title, "results": orEmpty(results)})
}

func (c *Client) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	info, err := c.Playlist(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (c *Client) handleChannel(w http.ResponseWriter, r *http.Request) {
	info, err := c.Channel(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (c *Client) handleRelated(w http.ResponseWriter, r *http.Request) {
	results, err := c.Related(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	writeResults(w, results)
}

func (c *Client) handleFeed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channels []string `json:"channels"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 64<<10) {
		return
	}
	results, err := c.Feed(r.Context(), req.Channels)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	writeResults(w, results)
}

func orEmpty(results []Result) []Result {
	if results == nil {
		return []Result{}
	}
	return results
}

func writeResults(w http.ResponseWriter, results []Result) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": orEmpty(results)})
}

func (c *Client) handleVideo(w http.ResponseWriter, r *http.Request) {
	info, err := c.Video(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"info": info, "heights": muxedHeights(info.Formats),
		"has_audio": pickAudio(info.Formats) != nil,
	})
}

func (c *Client) handleSponsorBlock(w http.ResponseWriter, r *http.Request) {
	segments, err := c.SponsorBlock(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"segments": segments})
}

func (c *Client) handleVotes(w http.ResponseWriter, r *http.Request) {
	votes, err := c.Votes(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, votes)
}

func (c *Client) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if !httpx.DecodeJSONLimit(w, r, &req, 16<<10) {
		return
	}
	job, err := c.Download(r.Context(), req.ID, req.Mode)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"job": job})
}

// handleStream relays one resolved stream to the browser, with Range, so
// seeking works and the browser never talks to googlevideo itself.
func (c *Client) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	mode := r.URL.Query().Get("mode")
	info, err := c.Video(r.Context(), id)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	if info.Live {
		httpx.Error(w, http.StatusNotImplemented, "live streams are not supported yet")
		return
	}
	var f *Format
	if mode == "audio" {
		f = pickAudio(info.Formats)
	} else {
		maxH, _ := strconv.Atoi(r.URL.Query().Get("h"))
		f = pickVideoUpTo(info.Formats, maxH)
		if f == nil {
			f = pickVideo(info.Formats)
		}
	}
	if f == nil {
		httpx.Error(w, http.StatusNotImplemented, "no playable stream for this video")
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, f.URL, nil)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad stream address")
		return
	}
	out.Header.Set("User-Agent", "com.google.android.youtube/20.10.38 (Linux; U; Android 14) gzip")
	if rg := r.Header.Get("Range"); rg != "" {
		out.Header.Set("Range", rg)
	}
	res, err := c.http.Do(out)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not reach the stream")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		httpx.Error(w, http.StatusBadGateway, "the stream answered "+strconv.Itoa(res.StatusCode))
		return
	}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := res.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, res.Body)
}

// handleThumb relays a video thumbnail (the page CSP allows only own images).
func (c *Client) handleThumb(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validID(id) {
		httpx.Error(w, http.StatusBadRequest, "bad video id")
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://i.ytimg.com/vi/"+id+"/hqdefault.jpg", nil)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad video id")
		return
	}
	res, err := c.http.Do(out)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not load the thumbnail")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		httpx.Error(w, http.StatusBadGateway, "no thumbnail")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if cl := res.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, res.Body)
}
