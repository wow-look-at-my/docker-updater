package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
)

// healthCheckPollInterval is the polling interval for HTTP and exec health checks.
// Overridden in tests to keep the suite fast.
var healthCheckPollInterval = 2 * time.Second

// execPollInterval is the polling interval when waiting for an exec to finish.
// Overridden in tests to keep the suite fast.
var execPollInterval = 100 * time.Millisecond

// waitPostUpdateHealthy verifies the new container is healthy after an update.
// If info.HealthCheckURL is set, it polls via HTTP GET.
// If info.HealthCheckCommand is set, it polls via exec inside the container.
// Otherwise it falls back to Docker's HEALTHCHECK status with a budget derived
// from the container's healthcheck configuration.
func waitPostUpdateHealthy(ctx context.Context, cli DockerClient, containerID string, info ContainerInfo) error {
	if info.HealthCheckURL != "" {
		log.Printf("container %s: waiting for HTTP health check at %s", info.Name, info.HealthCheckURL)
		return waitHTTPHealthy(ctx, info.HealthCheckURL, info.HealthCheckTimeout)
	}
	if info.HealthCheckCommand != "" {
		log.Printf("container %s: waiting for exec health check: %s", info.Name, info.HealthCheckCommand)
		return waitExecHealthy(ctx, cli, containerID, info.HealthCheckCommand, info.HealthCheckTimeout)
	}
	timeout := dockerHealthBudget(ctx, cli, containerID)
	return waitHealthy(ctx, cli, containerID, timeout)
}

// waitHTTPHealthy polls url every 2s until it returns a 2xx response or timeout.
func waitHTTPHealthy(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("HTTP health check timed out after %s", timeout)
			}
			return ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return fmt.Errorf("creating health check request: %w", err)
			}
			req.Header.Set("User-Agent", "docker-updater")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// Not ready yet — keep polling.
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
	}
}

// waitExecHealthy polls command inside the container every 2s until it exits 0 or timeout.
func waitExecHealthy(ctx context.Context, cli DockerClient, containerID, command string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("exec health check timed out after %s", timeout)
			}
			return ctx.Err()
		case <-ticker.C:
			ok, err := runExecOnce(ctx, cli, containerID, command)
			if err == nil && ok {
				return nil
			}
		}
	}
}

// runExecOnce runs command inside the container and returns true if it exits 0.
func runExecOnce(ctx context.Context, cli DockerClient, containerID, command string) (bool, error) {
	resp, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: []string{"sh", "-c", command},
	})
	if err != nil {
		return false, fmt.Errorf("creating exec: %w", err)
	}
	if err := cli.ContainerExecStart(ctx, resp.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return false, fmt.Errorf("starting exec: %w", err)
	}

	pollTicker := time.NewTicker(execPollInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-pollTicker.C:
			inspect, err := cli.ContainerExecInspect(ctx, resp.ID)
			if err != nil {
				return false, fmt.Errorf("inspecting exec: %w", err)
			}
			if !inspect.Running {
				return inspect.ExitCode == 0, nil
			}
		}
	}
}

// dockerHealthBudget derives a wait budget from the container's HEALTHCHECK
// configuration: StartPeriod + Retries×Interval + 10s buffer.
// Returns 60s when the config is absent or zero.
func dockerHealthBudget(ctx context.Context, cli DockerClient, containerID string) time.Duration {
	const fallback = 60 * time.Second
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil || inspect.Config == nil || inspect.Config.Healthcheck == nil {
		return fallback
	}
	hc := inspect.Config.Healthcheck
	retries := hc.Retries
	if retries <= 0 {
		retries = 3
	}
	budget := hc.StartPeriod + time.Duration(retries)*hc.Interval
	if budget <= 0 {
		return fallback
	}
	return budget + 10*time.Second
}
