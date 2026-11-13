package youtube

import "testing"

const searchFixture = `{
  "contents": {"twoColumnSearchResultsRenderer": {"primaryContents": {"sectionListRenderer": {"contents": [
    {"itemSectionRenderer": {"contents": [
      {"channelRenderer": {"channelId": "UC123"}},
      {"videoRenderer": {
        "videoId": "abc123XYZ_-",
        "title": {"runs": [{"text": "Lo-fi "}, {"text": "Beats"}]},
        "lengthText": {"simpleText": "3:42"},
        "viewCountText": {"simpleText": "1,234 views"},
        "publishedTimeText": {"simpleText": "2 days ago"},
        "shortBylineText": {"runs": [{"text": "Chill Channel", "navigationEndpoint": {"browseEndpoint": {"browseId": "UCabc"}}}]}
      }},
      {"videoRenderer": {
        "videoId": "abc123XYZ_-",
        "title": {"runs": [{"text": "Duplicate"}]},
        "shortBylineText": {"runs": [{"text": "Chill Channel"}]}
      }},
      {"videoRenderer": {
        "videoId": "live1234567",
        "title": {"runs": [{"text": "Radio Live"}]},
        "shortBylineText": {"runs": [{"text": "Radio"}]},
        "thumbnailOverlays": [{"thumbnailOverlayTimeStatusRenderer": {"style": "LIVE"}}]
      }}
    ]}}
  ]}}}}
}`

func TestParseSearch(t *testing.T) {
	got := parseSearch([]byte(searchFixture))
	if len(got) != 2 {
		t.Fatalf("results = %+v", got)
	}
	first := got[0]
	if first.Title != "Lo-fi Beats" || first.Channel != "Chill Channel" || first.ChannelID != "UCabc" ||
		first.Duration != "3:42" || first.Views != "1,234 views" || first.Live {
		t.Fatalf("first = %+v", first)
	}
	if !got[1].Live {
		t.Fatalf("live not detected: %+v", got[1])
	}
}

const playerFixture = `{
  "playabilityStatus": {"status": "OK"},
  "videoDetails": {"title": "Song", "author": "Artist", "channelId": "UCx",
    "shortDescription": "desc", "lengthSeconds": "213", "viewCount": "999"},
  "streamingData": {
    "formats": [
      {"itag": 18, "mimeType": "video/mp4; codecs=\"avc1, mp4a\"", "url": "https://gv/18", "bitrate": 500000, "width": 640, "height": 360}
    ],
    "adaptiveFormats": [
      {"itag": 137, "mimeType": "video/mp4; codecs=\"avc1\"", "url": "https://gv/137", "bitrate": 4000000, "width": 1920, "height": 1080},
      {"itag": 140, "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "url": "https://gv/140", "bitrate": 130000},
      {"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "url": "https://gv/251", "bitrate": 160000},
      {"itag": 250, "mimeType": "audio/webm; codecs=\"opus\"", "signatureCipher": "s=abc"}
    ]
  }
}`

func TestParsePlayerPicksPlayableFormats(t *testing.T) {
	info, err := parsePlayer([]byte(playerFixture), "abc123XYZ_-")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Song" || info.DurationSec != 213 || len(info.Formats) != 4 {
		t.Fatalf("info = %+v", info)
	}
	if v := pickVideo(info.Formats); v == nil || v.Itag != 18 {
		t.Fatalf("video = %+v", v)
	}
	if a := pickAudio(info.Formats); a == nil || a.Itag != 140 {
		t.Fatalf("audio = %+v (AAC in MP4 should win over Opus)", a)
	}
}

