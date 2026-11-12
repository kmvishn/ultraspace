package server

import "testing"

func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.12.0", "v0.13.0", true},
		{"v0.12.0-18-g344d6eb", "v0.12.1", true},
		{"v0.12.1", "v0.12.0", false},
		{"v0.12.0", "v0.12.0", false},
		{"dev", "v1.0.0", false},
		{"v0.12.0", "", false},
	}
	for _, c := range cases {
		if got := updateAvailable(c.current, c.latest); got != c.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}
