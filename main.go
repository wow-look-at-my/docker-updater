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

	// Hidden subcommand: the detached helper a self-update spawns runs
	// `/docker-updater finish-self-update ...` to recreate the updater's own
	// container on the new image. It is a one-shot and never starts the loop.
	if len(os.Args) > 1 && os.Args[1] == finishSelfUpdateCommand {
		finishSelfUpdate(os.Args[2:])
		return
	}

	log.Printf("docker-updater %s starting", buildVersion())

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

	// Determine our own container ID. Two things need it: a self-update can't
	// recreate its own container inline (that would kill this process
	// mid-swap), so updateContainer routes it through a detached helper; and
	// attaching to a monitored container's network means naming the container
	// to connect. An explicit DOCKER_UPDATER_CONTAINER_ID wins; otherwise we
	// auto-detect.
	if cfg.SelfContainerID == "" {
		cfg.SelfContainerID = detectOwnContainerID()
	}
	if cfg.SelfContainerID != "" {
		log.Printf("self-update and network self-attach enabled (own container %s)", shortID(cfg.SelfContainerID))
	} else {
		log.Print("self-update and network self-attach disabled: could not determine own container ID " +
			"(set DOCKER_UPDATER_CONTAINER_ID to enable); update checks only reach containers on a network docker-updater already shares")
	}

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
