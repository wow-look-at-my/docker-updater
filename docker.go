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

		// Resolve a ":"-prefixed pre-check URL to the container's bridge IP.
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
		info.Address = containerAddress(inspect)
		info.ExposedPorts = exposedTCPPorts(inspect)

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

	// Create and start the new container.
	log.Printf("creating new container %s with image %s", info.Name, newImage)
	created, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, info.Name)
	if err != nil {
		return rollback("", fmt.Errorf("creating container %s: %w", info.Name, err))
	}

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return rollback(created.ID, fmt.Errorf("starting container %s: %w", info.Name, err))
	}

	if err := waitPostUpdateHealthy(ctx, cli, created.ID, info); err != nil {
		log.Printf("container %s: post-update health check failed (%s), rolling back: %v", info.Name, shortID(created.ID), err)
		return rollback(created.ID, fmt.Errorf("container %s not healthy after update: %w", info.Name, err))
	}

	log.Printf("container %s updated and healthy (%s)", info.Name, shortID(created.ID))
	return nil
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

	log.Printf("container %s: next container healthy, moving service aliases", info.Name)
	if err := attachServiceAliases(ctx, cli, created.ID, serviceAliases); err != nil {
		cli.ContainerStop(ctx, created.ID, container.StopOptions{})
		cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{})
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

	if err := cli.ContainerRename(ctx, created.ID, info.Name); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", nextName, info.Name, err)
	}

	log.Printf("container %s: rolling update complete (%s)", info.Name, shortID(created.ID))
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
