package services

import "testing"

func TestInspectPackages(t *testing.T) {
	got := inspectPackages(func(name string) (string, error) {
		if name == "mpv" || name == "nmcli" {
			return "/usr/bin/" + name, nil
		}
		return "", osErr
	}, func(path string) bool {
		return path == "/opt/ultraspace/chrome/chrome-headless-shell"
	}, false)
	want := map[string]bool{
		"mpv": true, "pulse": false, "networkmanager": true, "bluetooth": false, "chrome": true, "ffmpeg": false, "espeak": false,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for _, p := range got {
		if p.Present != want[p.ID] {
			t.Errorf("%s present = %v, detail %q", p.ID, p.Present, p.Detail)
		}
		if p.Present && p.Path == "" {
			t.Errorf("%s present without a path", p.ID)
		}
		if !p.Present && p.Detail == "" {
			t.Errorf("%s missing without an explanation", p.ID)
		}
	}
}

func TestInspectPackagesVPS(t *testing.T) {
	got := inspectPackages(func(name string) (string, error) { return "", osErr },
		func(path string) bool { return false }, true)
	if len(got) != 2 {
		t.Fatalf("vps packages = %v", got)
	}
	for _, p := range got {
		if p.ID != "chrome" && p.ID != "ffmpeg" {
			t.Fatalf("unexpected VPS package %s", p.ID)
		}
	}
}

type pathErr string

func (e pathErr) Error() string { return string(e) }

const osErr = pathErr("missing")
