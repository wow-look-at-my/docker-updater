package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeComposeRunner records compose build/up invocations without shelling out.
type fakeComposeRunner struct {
	buildCalls int
	upCalls    int
	buildErr   error
	upErr      error
	onBuild    func() // hook to mutate fixture state between build and the image re-read

	// Image-mode recreates go through UpNoDeps; recorded separately so tests
	// can assert dependencies are never swept into a single-service update.
	upNoDepsCalls    int
	upNoDepsErr      error
	upNoDepsServices []string
	upNoDepsFiles    [][]string
	upNoDepsDirs     []string
	upNoDepsProjects []string
	buildTargets     []composeTarget
	onUpNoDeps       func() // hook to mutate fixture state between converge attempts
}

func (f *fakeComposeRunner) Build(_ context.Context, t composeTarget) error {
	f.buildCalls++
	f.buildTargets = append(f.buildTargets, t)
	if f.onBuild != nil {
		f.onBuild()
	}
	return f.buildErr
}

func (f *fakeComposeRunner) Up(_ context.Context, _ composeTarget) error {
	f.upCalls++
	return f.upErr
}

func (f *fakeComposeRunner) UpNoDeps(_ context.Context, t composeTarget) error {
	f.upNoDepsCalls++
	f.upNoDepsFiles = append(f.upNoDepsFiles, t.ConfigFiles)
	f.upNoDepsDirs = append(f.upNoDepsDirs, t.WorkingDir)
	f.upNoDepsServices = append(f.upNoDepsServices, t.Service)
	f.upNoDepsProjects = append(f.upNoDepsProjects, t.Project)
	if f.onUpNoDeps != nil {
		f.onUpNoDeps()
	}
	return f.upNoDepsErr
}

// resetBuildState clears the per-service baseline and prefix-incapability
// records between tests so they don't share state.
func resetBuildState() {
	baseDigestStore.Lock()
	baseDigestStore.digests = make(map[string]string)
	baseDigestStore.Unlock()
	prefixIncapableStore.Lock()
	prefixIncapableStore.keys = make(map[string]bool)
	prefixIncapableStore.Unlock()
}

func TestResolveBaseImageExplicitLabelWins(t *testing.T) {
	t.Serial()
	info := ContainerInfo{
		Labels: map[string]string{
			"docker-updater.base-image":        "ghcr.io/anomalyco/opencode:latest",
			"docker-updater.dockerfile-inline": "FROM alpine:3.20\n",
		},
	}
	got := resolveBaseImage(info, func(string) (string, error) { return "", errors.New("nope") })
	assert.Equal(t, "ghcr.io/anomalyco/opencode:latest", got)
}

func TestResolveBaseImageFromInlineDockerfile(t *testing.T) {
	t.Serial()
	info := ContainerInfo{
		Labels: map[string]string{
			"docker-updater.dockerfile-inline": "FROM golang:1.25 AS b\nFROM gcr.io/distroless/static:nonroot\n",
		},
	}
	got := resolveBaseImage(info, func(string) (string, error) { return "", errors.New("nope") })
	assert.Equal(t, "gcr.io/distroless/static:nonroot", got)
}

func TestResolveBaseImageFromDockerfileOnDisk(t *testing.T) {
	t.Serial()
	info := ContainerInfo{
		ComposeWorkingDir: "/srv/app",
		Labels:            map[string]string{},
	}
	readFile := func(path string) (string, error) {
		if path == "/srv/app/Dockerfile" {
			return "FROM ghcr.io/org/base:1.0\n", nil
		}
		return "", errors.New("not found")
	}
	got := resolveBaseImage(info, readFile)
	assert.Equal(t, "ghcr.io/org/base:1.0", got)
}

func TestResolveBaseImageUnresolvableReturnsEmpty(t *testing.T) {
	t.Serial()
	// No label, no readable Dockerfile, and a scratch FROM all yield "".
	info := ContainerInfo{
		ComposeWorkingDir: "/srv/app",
		Labels:            map[string]string{"docker-updater.dockerfile-inline": "FROM scratch\n"},
	}
	got := resolveBaseImage(info, func(string) (string, error) { return "", errors.New("nope") })
	assert.Equal(t, "", got)
}

