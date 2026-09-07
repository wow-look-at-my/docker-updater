package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// recreateContainer stops the old container, creates a new one with the same
// config but an updated image, and starts it. Once the old container has been
// removed, every failure (create, start, or the post-update health gate) rolls
// back to the previous image under the same name -- a failed update must never
// leave the service down.
func recreateContainer(ctx context.Context, cli DockerClient, info ContainerInfo, newImage string) error {
	// Inspect the existing container to capture its full config.
	inspect, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
	}

	// Pin the running image by content ID before tearing anything down: after
	// the pull, the original tag reference resolves to the new image, so the
	// tag alone cannot bring the old version back.
	oldImageID := inspect.Image
	oldImageRef := ""
	if inspect.Config != nil {
		oldImageRef = inspect.Config.Image
	}

	// Stop and remove the old container.
	log.Printf("stopping container %s (%s)", info.Name, shortID(info.ID))
	timeout := 30
	if err := cli.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping container %s: %w", info.Name, err)
	}
	if err := cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing container %s: %w", info.Name, err)
	}

	// Update the image in the config.
	config := inspect.Config
	config.Image = newImage
	clearInheritedDefaultsFor(ctx, cli, config, oldImageID, info.Name)

	hostConfig := inspect.HostConfig

	// Rebuild networking config from current state.
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{},
	}
	for netName, netSettings := range inspect.NetworkSettings.Networks {
		networkingConfig.EndpointsConfig[netName] = &network.EndpointSettings{
			Aliases: netSettings.Aliases,
		}
	}

	// The old container no longer exists past this point, so merely failing
	// the update would leave the service down. rollback removes the failed
	// replacement (if any), brings the previous image back up under the
	// original name, and folds the outcome into the returned error. It runs on
	// a cancel-proof context so a shutdown mid-update cannot strand the
	// service offline.
	rollback := func(failedID string, cause error) error {
		rctx := context.WithoutCancel(ctx)
		if oldImageID == "" {
			return fmt.Errorf("%w; ROLLBACK FAILED, %s is down: previous image unknown", cause, info.Name)
		}
		if failedID != "" {
			// Best-effort stop; the failed container may already have exited.
			cli.ContainerStop(rctx, failedID, container.StopOptions{})
			if err := cli.ContainerRemove(rctx, failedID, container.RemoveOptions{}); err != nil {
				return fmt.Errorf("%w; ROLLBACK FAILED, %s is down: removing failed container: %v", cause, info.Name, err)
			}
		}

		// Restore by re-tagging the previous image to the original reference
		// rather than creating from the bare image ID: a container whose
		// Config.Image is a bare ID resolves to a digest-pinned reference on
		// the next cycle and would never see updates again.
		restoreRef := oldImageID
		if hasRepository(oldImageRef) {
			if err := cli.ImageTag(rctx, oldImageID, oldImageRef); err != nil {
				log.Printf("container %s: re-tagging %s as %s failed, restoring by image ID: %v", info.Name, shortID(oldImageID), oldImageRef, err)
			} else {
				restoreRef = oldImageRef
			}
		}
		config.Image = restoreRef

		restored, err := cli.ContainerCreate(rctx, config, hostConfig, networkingConfig, nil, info.Name)
		if err != nil {
			return fmt.Errorf("%w; ROLLBACK FAILED, %s is down: recreating previous container: %v", cause, info.Name, err)
		}
		if err := cli.ContainerStart(rctx, restored.ID, container.StartOptions{}); err != nil {
			return fmt.Errorf("%w; ROLLBACK FAILED, %s is down: starting previous container: %v", cause, info.Name, err)
		}
		log.Printf("container %s: rolled back to previous image %s (%s)", info.Name, shortID(oldImageID), shortID(restored.ID))
		return fmt.Errorf("%w; rolled back to previous image %s", cause, shortID(oldImageID))
	}

	// Create and start the new container. failedID is carried out of the attempt
	// so a rollback can remove whatever the failed try left behind.
	log.Printf("creating new container %s with image %s", info.Name, newImage)
	var failedID string
	attempt := func() (string, error) {
		failedID = ""
		created, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, info.Name)
		if err != nil {
			return "", fmt.Errorf("creating container %s: %w", info.Name, err)
		}
		failedID = created.ID
		if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			return "", fmt.Errorf("starting container %s: %w", info.Name, err)
		}
		if err := waitPostUpdateHealthy(ctx, cli, created.ID, info); err != nil {
			return "", fmt.Errorf("container %s not healthy after update: %w", info.Name, err)
		}
		failedID = ""
		return created.ID, nil
	}

	newID, err := attempt()
	if err != nil {
		// The name is taken by the container that just failed, so it goes before
		// the retry can create its replacement under the same name.
		if failedID != "" {
			cli.ContainerStop(ctx, failedID, container.StopOptions{})
			cli.ContainerRemove(ctx, failedID, container.RemoveOptions{})
			failedID = ""
		}
		newID, err = retryOnImageDefaults(config, info.Name, err, attempt)
		if err != nil {
			return rollback(failedID, err)
		}
	}

	log.Printf("container %s updated and healthy (%s)", info.Name, shortID(newID))
	return nil
}

