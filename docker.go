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
	"github.com/docker/docker/pkg/jsonmessage"
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
	ImageTag(ctx context.Context, source, target string) error
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
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

		// Compose metadata is read for every mode, not just build mode. A
		// compose-managed container recreated from its own stored HostConfig
		// silently keeps the config it was created with, so edits to the
		// compose file (a new mount, a changed env var) never reach the
		// service. Image mode uses these labels to converge through `docker
		// compose up -d` instead, making the compose file authoritative.
		info.ComposeProject = c.Labels["com.docker.compose.project"]
		info.ComposeService = c.Labels["com.docker.compose.service"]
		info.ComposeConfigFiles = c.Labels["com.docker.compose.project.config_files"]
		info.ComposeWorkingDir = c.Labels["com.docker.compose.project.working_dir"]

		if mode == UpdateModeBuild {
			info.BaseImage = resolveBaseImage(info, readDockerfile)
		}

		info.PreCheckURL = c.Labels["docker-updater.pre-check.url"]
		info.PreCheckCommand = c.Labels["docker-updater.pre-check.command"]
		if info.PreCheckURL != "" || info.PreCheckCommand != "" {
			info.PreCheckTimeout = defaultPreCheckTimeout
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
			info.HealthCheckTimeout = defaultHealthCheckTimeout
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

		// Resolve a ":"-prefixed pre-check URL to the container's own IP.
		if strings.HasPrefix(info.PreCheckURL, ":") && inspect.NetworkSettings != nil {
			for _, net := range inspect.NetworkSettings.Networks {
				if net.IPAddress != "" {
					info.PreCheckURL = "http://" + net.IPAddress + info.PreCheckURL
					break
				}
			}
		}

		// Discovery inputs for the standard /.well-known/docker-updater/
		// endpoints. Both come from the container's own settings, so a
		// container that says nothing beyond the enable label is still
		// reachable without configuration.
		info.Address, info.AddressNetwork = containerEndpoint(inspect)
		info.ExposedPorts = exposedTCPPorts(inspect)
		info.DockerHealthcheck = hasDockerHealthcheck(inspect)

		// Apply the same resolution for health-check URL. The address is this
		// container's, so the post-update gate re-resolves it against the
		// container that replaces it.
		if strings.HasPrefix(info.HealthCheckURL, ":") && inspect.NetworkSettings != nil {
			for _, net := range inspect.NetworkSettings.Networks {
				if net.IPAddress != "" {
					info.HealthCheckURL = "http://" + net.IPAddress + info.HealthCheckURL
					info.HealthCheckURLFromContainer = true
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

			// Skip-guard for locally-built images. A container whose image has
			// no RepoDigests has no registry origin (it was built locally and
			// never pushed/pulled, e.g. a compose `build:` tag like
			// `opencode:local`). resolveImageRef still returns the local tag as
			// pullable, so without this guard image mode would `docker pull
			// <local-tag>` every cycle and fail with "repository does not
			// exist". Detect it and skip with an actionable warning instead.
			// A digest-pinned reference (already canonical) is exempt -- it
			// names a real registry manifest even with no RepoDigests recorded.
			if len(repoDigests) == 0 && !isCanonicalRef(ref) {
				log.Printf("image %s is locally built and not in a registry; use docker-updater.mode=build (skipping container %s)", ref, name)
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

// listAllContainers returns every container on the host (running or not). Used
// by build mode to re-find a service's container after a compose recreate
// replaces it.
func listAllContainers(ctx context.Context, cli DockerClient) ([]types.Container, error) {
	return cli.ContainerList(ctx, container.ListOptions{All: true})
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

// pullImage pulls the latest version of an image and returns its identity (the
// registry manifest digest, falling back to the content ID) and whether the
// pull actually fetched new content.
//
// fetched is true only when the local image the reference resolves to changed
// as a result of the pull: a pull that finds the image already up to date
// downloads nothing and reports fetched=false. This distinguishes "we ran a
// pull check" (every cycle) from "we downloaded a newer image" (rare), so
// callers can record when an image was genuinely pulled rather than merely
// polled.
func pullImage(ctx context.Context, cli DockerClient, imageName string, resolveAuth AuthResolver) (digest string, fetched bool, err error) {
	// Never issue a pull/manifest request against a bare image ID (e.g.
	// "sha256:..."): it is not a registry repository and the daemon rejects it
	// with "pull access denied". Callers resolve a real reference first; this
	// guard enforces the invariant so a bad reference can never reach the daemon.
	if !hasRepository(imageName) {
		return "", false, fmt.Errorf("cannot pull %q: not a registry repository reference", imageName)
	}

	// Record the content ID the reference resolves to before pulling, so we can
	// tell afterwards whether the pull fetched new content. A reference not yet
	// present locally inspects with an error, leaving beforeID empty -- in which
	// case any successful pull is, by definition, fetching new content.
	beforeID := ""
	if before, _, e := cli.ImageInspectWithRaw(ctx, imageName); e == nil {
		beforeID = before.ID
	}

	opts := image.PullOptions{}
	if auth := resolveAuth(imageName); auth != "" {
		opts.RegistryAuth = auth
	}

	reader, err := cli.ImagePull(ctx, imageName, opts)
	if err != nil {
		return "", false, fmt.Errorf("pulling image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Decode the pull's JSON progress stream to completion. The daemon
	// reports mid-pull failures (registry 429/5xx, dropped connections) as
	// in-band `error` records in a cleanly terminated stream, so blindly
	// draining the reader would treat a failed pull as success -- the caller
	// would then inspect the OLD local image and wrongly conclude
	// "up-to-date". jsonmessage surfaces those records as an error.
	if err := jsonmessage.DisplayJSONMessagesStream(reader, io.Discard, 0, false, nil); err != nil {
		return "", false, fmt.Errorf("pulling image %s: %w", imageName, err)
	}

	// Inspect the pulled image to get its freshly-resolved registry digest.
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return "", false, fmt.Errorf("inspecting pulled image %s: %w", imageName, err)
	}

	digest = imageIdentity(inspect.RepoDigests, inspect.ID, repositoryOf(imageName))
	fetched = inspect.ID != beforeID
	return digest, fetched, nil
}
