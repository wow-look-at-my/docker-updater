package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
)

func runPreCheck(ctx context.Context, cli DockerClient, info ContainerInfo) error {
	if info.PreCheckURL != "" {
		return runHTTPCheck(ctx, info)
	}
	return runExecCheck(ctx, cli, info)
}

func runHTTPCheck(ctx context.Context, info ContainerInfo) error {
	if info.PreCheckURL == "" {
		return fmt.Errorf("pre-check url is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, info.PreCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.PreCheckURL, nil)
	if err != nil {
		return fmt.Errorf("creating pre-check request: %w", err)
	}
	req.Header.Set("User-Agent", "docker-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pre-check HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return fmt.Errorf("pre-check returned status %d", resp.StatusCode)
}

func runExecCheck(ctx context.Context, cli DockerClient, info ContainerInfo) error {
	if info.PreCheckCommand == "" {
		return fmt.Errorf("pre-check command is empty")
	}

	execConfig := container.ExecOptions{
		Cmd: []string{"sh", "-c", info.PreCheckCommand},
	}

	resp, err := cli.ContainerExecCreate(ctx, info.ID, execConfig)
	if err != nil {
		return fmt.Errorf("creating exec for pre-check: %w", err)
	}

	if err := cli.ContainerExecStart(ctx, resp.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("starting exec for pre-check: %w", err)
	}

	deadline := time.After(info.PreCheckTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("pre-check exec timed out after %s", info.PreCheckTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			inspect, err := cli.ContainerExecInspect(ctx, resp.ID)
			if err != nil {
				return fmt.Errorf("inspecting exec for pre-check: %w", err)
			}
			if !inspect.Running {
				if inspect.ExitCode != 0 {
					return fmt.Errorf("pre-check command exited with code %d", inspect.ExitCode)
				}
				log.Printf("container %s: pre-check passed", info.Name)
				return nil
			}
		}
	}
}
