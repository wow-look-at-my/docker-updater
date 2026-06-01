package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

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
	ContainerRename(ctx context.Context, containerID string, newContainerName string) error
	ContainerExecCreate(ctx context.Context, container string, options container.ExecOptions) (types.IDResponse, error)
	ContainerExecStart(ctx context.Context, execID string, config container.ExecStartOptions) error
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	Close() error
}

func newDockerClient() (DockerClient, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// AuthResolver returns a base64-encoded RegistryAuth string for the given
// image name, or empty string for anonymous pulls.
type AuthResolver func(imageName string) string

type dockerConfig struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

func loadDockerConfig(path string) (*dockerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading docker config %s: %w", path, err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing docker config %s: %w", path, err)
	}
	return &cfg, nil
}

func registryFromImage(imageName string) string {
	ref := imageName
	if at := strings.LastIndex(ref, "@"); at != -1 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon != -1 {
		if !strings.Contains(ref[colon:], "/") {
			ref = ref[:colon]
		}
	}

	slash := strings.IndexByte(ref, '/')
	if slash == -1 {
		return "https://index.docker.io/v1/"
	}

	firstPart := ref[:slash]
	if strings.ContainsAny(firstPart, ".:") {
		if firstPart == "docker.io" {
			return "https://index.docker.io/v1/"
		}
		return firstPart
	}

	return "https://index.docker.io/v1/"
}

func encodeRegistryAuth(entry dockerAuthEntry, serverAddress string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", fmt.Errorf("decoding auth for %s: %w", serverAddress, err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid auth format for %s", serverAddress)
	}
	authJSON, err := json.Marshal(map[string]string{
		"username":      parts[0],
		"password":      parts[1],
		"serveraddress": serverAddress,
	})
	if err != nil {
		return "", fmt.Errorf("encoding auth for %s: %w", serverAddress, err)
	}
	return base64.URLEncoding.EncodeToString(authJSON), nil
}

func newAuthResolver(cfg *dockerConfig) AuthResolver {
	if cfg == nil {
		return func(string) string { return "" }
	}
	return func(imageName string) string {
		registry := registryFromImage(imageName)
		entry, ok := cfg.Auths[registry]
		if !ok {
			return ""
		}
		encoded, err := encodeRegistryAuth(entry, registry)
		if err != nil {
			log.Printf("warning: failed to encode auth for %s: %v", registry, err)
			return ""
		}
		return encoded
	}
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

		info.PreCheckURL = c.Labels["docker-updater.pre-check.url"]
		info.PreCheckCommand = c.Labels["docker-updater.pre-check.command"]
		if info.PreCheckURL != "" || info.PreCheckCommand != "" {
			info.PreCheckTimeout = 30 * time.Second
			if t := c.Labels["docker-updater.pre-check.timeout"]; t != "" {
				if d, err := time.ParseDuration(t); err == nil {
					info.PreCheckTimeout = d
				}
			}
		}

		info.Rolling = c.Labels["docker-updater.rolling"] == "true"

		info.HealthCheckURL = c.Labels["docker-updater.health-check.url"]
		info.HealthCheckCommand = c.Labels["docker-updater.health-check.command"]
		if info.HealthCheckURL != "" || info.HealthCheckCommand != "" {
			info.HealthCheckTimeout = 60 * time.Second
			if t := c.Labels["docker-updater.health-check.timeout"]; t != "" {
				if d, err := time.ParseDuration(t); err == nil {
					info.HealthCheckTimeout = d
				}
			}
		}

		inspect, err := cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			// Without inspect data we cannot resolve a stable image reference.
			// Image mode depends on it, so skip rather than poll a bad ref.
			if mode == UpdateModeImage {
				log.Printf("cannot resolve registry repository for container %s; skipping (inspect failed: %v)", name, err)
				continue
			}
			monitored = append(monitored, info)
			continue
		}

		// Resolve a ":"-prefixed pre-check URL to the container's bridge IP.
		if strings.HasPrefix(info.PreCheckURL, ":") && inspect.NetworkSettings != nil {
			for _, net := range inspect.NetworkSettings.Networks {
				if net.IPAddress != "" {
					info.PreCheckURL = "http://" + net.IPAddress + info.PreCheckURL
					break
				}
			}
		}

		// Apply the same resolution for health-check URL.
		if strings.HasPrefix(info.HealthCheckURL, ":") && inspect.NetworkSettings != nil {
			for _, net := range inspect.NetworkSettings.Networks {
				if net.IPAddress != "" {
					info.HealthCheckURL = "http://" + net.IPAddress + info.HealthCheckURL
					break
				}
			}
		}

		if mode == UpdateModeImage {
			// Derive the registry reference from a stable, tag-independent
			// source: Config.Image (what the container was created with),
			// falling back to the running image's RepoDigests. This must not
			// depend on the running image still carrying a repo tag, and must
			// never poll the bare image ID that the container-list view
			// degrades to once RepoTags is lost.
			configImage := ""
			if inspect.Config != nil {
				configImage = inspect.Config.Image
			}
			repoDigests := runningRepoDigests(ctx, cli, inspect.Image)
			ref, ok := resolveImageRef(configImage, repoDigests)
			if !ok {
				log.Printf("cannot resolve registry repository for container %s; skipping", name)
				continue
			}
			info.Image = ref
			info.ImageDigest = imageIdentity(repoDigests, inspect.Image, repositoryOf(ref))
		} else {
			info.ImageDigest = inspect.Image
		}

		monitored = append(monitored, info)
	}

	return monitored, nil
}

