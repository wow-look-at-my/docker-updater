package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[docker-updater] ")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	cli, err := newDockerClient()
	if err != nil {
		log.Fatalf("failed to create Docker client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	info, err := cli.Info(ctx)
	if err != nil {
		log.Fatalf("failed to connect to Docker: %v", err)
	}
	log.Printf("connected to Docker %s (%s)", info.ServerVersion, info.Name)

	var resolveAuth AuthResolver
	if cfg.ConfigPath != "" {
		dockerCfg, err := loadDockerConfig(cfg.ConfigPath)
		if err != nil {
			log.Fatalf("failed to load docker config: %v", err)
		}
		log.Printf("loaded registry credentials for %d registries", len(dockerCfg.Auths))
		resolveAuth = newAuthResolver(dockerCfg)
	} else {
		resolveAuth = newAuthResolver(nil)
	}

	store := newStore()

	// Inbound GitHub webhook (opt-in): a valid delivery requests an immediate
	// check via this channel. Buffered with depth 1 and filled non-blockingly,
	// so a burst of deliveries coalesces into at most one pending check. nil
	// when disabled, in which case runLoop simply never selects on it.
	var trigger chan struct{}
	if cfg.GitHubWebhookAddr != "" {
		trigger = make(chan struct{}, 1)
		go newGitHubWebhookServer(cfg.GitHubWebhookAddr, cfg.GitHubWebhookSecret, cfg.GitHubWebhookPackages, trigger).run(ctx)
	} else {
		log.Print("github webhook disabled (DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR is empty)")
	}

	if cfg.DashboardAddr != "" {
		go newDashboardServer(cli, cfg, store).run(ctx)
	} else {
		log.Print("dashboard disabled (DOCKER_UPDATER_DASHBOARD_ADDR is empty)")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	runLoop(ctx, cli, cfg, sigCh, resolveAuth, store, trigger)
}