func TestIsLayerPrefix(t *testing.T) {
	t.Serial()
	l := func(ids ...string) []string { return ids }
	assert.True(t, isLayerPrefix(l("a", "b"), l("a", "b", "c")), "derived extends base")
	assert.True(t, isLayerPrefix(l("a", "b"), l("a", "b")), "derived IS the base")
	assert.False(t, isLayerPrefix(l("a", "b"), l("a", "x", "c")), "diverged layers")
	assert.False(t, isLayerPrefix(l("a", "b", "c"), l("a", "b")), "derived shorter than base")
	assert.False(t, isLayerPrefix(nil, l("a")), "unknown base layers never match")
	assert.False(t, isLayerPrefix(l("a"), nil), "unknown derived layers never match")
}

// layeredFixture is a mockDocker environment for build-mode tests where the
// base image, the running container's image, and the derived tag all resolve
// to distinct inspects with real RootFS layers. Tests mutate its fields to
// simulate registry pushes, rebuilds, and recreates between cycles.
type layeredFixture struct {
	cli          *mockDocker
	baseManifest string   // manifest digest the base tag currently resolves to
	baseLayers   []string // RootFS layers of the pulled base image
	runningID    string   // image ID the service's container currently runs
	builtID      string   // image ID the derived tag currently points at
	imageLayers  map[string][]string
}

const (
	fixtureBase       = "ghcr.io/anomalyco/opencode:latest"
	fixtureDerivedTag = "opencode:local"
)

func newLayeredFixture() *layeredFixture {
	runningID := "sha256:" + strings.Repeat("1", 64)
	f := &layeredFixture{
		baseManifest: "sha256:" + strings.Repeat("a", 64),
		baseLayers:   []string{"sha256:base1", "sha256:base2"},
		runningID:    runningID,
		builtID:      runningID,
		imageLayers:  map[string][]string{},
	}
	f.cli = &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID: "svc-1",
				Labels: map[string]string{
					"com.docker.compose.project": "demo",
					"com.docker.compose.service": "opencode",
				},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: f.runningID, State: &types.ContainerState{Running: true}},
				Config:            &container.Config{Image: fixtureDerivedTag},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(`{"status":"Pull complete"}`)), nil
		},
		imageInspectFn: func(_ context.Context, ref string) (types.ImageInspect, []byte, error) {
			switch ref {
			case fixtureBase:
				return types.ImageInspect{
					ID:          "sha256:baseimage",
					RepoDigests: []string{fixtureBase + "@" + f.baseManifest},
					RootFS:      types.RootFS{Type: "layers", Layers: f.baseLayers},
				}, nil, nil
			case fixtureDerivedTag:
				return types.ImageInspect{ID: f.builtID, RootFS: types.RootFS{Type: "layers", Layers: f.imageLayers[f.builtID]}}, nil, nil
			default:
				if layers, ok := f.imageLayers[ref]; ok {
					return types.ImageInspect{ID: ref, RootFS: types.RootFS{Type: "layers", Layers: layers}}, nil, nil
				}
				return types.ImageInspect{}, nil, errors.New("no such image: " + ref)
			}
		},
	}
	return f
}

func fixtureInfo() ContainerInfo {
	return ContainerInfo{
		ID: "svc-1", Name: "opencode", Mode: UpdateModeBuild,
		Image:          fixtureDerivedTag,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: fixtureBase,
	}
}

