package main

import (
	"context"
	"log"
	"os"
	"time"
)

// runUpdateCheck checks all monitored containers for updates and applies them.
func runUpdateCheck(ctx context.Context, cli DockerClient, cfg Config) []UpdateResult {
	containers, err := listMonitoredContainers(ctx, cli, cfg.Label)
	if err != nil {
		log.Printf("error listing containers: %v", err)
		return nil
	}

	if len(containers) == 0 {
		log.Println("no monitored containers found")
		return nil
	}

	log.Printf("checking %d monitored container(s)", len(containers))

	var results []UpdateResult

	for _, info := range containers {
		result := UpdateResult{
			Container: info,
			CheckedAt: time.Now(),
			DryRun:    cfg.DryRun,
		}

		switch info.Mode {
		case UpdateModeImage:
			result = checkAndUpdateImage(ctx, cli, info, cfg, result)
		case UpdateModeGit:
			result = checkAndUpdateGit(ctx, cli, info, cfg, result)
		default:
			log.Printf("container %s: unknown mode %q, skipping", info.Name, info.Mode)
			continue
		}

		results = append(results, result)
	}

	return results
}

func checkAndUpdateImage(ctx context.Context, cli DockerClient, info ContainerInfo, cfg Config, result UpdateResult) UpdateResult {
	result.OldRef = info.ImageDigest

	newDigest, err := checkImageUpdate(ctx, cli, info)
	if err != nil {
		result.Error = err
		log.Printf("container %s: error checking image: %v", info.Name, err)
		return result
	}

	if newDigest == "" {
		log.Printf("container %s: image up-to-date", info.Name)
		return result
	}

	result.NewRef = newDigest
	log.Printf("container %s: image update available (%s -> %s)", info.Name, shortID(info.ImageDigest), shortID(newDigest))

	if info.PreCheck != PreCheckNone {
		if err := runPreCheck(ctx, cli, info); err != nil {
			result.Skipped = true
			result.SkipReason = err.Error()
			log.Printf("container %s: pre-check failed, skipping update: %v", info.Name, err)
			return result
		}
	}

	if cfg.DryRun {
		log.Printf("container %s: dry-run mode, skipping update", info.Name)
		result.Updated = true
		return result
	}

	if err := recreateContainer(ctx, cli, info, info.Image); err != nil {
		result.Error = err
		log.Printf("container %s: error updating: %v", info.Name, err)
		return result
	}

	result.Updated = true
	return result
}

func checkAndUpdateGit(ctx context.Context, cli DockerClient, info ContainerInfo, cfg Config, result UpdateResult) UpdateResult {
	newSHA, err := checkGitUpdate(info)
	if err != nil {
		result.Error = err
		log.Printf("container %s: error checking git: %v", info.Name, err)
		return result
	}

	if newSHA == "" {
		log.Printf("container %s: git ref up-to-date", info.Name)
		return result
	}

	result.NewRef = newSHA
	log.Printf("container %s: git update detected (new SHA: %s)", info.Name, shortID(newSHA))

	if info.PreCheck != PreCheckNone {
		if err := runPreCheck(ctx, cli, info); err != nil {
			result.Skipped = true
			result.SkipReason = err.Error()
			log.Printf("container %s: pre-check failed, skipping update: %v", info.Name, err)
			return result
		}
	}

	if cfg.DryRun {
		log.Printf("container %s: dry-run mode, skipping update", info.Name)
		result.Updated = true
		return result
	}

	// For git mode, we re-pull the image (assumes image is built from the repo
	// and tagged accordingly, e.g. via CI) then recreate.
	if err := recreateContainer(ctx, cli, info, info.Image); err != nil {
		result.Error = err
		log.Printf("container %s: error updating: %v", info.Name, err)
		return result
	}

	result.Updated = true
	return result
}

// runLoop runs the main update loop until a signal is received.
func runLoop(ctx context.Context, cli DockerClient, cfg Config, sigCh <-chan os.Signal) {
	log.Printf("starting docker-updater (interval=%s, label=%s, dry_run=%v)", cfg.Interval, cfg.Label, cfg.DryRun)

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

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