// retryOnImageDefaults runs attempt once more with the new image's own process
// fields in place of the ones cloned off the container being replaced.
//
// A cloned entrypoint the new image does not carry cannot exec, so the retry is
// the difference between a deployment that heals itself and one that rolls back
// on every cycle forever. It returns the original error untouched when there was
// nothing to strip, or when the retry fails too.
func retryOnImageDefaults(config *container.Config, name string, cause error, attempt func() (string, error)) (string, error) {
	if !resetToImageDefaults(config) {
		return "", cause
	}
	log.Printf("container %s: %v; retrying with the image's own entrypoint, command, user and healthcheck, because the config cloned from the container being replaced cannot start this image", name, cause)
	id, err := attempt()
	if err != nil {
		return "", fmt.Errorf("%w; a retry on the image's own defaults also failed: %v", cause, err)
	}
	log.Printf("container %s: started on the image's own defaults (%s). The config it inherited named something this image does not carry.", name, shortID(id))
	return id, nil
}

// aliasSettleDelay is how long the new container serves under the service
// aliases before the old one is stopped, so a proxy that caches DNS answers
// re-resolves and learns the new address while the old is still reachable.
// Overridden in tests.
var aliasSettleDelay = 5 * time.Second

// attachServiceAliases gives a running container the network aliases the
// container it replaces is serving under. Docker cannot add an alias to an
// existing endpoint, so each network is disconnected and reconnected -- safe
// precisely because this container carries no traffic yet: nothing resolves to
// it until this call lands.
func attachServiceAliases(ctx context.Context, cli DockerClient, containerID string, aliases map[string][]string) error {
	for netName, names := range aliases {
		if len(names) == 0 {
			continue
		}
		if err := cli.NetworkDisconnect(ctx, netName, containerID, false); err != nil {
			return fmt.Errorf("disconnecting %s from network %s: %w", shortID(containerID), netName, err)
		}
		if err := cli.NetworkConnect(ctx, netName, containerID, &network.EndpointSettings{Aliases: names}); err != nil {
			return fmt.Errorf("connecting %s to network %s: %w", shortID(containerID), netName, err)
		}
	}
	return nil
}

// rollingUpdateContainer starts a new container before stopping the old one,
// enabling zero-downtime updates when a reverse proxy routes by health.
func rollingUpdateContainer(ctx context.Context, cli DockerClient, info ContainerInfo, newImage string) error {
	inspect, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
	}

	config := inspect.Config
	config.Image = newImage
	clearInheritedDefaultsFor(ctx, cli, config, inspect.Image, info.Name)

	hostConfig := inspect.HostConfig
	hostConfig.PortBindings = nil

	// Network aliases are how a reverse proxy finds "the service", so they are
	// deliberately withheld until the new container is healthy. Copying them at
	// CREATE time puts two versions behind one alias for the whole health wait:
	// Docker's embedded DNS returns every IP an alias resolves to and rotates
	// them, so a proxy resolving that alias sends about half of all new
	// requests to the OLD image until it is stopped. With a container whose
	// HEALTHCHECK runs on a 30s interval that is tens of seconds of answering
	// from the version being replaced -- long enough for a client to get a
	// response shaped by the previous build, which is how a buildhost publish
	// came back missing a field its own release had added.
	serviceAliases := map[string][]string{}
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{},
	}
	for netName, netSettings := range inspect.NetworkSettings.Networks {
		serviceAliases[netName] = netSettings.Aliases
		networkingConfig.EndpointsConfig[netName] = &network.EndpointSettings{}
	}

	nextName := info.Name + "-next"
	log.Printf("container %s: starting rolling update with image %s", info.Name, newImage)

	attempt := func() (string, error) {
		created, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, nextName)
		if err != nil {
			return "", fmt.Errorf("creating next container %s: %w", nextName, err)
		}
		if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{})
			return "", fmt.Errorf("starting next container %s: %w", nextName, err)
		}
		if err := waitPostUpdateHealthy(ctx, cli, created.ID, info); err != nil {
			log.Printf("container %s: post-update health check failed for next container (%s): %v", info.Name, shortID(created.ID), err)
			cli.ContainerStop(ctx, created.ID, container.StopOptions{})
			cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{})
			return "", fmt.Errorf("next container %s not healthy: %w", nextName, err)
		}
		return created.ID, nil
	}

	nextID, err := attempt()
	if err != nil {
		nextID, err = retryOnImageDefaults(config, info.Name, err, attempt)
		if err != nil {
			return err
		}
	}

	log.Printf("container %s: next container healthy, moving service aliases", info.Name)
	if err := attachServiceAliases(ctx, cli, nextID, serviceAliases); err != nil {
		cli.ContainerStop(ctx, nextID, container.StopOptions{})
		cli.ContainerRemove(ctx, nextID, container.RemoveOptions{})
		return fmt.Errorf("attaching service aliases to %s: %w", nextName, err)
	}

	// Let a DNS-resolving proxy observe the new endpoint before the old one
	// stops answering. nginx and friends cache a resolved alias for a few
	// seconds; stopping the old container while a proxy still has only its
	// address cached turns the cutover into refused connections. The old
	// container keeps serving normally here -- it is the LAST few seconds in
	// which both versions can answer, down from the entire health wait.
	select {
	case <-time.After(aliasSettleDelay):
	case <-ctx.Done():
		return ctx.Err()
	}

	log.Printf("container %s: draining old", info.Name)
	timeout := 300
	if err := cli.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping old container %s: %w", info.Name, err)
	}
	if err := cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing old container %s: %w", info.Name, err)
	}

	if err := cli.ContainerRename(ctx, nextID, info.Name); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", nextName, info.Name, err)
	}

	log.Printf("container %s: rolling update complete (%s)", info.Name, shortID(nextID))
	return nil
}