func TestCheckBuildUpdateLayerPrefixVerifiedCurrent(t *testing.T) {
	t.Serial()
	resetBuildState()
	f := newLayeredFixture()
	// The running image extends the current base: base layers + app layers.
	f.imageLayers[f.runningID] = append(append([]string{}, f.baseLayers...), "sha256:app")

	old, newBase, verified, err := checkBuildUpdate(context.Background(), f.cli, fixtureInfo(), newAuthResolver(nil))
	require.NoError(t, err)
	assert.True(t, verified, "outcome must come from the layer check")
	assert.Equal(t, "", newBase, "an image extending the current base is up to date")
	assert.Equal(t, f.baseManifest, old)
}

func TestCheckBuildUpdateDetectsPreExistingStaleOnFirstCycle(t *testing.T) {
	t.Serial()
	// The opencode scenario: the updater (re)starts with empty in-memory
	// state while the container's image was built from a months-old base.
	// The old first-seen-digest baseline silently adopted the current
	// registry digest and reported "up-to-date" forever; the layer check
	// must flag the staleness on the very first cycle.
	resetBuildState()
	f := newLayeredFixture()
	// The running image was built from an OLD base: its layers do not start
	// with the current base's layers.
	f.imageLayers[f.runningID] = []string{"sha256:oldbase1", "sha256:oldbase2", "sha256:app"}

	old, newBase, verified, err := checkBuildUpdate(context.Background(), f.cli, fixtureInfo(), newAuthResolver(nil))
	require.NoError(t, err)
	assert.True(t, verified)
	assert.Equal(t, f.baseManifest, newBase, "pre-existing staleness must trigger a rebuild on the first cycle")
	assert.Equal(t, "", old, "the stale image's original base digest is unknown after a restart")
}

func TestCheckBuildUpdateFallbackFirstCycleSeedsNoRebuild(t *testing.T) {
	t.Serial()
	// Without layer information (image/container not inspectable), the
	// digest fallback keeps the old semantics: first cycle adopts the
	// current base as the baseline without rebuilding.
	resetBuildState()
	base := "ghcr.io/anomalyco/opencode:latest"
	manifest := "sha256:" + strings.Repeat("a", 64)
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + manifest}}, nil, nil
		},
	}
	info := ContainerInfo{ID: "c1", Name: "opencode", Mode: UpdateModeBuild, BaseImage: base}

	old, newBase, verified, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.NoError(t, err)
	assert.False(t, verified, "no layers available -> digest fallback")
	assert.Equal(t, "", newBase, "first fallback cycle adopts the baseline; no rebuild")
	assert.Equal(t, manifest, old)
}

func TestCheckBuildUpdateFallbackDetectsBaseChange(t *testing.T) {
	t.Serial()
	resetBuildState()
	base := "ghcr.io/anomalyco/opencode:latest"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	current := oldManifest
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + current}}, nil, nil
		},
	}
	info := ContainerInfo{ID: "c1", Name: "opencode", Mode: UpdateModeBuild, BaseImage: base}

	// First cycle: seed.
	_, n1, _, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.NoError(t, err)
	assert.Equal(t, "", n1)

	// Base publishes a new digest.
	current = newManifest
	o2, n2, verified, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.NoError(t, err)
	assert.False(t, verified)
	assert.Equal(t, oldManifest, o2)
	assert.Equal(t, newManifest, n2, "a changed base digest triggers a rebuild")
}

func TestRebuildAndRecreateOnlyRecreatesOnRealChange(t *testing.T) {
	t.Serial()
	resetBuildState()
	f := newLayeredFixture()
	newID := "sha256:" + strings.Repeat("2", 64)
	f.imageLayers[f.runningID] = []string{"sha256:oldbase1", "sha256:app"}
	f.imageLayers[newID] = append(append([]string{}, f.baseLayers...), "sha256:app")
	// The rebuild re-points the derived tag at a new image; the container
	// keeps running the old one until `up -d`.
	runner := &fakeComposeRunner{onBuild: func() { f.builtID = newID }}

	changed, err := rebuildAndRecreate(context.Background(), f.cli, runner, fixtureInfo(), "sha256:newbase", true)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 1, runner.upCalls, "a real image change recreates the service")
	assert.False(t, isPrefixIncapable(buildStateKey(fixtureInfo())), "a rebuild that extends the base keeps the layer check enabled")
}

