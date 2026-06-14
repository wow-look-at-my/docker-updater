package main

import (
	"context"
	"log"
	"os"
	"time"
)

// runUpdateCheck checks all monitored containers for updates and applies them.
func runUpdateCheck(ctx context.Context, cli DockerClient, cfg Config, resolveAuth AuthResolver) []UpdateResult {
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
			result = checkAndUpdateImage(ctx, cli, info, cfg, result, resolveAuth)
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

func checkAndUpdateImage(ctx context.Context, cli DockerClient, info ContainerInfo, cfg Config, result UpdateResult, resolveAuth AuthResolver) UpdateResult {
	result.OldRef = info.ImageDigest

	newDigest, fetched, err := checkImageUpdate(ctx, cli, info, resolveAuth)
	if err != nil {
		result.Error = err
		log.Printf("container %s: error checking image: %v", info.Name, err)
		return result
	}

	// Pulled reflects whether the pull actually fetched a newer image, not merely
	// that a pull check ran. An up-to-date check downloads nothing and must not
	// reset the "last pulled" timestamp.
	result.Pulled = fetched

	if newDigest == "" {
		log.Printf("container %s: image up-to-date", info.Name)
		return result
	}

	result.NewRef = newDigest
	log.Printf("container %s: image update available (%s -> %s)", info.Name, shortID(info.ImageDigest), shortID(newDigest))

	if !info.Rolling {
		if info.PreCheckURL != "" || info.PreCheckCommand != "" {
			if err := runPreCheck(ctx, cli, info); err != nil {
				result.Skipped = true
				result.SkipReason = err.Error()
				log.Printf("container %s: pre-check failed, skipping update: %v", info.Name, err)
				return result
			}
		}
	}

	if cfg.DryRun {
		log.Printf("container %s: dry-run mode, skipping update", info.Name)
		result.Updated = true
		return result
	}

	if err := updateContainer(ctx, cli, info, cfg); err != nil {
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

	if !info.Rolling {
		if info.PreCheckURL != "" || info.PreCheckCommand != "" {
			if err := runPreCheck(ctx, cli, info); err != nil {
				result.Skipped = true
				result.SkipReason = err.Error()
				log.Printf("container %s: pre-check failed, skipping update: %v", info.Name, err)
				return result
			}
		}
	}

	if cfg.DryRun {
		log.Printf("container %s: dry-run mode, skipping update", info.Name)
		result.Updated = true
		return result
	}

	if err := updateContainer(ctx, cli, info, cfg); err != nil {
		result.Error = err
		log.Printf("container %s: error updating: %v", info.Name, err)
		return result
	}

	result.Updated = true
	return result
}

func updateContainer(ctx context.Context, cli DockerClient, info ContainerInfo, cfg Config) error {
	// Updating our own container can't be done inline -- stopping it would kill
	// this process before the replacement is created. Hand the swap to a
	// detached helper instead.
	if sameContainer(info.ID, cfg.SelfContainerID) {
		return selfUpdate(ctx, cli, info, info.Image)
	}
	if info.Rolling {
		return rollingUpdateContainer(ctx, cli, info, info.Image)
	}
	return recreateContainer(ctx, cli, info, info.Image)
}

// runLoop runs the main update loop until a signal is received. After every
// cycle it records results into the store so the dashboard reflects the
// updater's latest knowledge.
//
// trigger lets an external source (the GitHub webhook) request an immediate
// check between ticks. It may be nil, in which case only the interval drives
// checks.
func runLoop(ctx context.Context, cli DockerClient, cfg Config, sigCh <-chan os.Signal, resolveAuth AuthResolver, store *Store, trigger <-chan struct{}) {
	log.Printf("starting docker-updater (interval=%s, label=%s, dry_run=%v)", cfg.Interval, cfg.Label, cfg.DryRun)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// runCheck runs one full cycle and records/notifies. Shared by the first
	// run, the interval tick, and the webhook trigger so all three behave
	// identically.
	runCheck := func() {
		results := runUpdateCheck(ctx, cli, cfg, resolveAuth)
		store.Record(results, time.Now())
		sendWebhookNotifications(cfg, results)
	}

	// Run first check immediately.
	runCheck()

	for {
		select {
		case <-ticker.C:
			runCheck()
		case <-trigger:
			log.Print("webhook trigger received, running update check now")
			runCheck()
			// Realign the interval so the next scheduled check is a full
			// interval after this one, rather than firing right on its heels.
			ticker.Reset(cfg.Interval)
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
