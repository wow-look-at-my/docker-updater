package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/docker/docker/api/types"
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
// from the container's healthcheck configuration; a container with no
// HEALTHCHECK at all just has to stay running through a short grace period.
func waitPostUpdateHealthy(ctx context.Context, cli DockerClient, containerID string, info ContainerInfo) error {
	if info.HealthCheckURL != "" {
		url := info.HealthCheckURL
		var notBefore time.Time
		if info.HealthCheckURLFromContainer {
			var err error
			if url, notBefore, err = rehostToNewContainer(ctx, cli, containerID, url); err != nil {
				return err
			}
		}
		log.Printf("container %s: waiting for HTTP health check at %s", info.Name, url)
		return waitHTTPHealthy(ctx, url, info.HealthCheckTimeout, notBefore)
	}
	if info.HealthCheckCommand != "" {
		log.Printf("container %s: waiting for exec health check: %s", info.Name, info.HealthCheckCommand)
		return waitExecHealthy(ctx, cli, containerID, info.HealthCheckCommand, info.HealthCheckTimeout)
	}
	// No health-check label: fall back to Docker's HEALTHCHECK status. waitHealthy
	// derives its own budget from the container's healthcheck config.
	return waitHealthy(ctx, cli, containerID)
}

// noEndpointError explains why a container has no address to dial.
//
// Docker gives an endpoint an IP only while the container RUNS, so a container
// that crash-loops looks exactly like one on host networking. Reporting the
// network mode alone sends the operator to a label that would change nothing,
// which is how one entry sat stuck for hours behind advice that did not apply.
// A container that is not running is reported as what it is, with the line it
// died on.
func noEndpointError(ctx context.Context, cli DockerClient, containerID string, inspect types.ContainerJSON) error {
	id := shortID(containerID)
	if st := inspect.State; st != nil && !st.Running {
		msg := fmt.Sprintf("new container %s is %s, so it has no address to health check", id, st.Status)
		if st.ExitCode != 0 {
			msg += fmt.Sprintf(" (exit %d)", st.ExitCode)
		}
		if st.Error != "" {
			msg += ": " + st.Error
		}
		return errors.New(msg + whyItDied(ctx, cli, containerID))
	}
	return fmt.Errorf("new container %s runs on host, none or container: networking, so it has no IP of "+
		"its own to poll; give it an absolute docker-updater.health-check.url", id)
}

// rehostToNewContainer points a container-derived health URL at the container
// the update started. The URL was built from the address of the container the
// update destroyed, and Docker both gives the replacement its own IP and may
// hand the one to an unrelated container. Only the host moves: a recreated
// container keeps the port and path it served on.
//
// An address it cannot resolve is a failure, never a fall back to the one:
// polling a dead or recycled address either times out the gate or passes it
// on somebody else's health.
//
// It also returns the time the new container started, so the gate can reject
// a health answer from a process that started before it.
func rehostToNewContainer(ctx context.Context, cli DockerClient, containerID, rawURL string) (string, time.Time, error) {
	if containerID == "" {
		return "", time.Time{}, fmt.Errorf("health check address: no container found after the update to resolve %s against", rawURL)
	}
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("inspecting new container %s for health check address: %w", shortID(containerID), err)
	}
	address, _ := containerEndpoint(inspect)
	if address == "" {
		return "", time.Time{}, noEndpointError(ctx, cli, containerID, inspect)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parsing health check URL %q: %w", rawURL, err)
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(address, port)
	} else {
		u.Host = address
	}
	return u.String(), containerStartedAt(inspect), nil
}

// containerStartedAt returns State.StartedAt, or the zero time when Docker does
// not report one. The zero time turns the started check off.
func containerStartedAt(inspect types.ContainerJSON) time.Time {
	if inspect.ContainerJSONBase == nil || inspect.State == nil {
		return time.Time{}
	}
	started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return started
}

// healthBody is the optional JSON body of a health answer.
type healthBody struct {
	Started string `json:"started"`
}

// staleAnswer reports why a 2xx answer cannot count: the body names a process
// start before notBefore, or a start time it cannot parse. A body without
// `started` passes on the status code alone.
func staleAnswer(body []byte, notBefore time.Time) string {
	if notBefore.IsZero() {
		return ""
	}
	var parsed healthBody
	if json.Unmarshal(body, &parsed) != nil || parsed.Started == "" {
		return ""
	}
	started, err := time.Parse(time.RFC3339Nano, parsed.Started)
	if err != nil {
		return fmt.Sprintf("the health answer names started=%q, which is not an RFC 3339 time", parsed.Started)
	}
	if started.Before(notBefore.Truncate(time.Second)) {
		return fmt.Sprintf("the health answer came from a process started at %s, before the new container started at %s",
			started.UTC().Format(time.RFC3339), notBefore.UTC().Format(time.RFC3339))
	}
	return ""
}

// waitHTTPHealthy polls url every 2s until it returns a 2xx response or timeout.
// A non-zero notBefore also requires the answer's `started` to be no earlier.
func waitHTTPHealthy(ctx context.Context, url string, timeout time.Duration, notBefore time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	var stale string
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				if stale != "" {
					return fmt.Errorf("HTTP health check timed out after %s: %s", timeout, stale)
				}
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
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				continue
			}
			if stale = staleAnswer(body, notBefore); stale == "" {
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
