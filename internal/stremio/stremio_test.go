package stremio

import (
	"encoding/json"
	"testing"
)

func TestManifestURL(t *testing.T) {
	for in, want := range map[string]string{
		"stremio://addon.example/abc/manifest.json": "https://addon.example/abc/manifest.json",
		"https://addon.example/abc":                 "https://addon.example/abc/manifest.json",
		"https://addon.example/manifest.json?x=1#y": "https://addon.example/manifest.json",
	} {
		u, err := manifestURL(in)
		if err != nil || u.String() != want {
			t.Errorf("manifestURL(%q) = %v %v, want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://user:pw@x/manifest.json", "ftp://x/manifest.json"} {
		if _, err := manifestURL(bad); err == nil {
			t.Errorf("manifestURL(%q) accepted", bad)
		}
	}
}

func TestMergeCollection(t *testing.T) {
	addon := func(url string, protected bool) remoteAddon {
		return remoteAddon{"transportUrl": url, "manifest": map[string]any{"id": url}, "flags": map[string]any{"official": protected, "protected": protected}}
	}
	coll := []remoteAddon{
		addon("https://v3-cinemeta.strem.io/manifest.json", true),
		addon("http://127.0.0.1:11470/local-addon/manifest.json", true),
		addon("https://a.example/manifest.json", false),
		addon("https://b.example/manifest.json", false),
	}
	m := json.RawMessage(`{"id":"new"}`)
	// The app removed b and Cinemeta, kept a, added c; order: c, a.
	next := mergeCollection(coll, []string{"https://c.example/manifest.json", "https://a.example/manifest.json"}, []json.RawMessage{m, m})
	var got []string
	for _, ad := range next {
		got = append(got, transport(ad))
	}
	want := []string{"https://c.example/manifest.json", "https://a.example/manifest.json",
		"https://v3-cinemeta.strem.io/manifest.json", "http://127.0.0.1:11470/local-addon/manifest.json"}
	if len(got) != len(want) {
		t.Fatalf("collection = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collection = %v, want %v", got, want)
		}
	}
	if next[1]["flags"].(map[string]any)["official"] != false || next[2]["flags"].(map[string]any)["protected"] != true {
		t.Fatal("flags of existing addons were not kept")
	}
}
