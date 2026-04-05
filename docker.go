package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DockerClient defines the Docker API methods we use, allowing test mocks.
type DockerClient interface {
	Info(ctx context.Context) (system.Info, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	Close() error
}

func newDockerClient() (DockerClient, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// listMonitoredContainers returns containers that have the opt-in label set to "true".
func listMonitoredContainers(ctx context.Context, cli DockerClient, label string) ([]ContainerInfo, error) {
	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var monitored []ContainerInfo
	for _, c := range containers {
		if c.Labels[label] != "true" {
			continue
		}

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		mode := UpdateModeImage
		if m := c.Labels["docker-updater.mode"]; m != "" {
			mode = UpdateMode(m)
		}

		info := ContainerInfo{
			ID:     c.ID,
			Name:   name,
			Image:  c.Image,
			Mode:   mode,
			Labels: c.Labels,
		}

		if mode == UpdateModeGit {
			info.GitRepo = c.Labels["docker-updater.git-repo"]
			info.GitRef = c.Labels["docker-updater.git-ref"]
			if info.GitRef == "" {
				info.GitRef = "refs/heads/main"
			}
		}

		// Get current image digest from inspection.
		inspect, err := cli.ContainerInspect(ctx, c.ID)
		if err == nil {
			info.ImageDigest = inspect.Image
		}

		monitored = append(monitored, info)
	}

	return monitored, nil
}

// pullImage pulls the latest version of an image and returns the new digest.
func pullImage(ctx context.Context, cli DockerClient, imageName string) (string, error) {
	reader, err := cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Consume the pull output to completion.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return "", fmt.Errorf("reading pull response for %s: %w", imageName, err)
	}

	// Inspect the pulled image to get its digest.
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return "", fmt.Errorf("inspecting pulled image %s: %w", imageName, err)
	}

	return inspect.ID, nil
}

// recreateContainer stops the old container, creates a new one with the same
// config but an updated image, and starts it.
func recreateContainer(ctx context.Context, cli DockerClient, info ContainerInfo, newImage string) error {
	// Inspect the existing container to capture its full config.
	inspect, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
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

	// Create and start the new container.
	log.Printf("creating new container %s with image %s", info.Name, newImage)
	created, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, info.Name)
	if err != nil {
		return fmt.Errorf("creating container %s: %w", info.Name, err)
	}

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container %s: %w", info.Name, err)
	}

	log.Printf("container %s updated and started (%s)", info.Name, shortID(created.ID))
	return nil
}