const lockupFixture = `{"contents": [{"lockupViewModel": {"contentId": "d3QQHqRmvH4", "contentType": "LOCKUP_CONTENT_TYPE_VIDEO", "contentImage": {"thumbnailViewModel": {"overlays": [{"thumbnailBottomOverlayViewModel": {"badges": [{"thumbnailBadgeViewModel": {"text": "1:13"}}]}}]}}, "metadata": {"lockupMetadataViewModel": {"title": {"content": "A New Format Video"}, "metadata": {"contentMetadataViewModel": {"metadataRows": [{"metadataParts": [{"text": {"content": "129K views"}}, {"text": {"content": "8 days ago"}}]}, {"metadataParts": [{"text": {"content": "Some Channel"}}]}]}}}}}}, {"lockupViewModel": {"contentId": "PLxxxxxxxxxxxx", "contentType": "LOCKUP_CONTENT_TYPE_PLAYLIST", "metadata": {"lockupMetadataViewModel": {"title": {"content": "A Playlist"}}}}}]}`

func TestParseLockups(t *testing.T) {
	got := parseSearch([]byte(lockupFixture))
	if len(got) != 2 {
		t.Fatalf("results = %+v (want one video and one playlist)", got)
	}
	r := got[0]
	if r.Kind != "video" || r.Title != "A New Format Video" || r.Duration != "1:13" || r.Views != "129K views" ||
		r.Published != "8 days ago" || r.Channel != "Some Channel" {
		t.Fatalf("result = %+v", r)
	}
	p := got[1]
	if p.Kind != "playlist" || p.ID != "PLxxxxxxxxxxxx" || p.Title != "A Playlist" {
		t.Fatalf("playlist = %+v", p)
	}
}

const shortsFixture = `{"contents": [{"shortsLockupViewModel": {
  "entityId": "shorts-shelf-item-abc123XYZ_-",
  "accessibilityText": "Funny Cat Moments, 1.2 million views - play Short",
  "onTap": {"innertubeCommand": {"reelWatchEndpoint": {"videoId": "abc123XYZ_-"}}}
}}]}`

func TestParseShorts(t *testing.T) {
	got := parseSearch([]byte(shortsFixture))
	if len(got) != 1 {
		t.Fatalf("results = %+v", got)
	}
	r := got[0]
	if r.Kind != "short" || r.ID != "abc123XYZ_-" || r.Title != "Funny Cat Moments" || r.Views != "1.2 million views" {
		t.Fatalf("short = %+v", r)
	}
}

func TestPlaylistIDFrom(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/playlist?list=PLabc123_-XYZ":       "PLabc123_-XYZ",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc123": "PLabc123",
		"PLabc123_-XYZ":            "PLabc123_-XYZ",
		"UCSJ4gkVC6NrvII8umztf0Ow": "UCSJ4gkVC6NrvII8umztf0Ow",
	} {
		if got, ok := playlistIDFrom(in); !ok || got != want {
			t.Fatalf("playlistIDFrom(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := playlistIDFrom("https://example.com/nope"); ok {
		t.Fatal("accepted a non-playlist")
	}
}

func TestQualityAndDownloadName(t *testing.T) {
	info, err := parsePlayer([]byte(playerFixture), "abc123XYZ_-")
	if err != nil {
		t.Fatal(err)
	}
	if v := pickVideoUpTo(info.Formats, 360); v == nil || v.Itag != 18 {
		t.Fatalf("360p pick = %+v", v)
	}
	if v := pickVideoUpTo(info.Formats, 144); v != nil {
		t.Fatalf("144p pick should be empty, got %+v", v)
	}
	if hs := muxedHeights(info.Formats); len(hs) != 1 || hs[0] != 360 {
		t.Fatalf("heights = %v", hs)
	}
	if got := downloadName("A/B: Test?", "Chan|nel"); got != "A_B_ Test_ - Chan_nel" {
		t.Fatalf("downloadName = %q", got)
	}
	if got := downloadName("", ""); got != "youtube-video" {
		t.Fatalf("empty downloadName = %q", got)
	}
}

func TestCleanRegion(t *testing.T) {
	if cleanRegion("in") != "IN" || cleanRegion("XX1") != "US" || cleanRegion("") != "US" {
		t.Fatal("region cleaning wrong")
	}
}

func TestParsePlayerRefusesUnplayable(t *testing.T) {
	_, err := parsePlayer([]byte(`{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in"}}`), "abc123XYZ_-")
	if err == nil {
		t.Fatal("accepted an unplayable response")
	}
}
