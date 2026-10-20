package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"ultraspace/internal/mediasrc"
	"ultraspace/internal/sysmem"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := New(Options{
		Dir: filepath.Join(dir, "files", "Torrents"), FilesPath: "/Torrents", StateDir: filepath.Join(dir, "state"),
		SeedRatio: 0, Profile: sysmem.Pick("small"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// makeTorrent builds a single-file .torrent whose data comes from a web seed on loopback.
func makeTorrent(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(src); err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(src))))
	t.Cleanup(seed.Close)
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), UrlList: []string{seed.URL + "/"}}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStreamWhileDownloading(t *testing.T) {
	m := newManager(t)
	data := make([]byte, 3<<20+12345)
	rand.Read(data)
	hash, err := m.AddFile(makeTorrent(t, "film.mp4", data), true)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src, err := m.Resolve(ctx, hash+":-1") // -1: the largest video
	if err != nil {
		t.Fatal(err)
	}
	if src.Name != "film.mp4" || !src.Slow {
		t.Fatalf("source = %+v", src)
	}
	// A ranged read in the middle, the way a video player seeks.
	req := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	req.Header.Set("Range", "bytes=2000000-2099999")
	rec := httptest.NewRecorder()
	src.Serve(rec, req)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), data[2000000:2100000]) {
		t.Fatalf("range read = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content type = %q", ct)
	}

	// ffmpeg reads sources through the loopback server, with ranges.
	reg := mediasrc.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	u, release, err := reg.Input(src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(all, data) {
		t.Fatalf("whole file via loopback: %d bytes, want %d", len(all), len(data))
	}
	release()
	if res, err := http.Get(u); err == nil && res.StatusCode != http.StatusNotFound {
		t.Fatalf("released token still served: %d", res.StatusCode)
	}

	list := m.List()["torrents"].([]Summary)
	if len(list) != 1 || !list[0].Stream || list[0].Path != "/Torrents/film.mp4" {
		t.Fatalf("list = %+v", list)
	}
}

func TestDownloadFinishesAndPersists(t *testing.T) {
	m := newManager(t)
	data := make([]byte, 1<<20)
	rand.Read(data)
	hash, err := m.AddFile(makeTorrent(t, "notes.bin", data), false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		m.update()
		d, _ := m.Detail(hash)
		if d["torrent"].(Summary).State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finished: %+v", d["torrent"])
		}
		time.Sleep(200 * time.Millisecond)
	}
	got, err := os.ReadFile(filepath.Join(m.opts.Dir, "notes.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("downloaded file: %v (%d bytes)", err, len(got))
	}
	// Finished and not seeding: the engine lets go of it.
	if d, _ := m.Detail(hash); len(d["files"].([]FileInfo)) != 1 {
		t.Fatalf("files from the saved .torrent = %+v", d["files"])
	}

	// The list survives a restart.
	m.Close()
	m2, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if s := m2.List()["torrents"].([]Summary); len(s) != 1 || s[0].State != "done" || s[0].Progress != 1 {
		t.Fatalf("after restart = %+v", s)
	}

	// Remove with data deletes the file, and only it.
	other := filepath.Join(m.opts.Dir, "keep.txt")
	os.WriteFile(other, []byte("x"), 0o644)
	if err := m2.Do(hash, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.opts.Dir, "notes.bin")); !os.IsNotExist(err) {
		t.Fatalf("data not deleted: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other file touched: %v", err)
	}
}

func TestAddRejectsGarbage(t *testing.T) {
	m := newManager(t)
	if _, err := m.AddMagnet("https://example.com/x.torrent", false); err == nil {
		t.Fatal("a web address was taken as a magnet link")
	}
	if _, err := m.AddFile([]byte("not bencode"), false); err == nil {
		t.Fatal("garbage taken as a .torrent")
	}
	if _, err := m.Resolve(context.Background(), "0000000000000000000000000000000000000000:0"); err == nil {
		t.Fatal("unknown torrent resolved")
	}
	if err := m.Do("nope", "pause"); err == nil {
		t.Fatal("action on an unknown torrent")
	}
	hash, err := m.AddMagnet("magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056&dn=Example", true)
	if err != nil || hash != "c9e15763f722f23e98a29decdfae341b98d53056" {
		t.Fatalf("magnet = %q %v", hash, err)
	}
	if err := m.Do(hash, "explode"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestDeleteDataStaysInside(t *testing.T) {
	m := newManager(t)
	outside := filepath.Join(filepath.Dir(m.opts.Dir), "precious")
	os.WriteFile(outside, []byte("x"), 0o644)
	for _, name := range []string{"..", "../precious", "/etc", "a/../../precious", ""} {
		_ = m.deleteData(name)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("file outside the torrent folder was removed: %v", err)
	}
}

func TestOnlyPublicAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254", "::1", "fd00::1", "100.64.0.1"} {
		if _, blocked := (notPublic{}).Lookup(net.ParseIP(ip)); !blocked {
			t.Errorf("peer %s not blocked", ip)
		}
	}
	if _, blocked := (notPublic{}).Lookup(net.ParseIP("1.1.1.1")); blocked {
		t.Error("public peer blocked")
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := (publicPackets{pc}).WriteTo([]byte("x"), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9}); err == nil {
		t.Error("UDP tracker packet to loopback was sent")
	}
}

func TestStreamingFileThroughFFprobe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	video := filepath.Join(t.TempDir(), "clip.mkv")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=6",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", video)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make video: %v %s", err, out)
	}
	data, _ := os.ReadFile(video)
	m := newManager(t)
	hash, err := m.AddFile(makeTorrent(t, "clip.mkv", data), true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src, err := m.Resolve(ctx, hash+":0")
	if err != nil {
		t.Fatal(err)
	}
	reg := mediasrc.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	u, release, err := reg.Input(src)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", u).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "h264") {
		t.Fatalf("ffprobe over the torrent: %v %s", err, out)
	}
}

func TestDownloadOnlyOneFile(t *testing.T) {
	m := newManager(t)
	dir := filepath.Join(t.TempDir(), "Season 1")
	os.MkdirAll(dir, 0o755)
	big := make([]byte, 900<<10)
	small := make([]byte, 300<<10)
	rand.Read(big)
	rand.Read(small)
	os.WriteFile(filepath.Join(dir, "E01.mkv"), small, 0o644)
	os.WriteFile(filepath.Join(dir, "E02.mkv"), big, 0o644)
	info := metainfo.Info{PieceLength: 32 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(dir))))
	defer seed.Close()
	var buf bytes.Buffer
	(&metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), UrlList: []string{seed.URL + "/"}}).Write(&buf)
	hash, err := m.AddFile(buf.Bytes(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DownloadOnly(hash, -1); err != nil { // -1: the largest video
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		m.update()
		if d, _ := m.Detail(hash); d["torrent"].(Summary).State == "done" {
			files := d["files"].([]FileInfo)
			if len(files) != 2 || files[0].Wanted || !files[1].Wanted {
				t.Fatalf("wanted files = %+v", files)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not finished")
		}
		time.Sleep(200 * time.Millisecond)
	}
	got, err := os.ReadFile(filepath.Join(m.opts.Dir, "Season 1", "E02.mkv"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("E02: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.opts.Dir, "Season 1", "E01.mkv")); err == nil {
		t.Fatal("E01 was downloaded too")
	}
}
