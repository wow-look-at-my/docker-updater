package main

import (
	"context"
	"fmt"
	"log"
)

// composeManaged reports whether a container was created by docker compose and
// carries enough project metadata to converge it through the compose CLI.
// Containers started with plain `docker run` have none of these labels.
func composeManaged(info ContainerInfo) bool {
	return info.ComposeService != "" && len(splitConfigFiles(info.ComposeConfigFiles)) > 0
}

// recreateViaCompose converges a compose-managed service onto the freshly
// pulled image with `docker compose up -d --no-deps`, so the recreate reads the
// compose file instead of replaying the old container's stored config.
//
// This is the difference between docker-updater and image-swapping updaters:
// cloning a container's HostConfig preserves whatever it was created with, so a
// compose file that gained a mount or an env var never takes effect and an
// image built to require it fails on every update. Letting compose own the
// container definition makes the file authoritative.
//
// --no-deps keeps the convergence scoped to the one service. Recreating a
// dependency (a docker-in-docker daemon holding live containers, a database)
// merely because a sibling service took a new image would destroy state the
// update never intended to touch.
//
// The rollback contract of the raw-API path is preserved: the previous image is
// pinned by content ID before compose tears anything down, and any failure
// afterwards re-tags that image and converges the service back onto it, so a
// failed update never leaves the service down.
func recreateViaCompose(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo) error {
	configFiles := splitConfigFiles(info.ComposeConfigFiles)
	if len(configFiles) == 0 || info.ComposeService == "" {
		return fmt.Errorf("container %s: not compose-managed", info.Name)
	}

	// Pin the running image by content ID before compose replaces the
	// container: the pull already re-pointed the tag at the new image, so the
	// tag alone cannot bring the old version back.
	inspect, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
	}
	oldImageID := inspect.Image
	oldImageRef := ""
	if inspect.Config != nil {
		oldImageRef = inspect.Config.Image
	}

	rollback := func(cause error) error {
		// Cancel-proof: a shutdown mid-update must not strand the service.
		rctx := context.WithoutCancel(ctx)
		if oldImageID == "" {
			return fmt.Errorf("%w; ROLLBACK FAILED, %s may be down: previous image unknown", cause, info.Name)
		}
		// Re-tag rather than pinning the bare ID: compose resolves the
		// service's image reference from the file, and a digest-pinned or
		// ID-only reference would never see updates again.
		if hasRepository(oldImageRef) {
			if err := cli.ImageTag(rctx, oldImageID, oldImageRef); err != nil {
				return fmt.Errorf("%w; ROLLBACK FAILED, %s may be down: re-tagging %s as %s: %v",
					cause, info.Name, shortID(oldImageID), oldImageRef, err)
			}
		}
		if err := runner.UpNoDeps(rctx, configFiles, info.ComposeWorkingDir, info.ComposeService); err != nil {
			return fmt.Errorf("%w; ROLLBACK FAILED, %s may be down: compose up: %v", cause, info.Name, err)
		}
		log.Printf("container %s: rolled back to previous image %s", info.Name, shortID(oldImageID))
		return fmt.Errorf("%w; rolled back to previous image %s", cause, shortID(oldImageID))
	}

	log.Printf("container %s: recreating compose service %s (image %s)", info.Name, info.ComposeService, info.Image)
	if err := runner.UpNoDeps(ctx, configFiles, info.ComposeWorkingDir, info.ComposeService); err != nil {
		cause := fmt.Errorf("recreating service %s: %w", info.ComposeService, err)
		// Compose that failed before replacing anything -- an unreadable
		// compose file, an invalid config -- leaves the original container
		// running on the original image. There is nothing to roll back, and
		// rolling back anyway re-runs the same failing compose command, so the
		// error reads "ROLLBACK FAILED, <service> may be down" about a service
		// that never went anywhere. Report the real cause instead.
		if containerUnchanged(ctx, cli, info.ID, oldImageID) {
			log.Printf("container %s: compose failed before touching the container; still running on %s", info.Name, shortID(oldImageID))
			return cause
		}
		return rollback(cause)
	}

	// Compose replaced the container, so the original ID is gone; re-find the
	// service's current container before gating on health.
	newID := currentServiceContainerID(ctx, cli, info)
	if err := waitPostUpdateHealthy(ctx, cli, newID, info); err != nil {
		log.Printf("container %s: post-update health check failed (%s), rolling back: %v", info.Name, shortID(newID), err)
		return rollback(fmt.Errorf("container %s not healthy after update: %w", info.Name, err))
	}

	log.Printf("container %s updated and healthy via compose (%s)", info.Name, shortID(newID))
	return nil
}

// containerUnchanged reports whether the pre-update container is still there,
// still running, and still on the image it started with — i.e. the failed
// compose invocation was a no-op. Anything it cannot confirm (an inspect
// error, a gone container, a stopped one) reads as changed, so an uncertain
// state still gets the rollback.
func containerUnchanged(ctx context.Context, cli DockerClient, containerID, imageID string) bool {
	if containerID == "" || imageID == "" {
		return false
	}
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return false
	}
	return inspect.State != nil && inspect.State.Running && inspect.Image == imageID
}
