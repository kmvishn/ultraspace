// Command ultraspace is a personal server dashboard in a single binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ultraspace/internal/agent"
	"ultraspace/internal/config"
	"ultraspace/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		runAgent(os.Args[2:])
		return
	}
	configPath := flag.String("config", "/etc/ultraspace/config.toml", "path to the config file (missing file = defaults)")
	listen := flag.String("listen", "", "override the listen address")
	dataDir := flag.String("data", "", "override the data directory")
	debug := flag.Bool("debug", false, "log every request")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ultraspace", version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if err := cfg.Finalize(); err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, cfg, version, log); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

// runAgent is "ultraspace agent": the Terminal and VS Code backend, run as a login user.
func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	socket := fs.String("socket", "", "Unix socket to listen on when not started by systemd")
	code := fs.String("code-server", "/opt/ultraspace/code-server/bin/code-server", "code-server binary for VS Code")
	idle := fs.Duration("code-idle", 20*time.Minute, "stop VS Code after this long unused")
	debug := fs.Bool("debug", false, "verbose logging")
	_ = fs.Parse(args)
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx, agent.Options{Socket: *socket, CodeServer: *code, CodeIdle: *idle}, log); err != nil {
		log.Error("agent", "err", err)
		os.Exit(1)
	}
}
