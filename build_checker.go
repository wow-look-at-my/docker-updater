package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// composeRunner runs `docker compose ...` invocations. It is an interface so
// build-mode tests can drive the rebuild/recreate decision without a real
// Docker daemon or compose CLI. The real implementation shells out to the
// `docker compose` plugin.
type composeRunner interface {
	// Build runs `docker compose -f <configFiles...> --project-directory
	// <workingDir> build --pull <service>`.
	Build(ctx context.Context, configFiles []string, workingDir, service string) error
	// Up runs `docker compose -f <configFiles...> --project-directory
	// <workingDir> up -d <service>`.
	Up(ctx context.Context, configFiles []string, workingDir, service string) error
}

// execComposeRunner is the production composeRunner: it execs the docker CLI.
type execComposeRunner struct{}

func (execComposeRunner) Build(ctx context.Context, configFiles []string, workingDir, service string) error {
	args := composeArgs(configFiles, workingDir, "build", "--pull", service)
	return runDockerCompose(ctx, args)
}

func (execComposeRunner) Up(ctx context.Context, configFiles []string, workingDir, service string) error {
	args := composeArgs(configFiles, workingDir, "up", "-d", service)
	return runDockerCompose(ctx, args)
}

// composeArgs assembles the `compose` argument list shared by Build and Up.
func composeArgs(configFiles []string, workingDir string, rest ...string) []string {
	args := []string{"compose"}
	for _, f := range configFiles {
		if f != "" {
			args = append(args, "-f", f)
		}
	}
	if workingDir != "" {
		args = append(args, "--project-directory", workingDir)
	}
	return append(args, rest...)
}

