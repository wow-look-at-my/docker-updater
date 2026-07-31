package main

import (
	"context"
	"fmt"
	"io"
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
	// UpNoDeps runs `docker compose -f <configFiles...> --project-directory
	// <workingDir> up -d --no-deps <service>`. Image-mode recreates use this so
	// converging one service can never restart its dependencies: a stack whose
	// dependency owns durable state (a docker-in-docker daemon, a database)
	// must not lose it because a sibling service took a new image.
	UpNoDeps(ctx context.Context, configFiles []string, workingDir, service string) error
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

func (execComposeRunner) UpNoDeps(ctx context.Context, configFiles []string, workingDir, service string) error {
	args := composeArgs(configFiles, workingDir, "up", "-d", "--no-deps", service)
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

// composeLogWriter is where a child compose process's combined output is
// streamed. Production wires it to the updater's own stderr so `docker logs
// docker-updater` always carries the real compose output (progress AND the
// actual failure reason); tests capture it. Var so tests can substitute.
var composeLogWriter io.Writer = os.Stderr

// Bounds for the output tail attached to a failed compose invocation's error:
// the last composeErrTailLines lines or composeErrTailBytes bytes, whichever
// is smaller. Enough to name the actual cause (e.g. a compose config or
// build error) on the dashboard and in webhooks without shipping a whole
// build log there.
const (
	composeErrTailLines = 20
	composeErrTailBytes = 2048
)

func runDockerCompose(ctx context.Context, args []string) error {
	return runLoggedCommand(ctx, "docker", args)
}

// runLoggedCommand runs name with args, streaming the child's combined output
// to the updater's own log while retaining a bounded tail. On a non-zero exit
// the tail is folded into the returned error, so the dashboard and webhooks
// name the actual cause instead of a bare "exit status 1".
func runLoggedCommand(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	// Stdout and Stderr share the one writer value, so os/exec serializes
	// writes to it (no interleaving corruption) and uses a single pipe.
	tail := &tailBuffer{max: composeErrTailBytes}
	out := io.MultiWriter(composeLogWriter, tail)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		// A bare *exec.ExitError renders as "exit status 1", which names no
		// cause at all -- the dashboard/webhook error must carry the output
		// tail so the operator sees WHY compose failed without having to
		// open the updater's logs.
		if t := tail.tail(composeErrTailLines); t != "" {
			return fmt.Errorf("%s %s: %w; last output:\n%s", name, strings.Join(args, " "), err, t)
		}
		return fmt.Errorf("%s %s: %w (no output)", name, strings.Join(args, " "), err)
	}
	return nil
}

// tailBuffer is an io.Writer retaining only the last max bytes written, so a
// multi-megabyte build log costs constant memory while the failure tail stays
// available. Not safe for concurrent writers; os/exec guarantees serialized
// writes when the same writer value backs both Stdout and Stderr, and tail()
// is only called after the command has exited.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// tail returns the retained output trimmed to at most maxLines final lines,
// with leading/trailing whitespace dropped. Empty when the command wrote
// nothing.
func (t *tailBuffer) tail(maxLines int) string {
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}

// defaultComposeRunner is the runner used in production. Var so tests can
// substitute a fake.
var defaultComposeRunner composeRunner = execComposeRunner{}

// baseDigestStore records, per build-mode service, the base image identity
// last observed or built from. The primary staleness signal is the stateless
// layer-prefix check (see checkBuildUpdate); this store serves two roles:
// remembering the previous base identity for reporting the old->new
// transition, and acting as the change signal for services whose builds the
// layer check cannot validate (see prefixIncapableStore).
var baseDigestStore = struct {
	sync.Mutex
	digests map[string]string
}{digests: make(map[string]string)}

// prefixIncapableStore records build-mode services whose Dockerfile can never
// satisfy the layer-prefix check: a multi-stage build whose final stage is not
// built directly FROM the watched base (e.g. `FROM scratch` + `COPY
// --from=<base stage>`). For those, the layer check would report "stale" on
// every cycle and trigger an endless rebuild loop, so once a completed rebuild
// proves the built image still does not extend the base, the service falls
// back to base-digest change tracking.
var prefixIncapableStore = struct {
	sync.Mutex
	keys map[string]bool
}{keys: make(map[string]bool)}

// buildStateKey identifies a build-mode service across container recreations.
// A rebuild replaces the container (new ID), so keying by container ID alone
// would discard state on every applied update; the compose project/service
// pair is stable. Containers without compose labels fall back to their ID.
func buildStateKey(info ContainerInfo) string {
	if info.ComposeProject != "" && info.ComposeService != "" {
		return info.ComposeProject + "/" + info.ComposeService + ":" + info.BaseImage
	}
	return info.ID + ":" + info.BaseImage
}

func isPrefixIncapable(key string) bool {
	prefixIncapableStore.Lock()
	defer prefixIncapableStore.Unlock()
	return prefixIncapableStore.keys[key]
}

// markPrefixIncapable switches a service to base-digest change tracking,
// logging the fallback once.
func markPrefixIncapable(info ContainerInfo) {
	key := buildStateKey(info)
	prefixIncapableStore.Lock()
	already := prefixIncapableStore.keys[key]
	prefixIncapableStore.keys[key] = true
	prefixIncapableStore.Unlock()
	if !already {
		log.Printf("container %s: rebuilt image does not extend base %s (final stage is not built directly FROM it); falling back to base-digest change tracking", info.Name, info.BaseImage)
	}
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

// checkBuildUpdate reports whether a build-mode container's derived image is
// stale relative to its base image. It pulls ONLY the base image (never the
// local derived tag), then determines staleness from ground truth: an image
// built FROM a base starts with exactly the base's RootFS layers, so the base
// being a layer prefix of the derived image means the derived image extends
// the CURRENT base. This needs no persisted state -- it survives updater
// restarts and detects staleness that predates this process (the failure mode
// of the old first-seen-digest baseline, which silently adopted whatever the
// registry served on the first cycle after every restart).
//
// newBase is non-empty when a rebuild is warranted. oldBase is the
// previously-recorded base identity for reporting (may be empty when
// staleness is detected on the first cycle after a restart). verified is true
// when the outcome came from the layer check rather than the digest fallback.
//
// Services whose builds the layer check cannot validate (prefixIncapableStore)
// -- or whose layers are momentarily uninspectable -- use the digest fallback:
// first cycle adopts the current base as the baseline, later digest changes
// trigger a rebuild.
func checkBuildUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, resolveAuth AuthResolver) (oldBase, newBase string, verified bool, err error) {
	if info.BaseImage == "" {
		return "", "", false, fmt.Errorf("container %s has build mode but no resolvable base image", info.Name)
	}

	// Pull the base image (the only registry pull build mode ever issues). A
	// pull that finds the base already current downloads nothing; we still read
	// its identity afterwards.
	baseDigest, _, err := pullImage(ctx, cli, info.BaseImage, resolveAuth)
	if err != nil {
		return "", "", false, fmt.Errorf("pulling base image %s for %s: %w", info.BaseImage, info.Name, err)
	}

	key := buildStateKey(info)
	prev, seen := lookupBaseDigest(key)

	if !isPrefixIncapable(key) {
		if stale, ok := derivedImageStale(ctx, cli, info); ok {
			if !stale {
				// Verified current: the running image extends the base we just
				// pulled. Keep the digest baseline warm so the fallback path
				// and old->new reporting stay coherent.
				recordBuiltBase(info, baseDigest)
				return baseDigest, "", true, nil
			}
			return prev, baseDigest, true, nil
		}
		// Layers unavailable (container or image not inspectable right now):
		// fall through to the digest baseline for this cycle.
	}

	if !seen {
		// First cycle: adopt the current base as the built-from baseline so we
		// don't rebuild an already-current derived image. A real base change on
		// a later cycle then triggers the rebuild.
		recordBuiltBase(info, baseDigest)
		return baseDigest, "", false, nil
	}

	if baseDigest != prev {
		return prev, baseDigest, false, nil
	}

	return prev, "", false, nil
}

// derivedImageStale is the layer-prefix ground-truth check: it inspects the
// freshly pulled base image and the image the service's container actually
// runs, and reports the container's image stale when the base's layers are
// not a prefix of its layers. ok is false when either side's layers cannot be
// determined, in which case the caller must fall back to digest comparison.
func derivedImageStale(ctx context.Context, cli DockerClient, info ContainerInfo) (stale, ok bool) {
	baseLayers := imageLayersByRef(ctx, cli, info.BaseImage)
	derivedLayers := imageLayersByRef(ctx, cli, runningImageID(ctx, cli, info))
	if len(baseLayers) == 0 || len(derivedLayers) == 0 {
		return false, false
	}
	return !isLayerPrefix(baseLayers, derivedLayers), true
}

// isLayerPrefix reports whether base's RootFS layer diff IDs are a prefix of
// derived's -- the definition of "derived was built from this base". Equal
// layer lists count as a match (the derived image IS the base).
func isLayerPrefix(base, derived []string) bool {
	if len(base) == 0 || len(derived) < len(base) {
		return false
	}
	for i, l := range base {
		if derived[i] != l {
			return false
		}
	}
	return true
}

// imageIDAndLayers inspects an image by any reference (tag, digest, or
// content ID), returning its content ID and RootFS layer diff IDs. Zero
// values mean the image could not be inspected.
func imageIDAndLayers(ctx context.Context, cli DockerClient, ref string) (id string, layers []string) {
	if ref == "" {
		return "", nil
	}
	inspect, _, err := cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", nil
	}
	return inspect.ID, inspect.RootFS.Layers
}