func TestRebuildAndRecreateNoChurnOnCacheHit(t *testing.T) {
	t.Serial()
	resetBuildState()
	// The derived tag points at the identical image after the rebuild (cache
	// hit): the service must NOT be recreated.
	f := newLayeredFixture()
	f.imageLayers[f.runningID] = append(append([]string{}, f.baseLayers...), "sha256:app")
	runner := &fakeComposeRunner{}

	changed, err := rebuildAndRecreate(context.Background(), f.cli, runner, fixtureInfo(), "sha256:newbase", false)
	require.NoError(t, err)
	assert.False(t, changed, "a cache-hit rebuild yields no recreate")
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 0, runner.upCalls, "identical image ID must not recreate the service")
}

func TestRebuildMarksPrefixIncapableWhenRebuiltImageStillNotFromBase(t *testing.T) {
	t.Serial()
	resetBuildState()
	f := newLayeredFixture()
	newID := "sha256:" + strings.Repeat("2", 64)
	// Even the freshly rebuilt image does not start with the base's layers
	// (e.g. FROM scratch + COPY --from=stage).
	f.imageLayers[f.runningID] = []string{"sha256:artifact1"}
	f.imageLayers[newID] = []string{"sha256:artifact2"}
	runner := &fakeComposeRunner{onBuild: func() { f.builtID = newID }}

	changed, err := rebuildAndRecreate(context.Background(), f.cli, runner, fixtureInfo(), "sha256:newbase", true)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, runner.upCalls)
	assert.True(t, isPrefixIncapable(buildStateKey(fixtureInfo())), "a completed rebuild that still fails the prefix check must switch to digest tracking")
}

func TestBuildPrefixIncapableFallsBackWithoutRebuildLoop(t *testing.T) {
	t.Serial()
	// A build whose final stage never extends the base (FROM scratch + COPY
	// --from) can never satisfy the layer check. The first rebuild proves
	// that (cache hit, image unchanged), after which the service must fall
	// back to digest tracking instead of rebuilding every cycle.
	resetBuildState()
	f := newLayeredFixture()
	f.imageLayers[f.runningID] = []string{"sha256:artifact"}
	runner := &fakeComposeRunner{}
	info := fixtureInfo()
	cfg := Config{Label: "docker-updater.enable"}

	// Cycle 1: layer check says stale -> rebuild -> cache hit -> marked
	// prefix-incapable, baseline recorded.
	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 0, runner.upCalls, "cache hit must not recreate")
	assert.True(t, isPrefixIncapable(buildStateKey(info)))

	// Cycles 2 and 3: digest unchanged -> no rebuild. The loop is broken.
	for cycle := 2; cycle <= 3; cycle++ {
		r = checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
		require.NoError(t, r.Error)
		assert.False(t, r.Updated)
		assert.Equal(t, 1, runner.buildCalls, "cycle %d must not rebuild again", cycle)
	}

	// The base genuinely advances: the digest fallback still catches it.
	f.baseManifest = "sha256:" + strings.Repeat("b", 64)
	r = checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.True(t, r.Updated)
	assert.Equal(t, 2, runner.buildCalls, "a real digest change still rebuilds")

	// And the new digest is recorded: the next cycle is quiet again.
	r = checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.Equal(t, 2, runner.buildCalls)
}

