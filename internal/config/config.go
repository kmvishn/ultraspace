// Package config loads the ultraspace TOML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Listen is the HTTP address. Keep it on loopback and put a TLS proxy in front.
	Listen string `toml:"listen"`
	// DataDir holds the database and the setup token.
	DataDir string `toml:"data_dir"`
	// FilesRoot is the directory the file manager is confined to.
	FilesRoot string `toml:"files_root"`
	// PublicOrigin is the external URL (e.g. https://example.duckdns.org), accepted in Origin checks.
	PublicOrigin string `toml:"public_origin"`
	// TrustProxy trusts X-Forwarded-Proto / X-Real-IP from a loopback peer.
	TrustProxy bool     `toml:"trust_proxy"`
	SessionTTL Duration `toml:"session_ttl"`
	Browser    Browser  `toml:"browser"`
	// MemoryProfile sizes the browser for the host: "auto" (from total RAM), "tiny"
	// (512 MB hosts), "small" (about 1 GB) or "normal".
	MemoryProfile string `toml:"memory_profile"`
	// AgentSocket is where the Terminal and VS Code service (ultraspace agent) listens.
	AgentSocket string  `toml:"agent_socket"`
	Torrent     Torrent `toml:"torrent"`
}

// Torrent configures the BitTorrent client behind the Torrents and Stremio apps.
type Torrent struct {
	// Disabled turns the torrent client off (the apps then say so).
	Disabled bool `toml:"disabled"`
	// Folder is where downloads go, relative to files_root (default "Torrents").
	Folder string `toml:"folder"`
	// Port is the incoming peer port (open it in the firewall for better speeds); 0 = random.
	Port int `toml:"port"`
	// SeedRatio stops uploading a finished torrent at this upload/size ratio.
	// 0 = do not seed at all, -1 = seed for as long as ultraspace runs.
	SeedRatio float64 `toml:"seed_ratio"`
}

// Browser configures the remote Chrome behind the Browser app.
type Browser struct {
	// Chrome is the chrome-headless-shell binary. If it is missing, the Browser app
	// falls back to showing pages directly in the visitor's own browser.
	Chrome string `toml:"chrome"`
	// Idle is how long Chrome keeps running with no window connected (0 = from the memory profile).
	Idle Duration `toml:"idle"`
	// NoSandbox turns off Chrome's own sandbox. Only for hosts where it cannot run.
	NoSandbox bool `toml:"no_sandbox"`
	// Quality is the JPEG quality of the streamed screen (1-100).
	Quality int `toml:"quality"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func Default() Config {
	return Config{
		Listen:        "127.0.0.1:4600",
		DataDir:       "/var/lib/ultraspace",
		SessionTTL:    Duration{30 * 24 * time.Hour},
		AgentSocket:   "/run/ultraspace-agent.sock",
		MemoryProfile: "auto",
		Browser: Browser{
			Chrome:  "/opt/ultraspace/chrome/chrome-headless-shell",
			Quality: 70,
		},
		Torrent: Torrent{Folder: "Torrents", Port: 42069, SeedRatio: 1},
	}
}

// Load reads path over the defaults. A missing file is not an error.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	if _, err := toml.DecodeFile(path, &c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Finalize fills derived defaults and validates. Call it after applying flag overrides.
func (c *Config) Finalize() error {
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if c.FilesRoot == "" {
		c.FilesRoot = filepath.Join(c.DataDir, "files")
	}
	if c.SessionTTL.Duration < time.Minute {
		return errors.New("session_ttl must be at least 1m")
	}
	if c.Torrent.Folder == "" || !filepath.IsLocal(c.Torrent.Folder) {
		return errors.New("torrent.folder must be a folder inside files_root")
	}
	if c.Torrent.Port < 0 || c.Torrent.Port > 65535 {
		return errors.New("torrent.port must be 0-65535")
	}
	return nil
}