// imageLayersByRef returns just an image's RootFS layer diff IDs.
func imageLayersByRef(ctx context.Context, cli DockerClient, ref string) []string {
	_, layers := imageIDAndLayers(ctx, cli, ref)
	return layers
}

// lookupBaseDigest returns the recorded base identity for a service, and
// whether one has been recorded at all.
func lookupBaseDigest(key string) (string, bool) {
	baseDigestStore.Lock()
	defer baseDigestStore.Unlock()
	digest, seen := baseDigestStore.digests[key]
	return digest, seen
}

// recordBuiltBase records the base identity the derived image was just built
// from (or verified against), so later cycles can report the old->new
// transition and the digest fallback compares against it.
func recordBuiltBase(info ContainerInfo, baseDigest string) {
	if baseDigest == "" {
		return
	}
	key := buildStateKey(info)
	baseDigestStore.Lock()
	baseDigestStore.digests[key] = baseDigest
	baseDigestStore.Unlock()
}

// rebuildAndRecreate rebuilds the service's image via `docker compose build
// --pull` and, only if the rebuild actually produced a different image than
// the one the container runs, recreates the service via `docker compose up
// -d`. Mirrors the "only recreate on real change" behavior of image mode: a
// cache-hit rebuild that yields the same image is a no-op (no churn). After a
// successful rebuild it records the new base identity so the next cycle is a
// no-op.
//
// verifyPrefix is true when the rebuild was triggered by the layer-prefix
// check; in that case a completed rebuild whose image STILL does not extend
// the base proves the build is prefix-incapable, and the service is switched
// to digest tracking so the layer check cannot rebuild it in a loop.
//
// It honors the post-update health check by polling the recreated container;
// rolling back is delegated to compose (re-running up -d with the prior image
// is not generically possible here; instead we report a health failure).
func rebuildAndRecreate(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, newBase string, verifyPrefix bool) (changed bool, err error) {
	configFiles := splitConfigFiles(info.ComposeConfigFiles)
	if len(configFiles) == 0 {
		return false, fmt.Errorf("container %s: no compose config files (com.docker.compose.project.config_files)", info.Name)
	}
	if info.ComposeService == "" {
		return false, fmt.Errorf("container %s: no compose service label", info.Name)
	}

	// Pin, before the rebuild, the image the container currently runs and the
	// reference it was created with (the tag the build re-points). `compose
	// build` never touches the container, so both stay valid across it.
	runningID, derivedRef := serviceContainerState(ctx, cli, info)

	log.Printf("container %s: rebuilding service %s (base %s changed)", info.Name, info.ComposeService, shortID(newBase))
	if err := runner.Build(ctx, configFiles, info.ComposeWorkingDir, info.ComposeService); err != nil {
		return false, fmt.Errorf("building %s: %w", info.ComposeService, err)
	}

	// What the rebuild produced: the image the derived tag points at NOW.
	// (Inspecting the container again would be useless -- a build re-tags the
	// image but leaves the container on the old one until `up -d`.)
	builtID, builtLayers := imageIDAndLayers(ctx, cli, derivedRef)
	if builtID != "" && runningID != "" && builtID == runningID {
		// Cache hit: the rebuild produced the identical image. Record the new
		// base so we don't keep rebuilding, but do not churn the container.
		log.Printf("container %s: rebuild produced identical image %s; not recreating", info.Name, shortID(builtID))
		if verifyPrefix {
			// The layer check flagged the running image as stale, yet a
			// --pull rebuild against the new base changed nothing: this build
			// can never satisfy the prefix check.
			markPrefixIncapable(info)
		}
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
	// failed build update -- and skip recording so the next cycle retries.
	if newID := currentServiceContainerID(ctx, cli, info); newID != "" {
		if herr := waitPostUpdateHealthy(ctx, cli, newID, info); herr != nil {
			return true, fmt.Errorf("container %s rebuilt but not healthy: %w", info.Name, herr)
		}
	}

	if verifyPrefix && len(builtLayers) > 0 {
		if baseLayers := imageLayersByRef(ctx, cli, info.BaseImage); len(baseLayers) > 0 && !isLayerPrefix(baseLayers, builtLayers) {
			// The rebuild completed on the new base and its image still does
			// not start with the base's layers (e.g. `FROM scratch` + `COPY
			// --from`): stop the layer check from re-flagging it forever.
			markPrefixIncapable(info)
		}
	}

	recordBuiltBase(info, newBase)
	log.Printf("container %s: rebuilt and recreated on new base %s", info.Name, shortID(newBase))
	return true, nil
}

// serviceContainerState inspects the service's current container once and
// returns both the content ID of the image it runs ("" if undeterminable)
// and the image reference it was created with (e.g. "opencode:local", the
// tag `compose build` re-points; falls back to info.Image).
func serviceContainerState(ctx context.Context, cli DockerClient, info ContainerInfo) (runningID, imageRef string) {
	imageRef = info.Image
	inspect, err := cli.ContainerInspect(ctx, currentServiceContainerID(ctx, cli, info))
	if err != nil {
		return "", imageRef
	}
	if inspect.Config != nil && inspect.Config.Image != "" {
		imageRef = inspect.Config.Image
	}
	if inspect.ContainerJSONBase != nil {
		runningID = inspect.Image
	}
	return runningID, imageRef
}

// runningImageID returns the content ID of the image the service's current
// container runs, or "" if it cannot be determined.
func runningImageID(ctx context.Context, cli DockerClient, info ContainerInfo) string {
	id, _ := serviceContainerState(ctx, cli, info)
	return id
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