func TestCheckAndUpdateBuildRescuesStaleContainerOnFirstCycle(t *testing.T) {
	t.Serial()
	// End-to-end opencode scenario: fresh updater process, container built
	// from an old base, registry already many releases ahead. The first
	// cycle must rebuild and recreate.
	resetBuildState()
	f := newLayeredFixture()
	newID := "sha256:" + strings.Repeat("2", 64)
	f.imageLayers[f.runningID] = []string{"sha256:oldbase1", "sha256:oldbase2", "sha256:app"}
	f.imageLayers[newID] = append(append([]string{}, f.baseLayers...), "sha256:app")
	runner := &fakeComposeRunner{onBuild: func() { f.builtID = newID }}
	info := fixtureInfo()
	cfg := Config{Label: "docker-updater.enable"}

	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.True(t, r.Updated)
	assert.Equal(t, f.baseManifest, r.NewRef)
	assert.Equal(t, 1, runner.buildCalls, "pre-existing staleness rebuilds on the first cycle")
	assert.Equal(t, 1, runner.upCalls)

	// After the rebuild the running image extends the base: the next cycle
	// verifies it as current without rebuilding.
	f.runningID = newID
	r = checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.False(t, r.Updated)
	assert.Equal(t, 1, runner.buildCalls, "a current image must not rebuild")
	assert.False(t, isPrefixIncapable(buildStateKey(info)))
}

func TestCheckAndUpdateBuildDryRunMutatesNothing(t *testing.T) {
	t.Serial()
	resetBuildState()
	base := "ghcr.io/anomalyco/opencode:latest"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	current := oldManifest
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + current}}, nil, nil
		},
	}
	info := ContainerInfo{
		ID: "c1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: base,
	}
	runner := &fakeComposeRunner{}
	cfg := Config{Label: "docker-updater.enable", DryRun: true}

	// Seed.
	r := checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info, DryRun: true}, newAuthResolver(nil))
	assert.False(t, r.Updated)

	// Base changes; dry-run should report Updated but call NO compose ops.
	current = newManifest
	r = checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info, DryRun: true}, newAuthResolver(nil))
	assert.True(t, r.Updated)
	assert.True(t, r.DryRun)
	assert.Equal(t, newManifest, r.NewRef)
	assert.Equal(t, 0, runner.buildCalls, "dry-run must not build")
	assert.Equal(t, 0, runner.upCalls, "dry-run must not recreate")

	// Crucially, dry-run must not advance the baseline: the same update is still
	// pending on the next cycle.
	r = checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info, DryRun: true}, newAuthResolver(nil))
	assert.True(t, r.Updated, "dry-run did not consume the update; it stays pending")
	assert.Equal(t, 0, runner.buildCalls)
}

func TestCheckAndUpdateBuildAppliesRebuild(t *testing.T) {
	t.Serial()
	resetBuildState()
	f := newLayeredFixture()
	newID := "sha256:" + strings.Repeat("2", 64)
	f.imageLayers[f.runningID] = append(append([]string{}, f.baseLayers...), "sha256:app")
	runner := &fakeComposeRunner{onBuild: func() { f.builtID = newID }}
	info := fixtureInfo()
	cfg := Config{Label: "docker-updater.enable"}

	// First cycle: the running image extends the base; nothing to do.
	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.False(t, r.Updated)
	assert.Equal(t, 0, runner.buildCalls)

	// The base publishes new layers -> rebuild + recreate.
	oldManifest := f.baseManifest
	newManifest := "sha256:" + strings.Repeat("b", 64)
	f.baseManifest = newManifest
	f.baseLayers = []string{"sha256:newbase1", "sha256:newbase2"}
	f.imageLayers[newID] = append(append([]string{}, f.baseLayers...), "sha256:app")

	r = checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NoError(t, r.Error)
	assert.True(t, r.Updated)
	assert.Equal(t, oldManifest, r.OldRef, "the previously verified base digest is reported as the old ref")
	assert.Equal(t, newManifest, r.NewRef)
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 1, runner.upCalls)
}

func TestCheckAndUpdateBuildSkipsWhenNoBaseImage(t *testing.T) {
	t.Serial()
	resetBuildState()
	cli := &mockDocker{}
	runner := &fakeComposeRunner{}
	info := ContainerInfo{ID: "c1", Name: "local", Mode: UpdateModeBuild, BaseImage: ""}
	cfg := Config{Label: "docker-updater.enable"}

	r := checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	assert.True(t, r.Skipped)
	assert.Nil(t, r.Error)
	assert.Equal(t, 0, runner.buildCalls)
}