func runDockerCompose(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stderr // surface build output in the updater's logs
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// defaultComposeRunner is the runner used in production. Var so tests can
// substitute a fake.
var defaultComposeRunner composeRunner = execComposeRunner{}

// baseDigestStore records, per container, the base image identity the derived
// image was last built from. This is the "built-from" signal build mode
// compares against: on the first cycle we record the base's current local
// identity (no rebuild), and on later cycles a changed base identity is what
// triggers a rebuild. After a successful rebuild we record the new base
// identity so the next cycle is a no-op. It mirrors gitRefStore (git_checker.go)
// -- the simplest correct signal, requiring no Dockerfile/label cooperation
// from the user and no inspection of the derived image's layers.
var baseDigestStore = struct {
	sync.Mutex
	digests map[string]string
}{digests: make(map[string]string)}

// baseImageIdentity returns a stable identity for the base image: its registry
// manifest digest when available (from RepoDigests), else its content ID.
// Comparing this across a base-image pull reveals whether the base advanced.
func baseImageIdentity(ctx context.Context, cli DockerClient, baseImage string) (string, error) {
	inspect, _, err := cli.ImageInspectWithRaw(ctx, baseImage)
	if err != nil {
		return "", fmt.Errorf("inspecting base image %s: %w", baseImage, err)
	}
	return imageIdentity(inspect.RepoDigests, inspect.ID, repositoryOf(baseImage)), nil
}

// resolveBaseImage determines the registry image build mode should watch.
// Preference: the explicit docker-updater.base-image label, else the FROM of
// the service's build (inline dockerfile_inline or a Dockerfile under the
// compose working dir). Returns "" if neither resolves.
//
// readFile is injected so tests don't touch the filesystem.
func resolveBaseImage(info ContainerInfo, readFile func(string) (string, error)) string {
	if explicit := strings.TrimSpace(info.Labels["docker-updater.base-image"]); explicit != "" {
		return explicit
	}

	// Inline Dockerfile via the compose `build.dockerfile_inline` field is
	// surfaced by compose as a label on the container's image config in some
	// setups; we also accept it as a docker-updater label for testability.
	if inline := info.Labels["docker-updater.dockerfile-inline"]; inline != "" {
		if base := parseBaseImageFromDockerfile(inline); base != "" && isRegistryBase(base) {
			return base
		}
	}

	// Otherwise look for a Dockerfile in the compose working dir (or alongside
	// the first config file).
	for _, dir := range candidateBuildDirs(info) {
		path := filepath.Join(dir, "Dockerfile")
		content, err := readFile(path)
		if err != nil {
			continue
		}
		if base := parseBaseImageFromDockerfile(content); base != "" && isRegistryBase(base) {
			return base
		}
	}

	return ""
}

// isRegistryBase reports whether a parsed FROM base is a watchable registry
// image, as opposed to `scratch` or a build-local stage alias that failed to
// resolve. `scratch` has no registry origin; an unresolved alias is not a
// pullable reference. Both are treated as "cannot watch".
func isRegistryBase(base string) bool {
	if base == "" || strings.EqualFold(base, "scratch") {
		return false
	}
	// A base referencing a build ARG (e.g. ${BASE} or $BASE) cannot be
	// resolved statically; skip rather than guess.
	if strings.Contains(base, "$") {
		return false
	}
	return hasRepository(base)
}

// candidateBuildDirs returns directories to look for a Dockerfile in,
// preferring the compose working dir, then the directory of each config file.
func candidateBuildDirs(info ContainerInfo) []string {
	var dirs []string
	if info.ComposeWorkingDir != "" {
		dirs = append(dirs, info.ComposeWorkingDir)
	}
	for _, f := range splitConfigFiles(info.ComposeConfigFiles) {
		dirs = append(dirs, filepath.Dir(f))
	}
	return dirs
}

// splitConfigFiles splits the comma-separated compose config_files label into
// individual file paths.
func splitConfigFiles(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readFileString reads a file's full contents as a string for resolveBaseImage.
func readDockerfile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkBuildUpdate reports whether the base image of a build-mode container has
// advanced since the derived image was last built. It pulls ONLY the base image
// (never the local derived tag), then compares the base's identity to the one
// recorded for this container.
//
// newBase is non-empty when a rebuild is warranted (the base changed, or this
// is the first time we've seen the container -- in which case we adopt the
// current base without rebuilding by returning newBase=="" but seeding the
// store). oldBase is the previously-recorded base identity for reporting.
func checkBuildUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, resolveAuth AuthResolver) (oldBase, newBase string, err error) {
	if info.BaseImage == "" {
		return "", "", fmt.Errorf("container %s has build mode but no resolvable base image", info.Name)
	}

	// Pull the base image (the only registry pull build mode ever issues). A
	// pull that finds the base already current downloads nothing; we still read
	// its identity afterwards.
	baseDigest, _, err := pullImage(ctx, cli, info.BaseImage, resolveAuth)
	if err != nil {
		return "", "", fmt.Errorf("pulling base image %s for %s: %w", info.BaseImage, info.Name, err)
	}

	key := info.ID + ":" + info.BaseImage

	baseDigestStore.Lock()
	prev, seen := baseDigestStore.digests[key]
	baseDigestStore.Unlock()

	if !seen {
		// First cycle: adopt the current base as the built-from baseline so we
		// don't rebuild an already-current derived image. A real base change on
		// a later cycle then triggers the rebuild.
		baseDigestStore.Lock()
		baseDigestStore.digests[key] = baseDigest
		baseDigestStore.Unlock()
		return baseDigest, "", nil
	}

	if baseDigest != prev {
		return prev, baseDigest, nil
	}

	return prev, "", nil
}

// recordBuiltBase records the base identity the derived image was just built
// from, so the next cycle compares against it.
func recordBuiltBase(info ContainerInfo, baseDigest string) {
	if baseDigest == "" {
		return
	}
	key := info.ID + ":" + info.BaseImage
	baseDigestStore.Lock()
	baseDigestStore.digests[key] = baseDigest
	baseDigestStore.Unlock()
}

// rebuildAndRecreate rebuilds the service's image via `docker compose build
// --pull` and, only if the resulting image ID actually changed, recreates the
// service via `docker compose up -d`. Mirrors the "only recreate on real
// change" behavior of image mode: a cache-hit rebuild that yields the same
// image ID is a no-op (no churn). After a successful rebuild it records the new
// base identity so the next cycle is a no-op.
//
// It honors the post-update health check by polling the recreated container,
// rolling back is delegated to compose (re-running up -d with the prior image
// is not generically possible here; instead we report a health failure).
func rebuildAndRecreate(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, newBase string) (changed bool, err error) {
	configFiles := splitConfigFiles(info.ComposeConfigFiles)
	if len(configFiles) == 0 {
		return false, fmt.Errorf("container %s: no compose config files (com.docker.compose.project.config_files)", info.Name)
	}
	if info.ComposeService == "" {
		return false, fmt.Errorf("container %s: no compose service label", info.Name)
	}

	// Pin the derived image ID before the rebuild so we can tell whether the
	// rebuild actually produced new content.
	oldImageID := derivedImageID(ctx, cli, info)

	log.Printf("container %s: rebuilding service %s (base %s changed)", info.Name, info.ComposeService, shortID(newBase))
	if err := runner.Build(ctx, configFiles, info.ComposeWorkingDir, info.ComposeService); err != nil {
		return false, fmt.Errorf("building %s: %w", info.ComposeService, err)
	}

	newImageID := derivedImageID(ctx, cli, info)
	if oldImageID != "" && newImageID != "" && oldImageID == newImageID {
		// Cache hit: the rebuild produced the identical image. Record the new
		// base so we don't keep rebuilding, but do not churn the container.
		log.Printf("container %s: rebuild produced identical image %s; not recreating", info.Name, shortID(newImageID))
		recordBuiltBase(info, newBase)
		return false, nil
	}

	log.Printf("container %s: recreating service %s on rebuilt image", info.Name, info.ComposeService)
	if err := runner.Up(ctx, configFiles, info.ComposeWorkingDir, info.ComposeService); err != nil {
		return false, fmt.Errorf("recreating %s: %w", info.ComposeService, err)
	}

	// Post-update health gate: compose recreated the container under the same
	// service; verify the new container is healthy (or, lacking a healthcheck,
	// stays running). On failure we report it -- the operator/webhook see a
	// failed build update.
	if newID := currentServiceContainerID(ctx, cli, info); newID != "" {
		if herr := waitPostUpdateHealthy(ctx, cli, newID, info); herr != nil {
			return true, fmt.Errorf("container %s rebuilt but not healthy: %w", info.Name, herr)
		}
	}

	recordBuiltBase(info, newBase)
	log.Printf("container %s: rebuilt and recreated on new base %s", info.Name, shortID(newBase))
	return true, nil
}

// derivedImageID returns the content ID of the service's derived image (the
// image the running container uses), or "" if it cannot be determined.
func derivedImageID(ctx context.Context, cli DockerClient, info ContainerInfo) string {
	id := currentServiceContainerID(ctx, cli, info)
	if id == "" {
		id = info.ID
	}
	inspect, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return ""
	}
	return inspect.Image
}

// currentServiceContainerID returns the container ID currently backing the
// build-mode service. After a compose recreate the original info.ID is gone, so
// we re-list and match on the compose project+service labels. Falls back to
// info.ID when no match is found (e.g. before the first recreate).
func currentServiceContainerID(ctx context.Context, cli DockerClient, info ContainerInfo) string {
	list, err := listAllContainers(ctx, cli)
	if err != nil {
		return info.ID
	}
	for _, c := range list {
		if c.Labels["com.docker.compose.project"] == info.ComposeProject &&
			c.Labels["com.docker.compose.service"] == info.ComposeService {
			return c.ID
		}
	}
	return info.ID
}
