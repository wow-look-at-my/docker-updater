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

	// Join the networks of the containers being checked before probing them:
	// discovery dials each container's own IP, which is only routable from a
	// network they share.
	attacher := newSelfAttacher(ctx, cli, cfg.SelfContainerID)

	var results []UpdateResult

	for _, info := range containers {
		var warnings []string
		if w := attacher.ensure(ctx, info); w != "" {
			warnings = append(warnings, w)
		}

		// Discover the standard /.well-known/docker-updater/ endpoints before
		// the mode switch: they fill in the pre-update gate and the
		// post-update liveness check for every container that serves them, and
		// produce the warnings the dashboard shows for those that do not.
		info, wellKnownWarnings := applyWellKnown(ctx, info)
		warnings = append(warnings, wellKnownWarnings...)
		logContainerWarnings(info.Name, warnings)

		result := UpdateResult{
			Container: info,
			CheckedAt: time.Now(),
			DryRun:    cfg.DryRun,
			Warnings:  warnings,
		}

		switch info.Mode {
		case UpdateModeImage:
			result = checkAndUpdateImage(ctx, cli, defaultComposeRunner, info, cfg, result, resolveAuth)
		case UpdateModeGit:
			result = checkAndUpdateGit(ctx, cli, defaultComposeRunner, info, cfg, result)
		case UpdateModeBuild:
			result = checkAndUpdateBuild(ctx, cli, defaultComposeRunner, info, cfg, result, resolveAuth)
		default:
			log.Printf("container %s: unknown mode %q, skipping", info.Name, info.Mode)
			continue
		}

		results = append(results, result)
	}

	return results
}

func checkAndUpdateImage(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, cfg Config, result UpdateResult, resolveAuth AuthResolver) UpdateResult {
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

	if err := updateContainer(ctx, cli, runner, info, cfg); err != nil {
		result.Error = err
		log.Printf("container %s: error updating: %v", info.Name, err)
		return result
	}

	result.Updated = true
	return result
}

func checkAndUpdateGit(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, cfg Config, result UpdateResult) UpdateResult {
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

	if err := updateContainer(ctx, cli, runner, info, cfg); err != nil {
		result.Error = err
		log.Printf("container %s: error updating: %v", info.Name, err)
		return result
	}

	result.Updated = true
	return result
}

// checkAndUpdateBuild handles a build-mode container: a locally-built (compose
// `build:`) service whose local derived tag has no registry origin. It watches
// the service's BASE image and, when the base digest advances, rebuilds and
// recreates the service via docker compose. It never pulls the local derived
// tag.
func checkAndUpdateBuild(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, cfg Config, result UpdateResult, resolveAuth AuthResolver) UpdateResult {
	if info.BaseImage == "" {
		// Unresolvable base: skip without erroring the loop. listMonitoredContainers
		// already logged once at resolution; report a one-line skip here too.
		result.Skipped = true
		result.SkipReason = "no base image to watch (set docker-updater.base-image or a parseable Dockerfile FROM)"
		log.Printf("container %s: build mode but no base image to watch; use docker-updater.base-image (skipping)", info.Name)
		return result
	}

	oldBase, newBase, verified, err := checkBuildUpdate(ctx, cli, info, resolveAuth)
	if err != nil {
		result.Error = err
		log.Printf("container %s: error checking base image: %v", info.Name, err)
		return result
	}

	result.OldRef = oldBase

	if newBase == "" {
		if verified {
			log.Printf("container %s: image up to date with base %s (base layers verified)", info.Name, shortID(oldBase))
		} else {
			log.Printf("container %s: base image up-to-date (digest match)", info.Name)
		}
		return result
	}

	result.NewRef = newBase
	if verified {
		from := shortID(oldBase)
		if from == "" {
			from = "unknown"
		}
		log.Printf("container %s: image built from stale base; rebuilding (%s -> %s)", info.Name, from, shortID(newBase))
	} else {
		log.Printf("container %s: base image update detected (%s -> %s)", info.Name, shortID(oldBase), shortID(newBase))
	}

	if !info.Rolling {
		if info.PreCheckURL != "" || info.PreCheckCommand != "" {
			if err := runPreCheck(ctx, cli, info); err != nil {
				result.Skipped = true
				result.SkipReason = err.Error()
				log.Printf("container %s: pre-check failed, skipping rebuild: %v", info.Name, err)
				return result
			}
		}
	}

	if cfg.DryRun {
		// Dry-run must mutate nothing: no compose build, no recreate, and the
		// recorded "built-from" base must not advance, so the same update keeps
		// showing as available until it is really applied.
		log.Printf("container %s: dry-run mode, would rebuild service %s on new base %s (no changes made)", info.Name, info.ComposeService, shortID(newBase))
		result.Updated = true
		return result
	}

	changed, err := rebuildAndRecreate(ctx, cli, runner, info, newBase, verified)
	if err != nil {
		result.Error = err
		log.Printf("container %s: error rebuilding: %v", info.Name, err)
		return result
	}

	// A cache-hit rebuild (identical image ID) is not a churn-worthy update, but
	// the base did advance, so still report that the check applied the new base.
	result.Updated = true
	_ = changed
	return result
}

func updateContainer(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, cfg Config) error {
	// Updating our own container can't be done inline -- stopping it would kill
	// this process before the replacement is created. Hand the swap to a
	// detached helper instead.
	if sameContainer(info.ID, cfg.SelfContainerID) {
		return selfUpdate(ctx, cli, info, info.Image)
	}
	if info.Rolling {
		// Rolling updates start the replacement before draining the old
		// container, which compose cannot express: `up -d` always stops the
		// existing container first. Zero downtime therefore keeps the raw-API
		// path, which carries the container definition over from the running
		// container rather than from the compose file.
		if composeManaged(info) {
			log.Printf("container %s: rolling update carries its config over from the running container; compose-file changes need a `docker compose up -d %s` to apply", info.Name, info.ComposeService)
		}
		return rollingUpdateContainer(ctx, cli, info, info.Image)
	}
	// A compose-managed service is converged through compose so the compose
	// file stays authoritative; only containers compose does not own fall back
	// to replaying the previous container's config.
	//
	// Image mode only: git mode recreates a container whose image and compose
	// config are both unchanged (the new commit arrives through a mount), and
	// `up -d` is a no-op when nothing drifted, so it would silently stop
	// applying git updates.
	if info.Mode == UpdateModeImage && composeManaged(info) {
		return recreateViaCompose(ctx, cli, runner, info)
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
		reportStuck(store.Snapshot(), time.Now())
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