// runningRepoDigests returns the RepoDigests of the running image, used to
// recover the registry repository and the running manifest digest. It returns
// nil if the image has no ID or cannot be inspected.
func runningRepoDigests(ctx context.Context, cli DockerClient, imageID string) []string {
	if imageID == "" {
		return nil
	}
	img, _, err := cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return nil
	}
	return img.RepoDigests
}

// pullImage pulls the latest version of an image and returns its identity
// (the registry manifest digest, falling back to the content ID).
func pullImage(ctx context.Context, cli DockerClient, imageName string, resolveAuth AuthResolver) (string, error) {
	// Never issue a pull/manifest request against a bare image ID (e.g.
	// "sha256:..."): it is not a registry repository and the daemon rejects it
	// with "pull access denied". Callers resolve a real reference first; this
	// guard enforces the invariant so a bad reference can never reach the daemon.
	if !hasRepository(imageName) {
		return "", fmt.Errorf("cannot pull %q: not a registry repository reference", imageName)
	}

	opts := image.PullOptions{}
	if auth := resolveAuth(imageName); auth != "" {
		opts.RegistryAuth = auth
	}

	reader, err := cli.ImagePull(ctx, imageName, opts)
	if err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Consume the pull output to completion.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return "", fmt.Errorf("reading pull response for %s: %w", imageName, err)
	}

	// Inspect the pulled image to get its freshly-resolved registry digest.
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return "", fmt.Errorf("inspecting pulled image %s: %w", imageName, err)
	}

	return imageIdentity(inspect.RepoDigests, inspect.ID, repositoryOf(imageName)), nil
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

	if err := waitPostUpdateHealthy(ctx, cli, created.ID, info); err != nil {
		log.Printf("container %s: post-update health check failed, stopping (%s): %v", info.Name, shortID(created.ID), err)
		cli.ContainerStop(ctx, created.ID, container.StopOptions{})
		return fmt.Errorf("container %s not healthy after update: %w", info.Name, err)
	}

	log.Printf("container %s updated and healthy (%s)", info.Name, shortID(created.ID))
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

	hostConfig := inspect.HostConfig
	hostConfig.PortBindings = nil

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{},
	}
	for netName, netSettings := range inspect.NetworkSettings.Networks {
		networkingConfig.EndpointsConfig[netName] = &network.EndpointSettings{
			Aliases: netSettings.Aliases,
		}
	}

	nextName := info.Name + "-next"
	log.Printf("container %s: starting rolling update with image %s", info.Name, newImage)

	created, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, nextName)
	if err != nil {
		return fmt.Errorf("creating next container %s: %w", nextName, err)
	}

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{})
		return fmt.Errorf("starting next container %s: %w", nextName, err)
	}

	if err := waitPostUpdateHealthy(ctx, cli, created.ID, info); err != nil {
		log.Printf("container %s: post-update health check failed for next container (%s): %v", info.Name, shortID(created.ID), err)
		cli.ContainerStop(ctx, created.ID, container.StopOptions{})
		cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{})
		return fmt.Errorf("next container %s not healthy: %w", nextName, err)
	}

	log.Printf("container %s: next container healthy, draining old", info.Name)
	timeout := 300
	if err := cli.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping old container %s: %w", info.Name, err)
	}
	if err := cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing old container %s: %w", info.Name, err)
	}

	if err := cli.ContainerRename(ctx, created.ID, info.Name); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", nextName, info.Name, err)
	}

	log.Printf("container %s: rolling update complete (%s)", info.Name, shortID(created.ID))
	return nil
}

func waitHealthy(ctx context.Context, cli DockerClient, containerID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
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
				return fmt.Errorf("no healthcheck defined")
			}
			if inspect.State.Health.Status == "healthy" {
				return nil
			}
		}
	}
}