// noHealthcheckGracePeriod is how long a container with no HEALTHCHECK (and no
// health-check labels) is observed after an update before being declared
// healthy. Long enough to catch crash-on-boot regressions, short enough not to
// stall the update cycle. Var so tests can shorten it.
var noHealthcheckGracePeriod = 15 * time.Second

// waitHealthy polls containerID until Docker reports it healthy. The deadline
// is derived from the container's own HEALTHCHECK config so callers don't
// need to know or duplicate the timing parameters. A container with no
// HEALTHCHECK at all is instead gated on staying up for
// noHealthcheckGracePeriod: failing such updates outright would make
// healthcheck-less containers permanently un-updatable.
func waitHealthy(ctx context.Context, cli DockerClient, containerID string) error {
	initial, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspecting container: %w", err)
	}
	if initial.State == nil || !initial.State.Running {
		return fmt.Errorf("container exited")
	}
	if initial.State.Health == nil {
		return waitStaysRunning(ctx, cli, containerID)
	}
	if initial.State.Health.Status == "healthy" {
		return nil
	}

	timeout := healthCheckTimeout(initial)
	deadline := time.After(timeout)
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out after %s", timeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			inspect, err := cli.ContainerInspect(ctx, containerID)
			if err != nil {
				return fmt.Errorf("inspecting container: %w", err)
			}
			if inspect.State == nil || !inspect.State.Running {
				return fmt.Errorf("container exited")
			}
			if inspect.State.Health == nil {
				continue
			}
			switch inspect.State.Health.Status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container unhealthy")
			}
		}
	}
}

// waitStaysRunning is the health gate for containers with no healthcheck of
// any kind: the container passes if it is still running, with no restarts, at
// the end of the grace period. A nonzero restart count means the process
// crashed and a restart policy revived it -- "running right now" would be a
// false positive.
func waitStaysRunning(ctx context.Context, cli DockerClient, containerID string) error {
	log.Printf("container %s: no healthcheck defined; verifying it stays running for %s", shortID(containerID), noHealthcheckGracePeriod)
	deadline := time.After(noHealthcheckGracePeriod)
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			inspect, err := cli.ContainerInspect(ctx, containerID)
			if err != nil {
				return fmt.Errorf("inspecting container: %w", err)
			}
			if inspect.State == nil || !inspect.State.Running {
				return fmt.Errorf("container exited within %s of starting", noHealthcheckGracePeriod)
			}
			if inspect.RestartCount > 0 {
				return fmt.Errorf("container restarted within %s of starting", noHealthcheckGracePeriod)
			}
		}
	}
}

// healthCheckTimeout returns the maximum time to wait for a container to become
// healthy, derived from its HEALTHCHECK config: start-period + retries*interval
// + one probe timeout as buffer. Falls back to 5 minutes if not configured.
func healthCheckTimeout(inspect types.ContainerJSON) time.Duration {
	const fallback = 5 * time.Minute
	if inspect.Config == nil || inspect.Config.Healthcheck == nil {
		return fallback
	}
	hc := inspect.Config.Healthcheck
	interval := hc.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	retries := hc.Retries
	if retries <= 0 {
		retries = 3
	}
	probeTimeout := hc.Timeout
	if probeTimeout <= 0 {
		probeTimeout = 30 * time.Second
	}
	total := hc.StartPeriod + time.Duration(retries)*interval + probeTimeout
	if total < 30*time.Second {
		return 30 * time.Second
	}
	return total
}
