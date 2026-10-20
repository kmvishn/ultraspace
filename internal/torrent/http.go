package torrent

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"ultraspace/internal/httpx"
	"ultraspace/internal/mediasrc"
)

const maxTorrentFile = 8 << 20

// Register adds the Torrents app's API.
func (m *Manager) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, require(h)) }
	handle("GET /api/torrents", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, m.List())
	})
	handle("POST /api/torrents", m.handleAdd)
	handle("GET /api/torrents/{hash}", func(w http.ResponseWriter, r *http.Request) {
		d, err := m.Detail(r.PathValue("hash"))
		if err != nil {
			mediasrc.Fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, d)
	})
	handle("POST /api/torrents/{hash}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Do(r.PathValue("hash"), r.PathValue("action")); err != nil {
			fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	handle("PUT /api/torrents/{hash}/files", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Skip []int `json:"skip"`
		}
		if !httpx.DecodeJSON(w, r, &req) {
			return
		}
		if err := m.SetSkip(r.PathValue("hash"), req.Skip); err != nil {
			fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// handleAdd takes {"magnet": "...", "stream": bool} as JSON, or a .torrent file as the
// multipart field "file".
func (m *Manager) handleAdd(w http.ResponseWriter, r *http.Request) {
	var hash string
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxTorrentFile+64<<10)
		file, _, ferr := r.FormFile("file")
		if ferr != nil {
			httpx.Error(w, http.StatusBadRequest, "choose a .torrent file (up to 8 MB)")
			return
		}
		defer file.Close()
		data, rerr := io.ReadAll(io.LimitReader(file, maxTorrentFile+1))
		if rerr != nil || len(data) > maxTorrentFile {
			httpx.Error(w, http.StatusBadRequest, "that .torrent file is too large")
			return
		}
		hash, err = m.AddFile(data, r.FormValue("stream") == "1")
	} else {
		var req struct {
			Magnet string `json:"magnet"`
			Stream bool   `json:"stream"`
			File   *int   `json:"file"` // download just this file (-1: the largest video)
		}
		if !httpx.DecodeJSONLimit(w, r, &req, 64<<10) {
			return
		}
		hash, err = m.AddMagnet(strings.TrimSpace(req.Magnet), req.Stream || req.File != nil)
		if err == nil && req.File != nil && !req.Stream {
			err = m.DownloadOnly(hash, *req.File)
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"hash": hash})
}

func fail(w http.ResponseWriter, err error) {
	var se *mediasrc.StatusError
	if errors.As(err, &se) {
		httpx.Error(w, se.Code, se.Msg)
		return
	}
	httpx.Error(w, http.StatusInternalServerError, err.Error())
}