func TestCheckAndUpdateBuildPreCheckSkips(t *testing.T) {
	t.Serial()
	resetBuildState()

	// A pre-check HTTP endpoint that always refuses (503) holds the rebuild back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	f := newLayeredFixture()
	f.imageLayers[f.runningID] = []string{"sha256:oldbase1", "sha256:app"}
	runner := &fakeComposeRunner{}
	info := fixtureInfo()
	info.PreCheckURL = srv.URL
	info.PreCheckTimeout = 2 * time.Second
	cfg := Config{Label: "docker-updater.enable"}

	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	assert.True(t, r.Skipped, "a failing pre-check holds the rebuild back")
	assert.NotEmpty(t, r.SkipReason)
	assert.Equal(t, f.baseManifest, r.NewRef, "the update is reported as available")
	assert.Equal(t, 0, runner.buildCalls, "pre-check failure must not build")
}

func TestCheckAndUpdateBuildReportsBuildError(t *testing.T) {
	t.Serial()
	resetBuildState()
	f := newLayeredFixture()
	f.imageLayers[f.runningID] = []string{"sha256:oldbase1", "sha256:app"}
	runner := &fakeComposeRunner{buildErr: errors.New("compose build exploded")}
	info := fixtureInfo()
	cfg := Config{Label: "docker-updater.enable"}

	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.Error(t, r.Error)
	assert.False(t, r.Updated)
	assert.Equal(t, f.baseManifest, r.NewRef, "the available update is still reported on the dashboard")
}

func TestCheckAndUpdateBuildPullErrorPropagates(t *testing.T) {
	t.Serial()
	// A mid-stream pull failure (in-band error record) must surface as a
	// check error -- never as a silent "up-to-date".
	resetBuildState()
	f := newLayeredFixture()
	f.imageLayers[f.runningID] = append(append([]string{}, f.baseLayers...), "sha256:app")
	f.cli.imagePullFn = func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
		stream := `{"status":"Pulling from anomalyco/opencode"}` + "\n" +
			`{"errorDetail":{"message":"toomanyrequests: rate limit exceeded"},"error":"toomanyrequests: rate limit exceeded"}` + "\n"
		return io.NopCloser(strings.NewReader(stream)), nil
	}
	runner := &fakeComposeRunner{}
	info := fixtureInfo()
	cfg := Config{Label: "docker-updater.enable"}

	r := checkAndUpdateBuild(context.Background(), f.cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.Error(t, r.Error)
	assert.Contains(t, r.Error.Error(), "toomanyrequests")
	assert.Equal(t, 0, runner.buildCalls, "a failed pull must not trigger a rebuild")
}

func TestCheckAndUpdateBuildNeverPullsDerivedTag(t *testing.T) {
	t.Serial()
	resetBuildState()
	base := "ghcr.io/anomalyco/opencode:latest"
	var pulled []string
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
			pulled = append(pulled, ref)
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}
	info := ContainerInfo{
		ID: "c1", Name: "opencode", Mode: UpdateModeBuild,
		Image:          "opencode:local", // the local derived tag
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: base,
	}
	cfg := Config{Label: "docker-updater.enable"}
	runner := &fakeComposeRunner{}

	checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))

	for _, p := range pulled {
		assert.NotEqual(t, "opencode:local", p, "build mode must never pull the local derived tag")
	}
	assert.Contains(t, pulled, base, "build mode pulls the base image")
}

func TestBuildStateKeySurvivesContainerRecreation(t *testing.T) {
	t.Serial()
	before := fixtureInfo()
	after := fixtureInfo()
	after.ID = "svc-2" // compose recreate replaced the container
	assert.Equal(t, buildStateKey(before), buildStateKey(after), "recorded state must survive a compose recreate")

	bare := ContainerInfo{ID: "c9", BaseImage: fixtureBase}
	assert.Contains(t, buildStateKey(bare), "c9", "non-compose containers key by ID")
}
