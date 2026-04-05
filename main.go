package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[docker-updater] ")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	log.Printf("starting docker-updater (interval=%s, label=%s, dry_run=%v)", cfg.Interval, cfg.Label, cfg.DryRun)

	cli := newDockerClient()
	defer cli.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Verify Docker connectivity.
	info, err := cli.info(ctx)
	if err != nil {
		log.Fatalf("failed to connect to Docker: %v", err)
	}
	log.Printf("connected to Docker %s (%s)", info.ServerVersion, info.Name)

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Run first check immediately.
	results := runUpdateCheck(ctx, cli, cfg)
	sendWebhookNotifications(cfg, results)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			results := runUpdateCheck(ctx, cli, cfg)
			sendWebhookNotifications(cfg, results)
		case sig := <-sigCh:
			log.Printf("received signal %v, shutting down", sig)
			return
		}
	}
}
