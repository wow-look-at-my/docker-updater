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
	onBuild    func() // hook to mutate fixture state between build and the ID re-read
}

func (f *fakeComposeRunner) Build(_ context.Context, _ []string, _, _ string) error {
	f.buildCalls++
	if f.onBuild != nil {
		f.onBuild()
	}
	return f.buildErr
}

func (f *fakeComposeRunner) Up(_ context.Context, _ []string, _, _ string) error {
	f.upCalls++
	return f.upErr
}

// resetBaseDigestStore clears the per-container baseline between tests so they
// don't share state.
func resetBaseDigestStore() {
	baseDigestStore.Lock()
	baseDigestStore.digests = make(map[string]string)
	baseDigestStore.Unlock()
}

func TestResolveBaseImageExplicitLabelWins(t *testing.T) {
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
	info := ContainerInfo{
		Labels: map[string]string{
			"docker-updater.dockerfile-inline": "FROM golang:1.25 AS b\nFROM gcr.io/distroless/static:nonroot\n",
		},
	}
	got := resolveBaseImage(info, func(string) (string, error) { return "", errors.New("nope") })
	assert.Equal(t, "gcr.io/distroless/static:nonroot", got)
}

func TestResolveBaseImageFromDockerfileOnDisk(t *testing.T) {
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
	// No label, no readable Dockerfile, and a scratch FROM all yield "".
	info := ContainerInfo{
		ComposeWorkingDir: "/srv/app",
		Labels:            map[string]string{"docker-updater.dockerfile-inline": "FROM scratch\n"},
	}
	got := resolveBaseImage(info, func(string) (string, error) { return "", errors.New("nope") })
	assert.Equal(t, "", got)
}

func TestCheckBuildUpdateFirstCycleSeedsNoRebuild(t *testing.T) {
	resetBaseDigestStore()
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

	old, newBase, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "", newBase, "first cycle adopts the baseline; no rebuild")
	assert.Equal(t, manifest, old)
}

func TestCheckBuildUpdateDetectsBaseChange(t *testing.T) {
	resetBaseDigestStore()
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
	_, n1, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "", n1)

	// Base publishes a new digest.
	current = newManifest
	o2, n2, err := checkBuildUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, oldManifest, o2)
	assert.Equal(t, newManifest, n2, "a changed base digest triggers a rebuild")
}

func TestRebuildAndRecreateOnlyRecreatesOnRealChange(t *testing.T) {
	resetBaseDigestStore()
	// The derived image ID changes across the rebuild, so the service is
	// recreated (up -d called).
	oldID := "sha256:" + strings.Repeat("1", 64)
	newID := "sha256:" + strings.Repeat("2", 64)
	currentID := oldID
	cli := &mockDocker{
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
				ContainerJSONBase: &types.ContainerJSONBase{Image: currentID, State: &types.ContainerState{Running: true}},
			}, nil
		},
	}
	runner := &fakeComposeRunner{onBuild: func() { currentID = newID }}
	info := ContainerInfo{
		ID: "svc-1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: "ghcr.io/anomalyco/opencode:latest",
	}

	changed, err := rebuildAndRecreate(context.Background(), cli, runner, info, "sha256:newbase")
	require.Nil(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 1, runner.upCalls, "a real image change recreates the service")
}

func TestRebuildAndRecreateNoChurnOnCacheHit(t *testing.T) {
	resetBaseDigestStore()
	// The derived image ID is identical after the rebuild (cache hit): the
	// service must NOT be recreated.
	sameID := "sha256:" + strings.Repeat("3", 64)
	cli := &mockDocker{
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
				ContainerJSONBase: &types.ContainerJSONBase{Image: sameID},
			}, nil
		},
	}
	runner := &fakeComposeRunner{}
	info := ContainerInfo{
		ID: "svc-1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: "ghcr.io/anomalyco/opencode:latest",
	}

	changed, err := rebuildAndRecreate(context.Background(), cli, runner, info, "sha256:newbase")
	require.Nil(t, err)
	assert.False(t, changed, "a cache-hit rebuild yields no recreate")
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 0, runner.upCalls, "identical image ID must not recreate the service")
}

func TestCheckAndUpdateBuildDryRunMutatesNothing(t *testing.T) {
	resetBaseDigestStore()
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
	resetBaseDigestStore()
	base := "ghcr.io/anomalyco/opencode:latest"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	currentBase := oldManifest
	oldID := "sha256:" + strings.Repeat("1", 64)
	newID := "sha256:" + strings.Repeat("2", 64)
	currentID := oldID

	cli := &mockDocker{
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
				ContainerJSONBase: &types.ContainerJSONBase{Image: currentID, State: &types.ContainerState{Running: true}},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, ref string) (types.ImageInspect, []byte, error) {
			// Base-image inspect (build_checker pulls the base).
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + currentBase}}, nil, nil
		},
	}
	runner := &fakeComposeRunner{onBuild: func() { currentID = newID }}
	info := ContainerInfo{
		ID: "svc-1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: base,
	}
	cfg := Config{Label: "docker-updater.enable"}

	// Seed.
	checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))

	// Base advances → rebuild + recreate.
	currentBase = newManifest
	r := checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.Nil(t, r.Error)
	assert.True(t, r.Updated)
	assert.Equal(t, oldManifest, r.OldRef)
	assert.Equal(t, newManifest, r.NewRef)
	assert.Equal(t, 1, runner.buildCalls)
	assert.Equal(t, 1, runner.upCalls)
}

func TestCheckAndUpdateBuildSkipsWhenNoBaseImage(t *testing.T) {
	resetBaseDigestStore()
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
	resetBaseDigestStore()
	base := "ghcr.io/anomalyco/opencode:latest"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	current := oldManifest

	// A pre-check HTTP endpoint that always refuses (503) holds the rebuild back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + current}}, nil, nil
		},
	}
	runner := &fakeComposeRunner{}
	info := ContainerInfo{
		ID: "c1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage:       base,
		PreCheckURL:     srv.URL,
		PreCheckTimeout: 2 * time.Second,
	}
	cfg := Config{Label: "docker-updater.enable"}

	// Seed, then advance the base so a rebuild is warranted.
	checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	current = newManifest

	r := checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	assert.True(t, r.Skipped, "a failing pre-check holds the rebuild back")
	assert.NotEmpty(t, r.SkipReason)
	assert.Equal(t, newManifest, r.NewRef, "the update is reported as available")
	assert.Equal(t, 0, runner.buildCalls, "pre-check failure must not build")
}

func TestCheckAndUpdateBuildReportsBuildError(t *testing.T) {
	resetBaseDigestStore()
	base := "ghcr.io/anomalyco/opencode:latest"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	current := oldManifest

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "svc-1",
				Labels: map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "opencode"},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:derived"}}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:base", RepoDigests: []string{base + "@" + current}}, nil, nil
		},
	}
	runner := &fakeComposeRunner{buildErr: errors.New("compose build exploded")}
	info := ContainerInfo{
		ID: "svc-1", Name: "opencode", Mode: UpdateModeBuild,
		ComposeProject: "demo", ComposeService: "opencode",
		ComposeConfigFiles: "/srv/demo/docker-compose.yml", ComposeWorkingDir: "/srv/demo",
		BaseImage: base,
	}
	cfg := Config{Label: "docker-updater.enable"}

	checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	current = newManifest

	r := checkAndUpdateBuild(context.Background(), cli, runner, info, cfg, UpdateResult{Container: info}, newAuthResolver(nil))
	require.NotNil(t, r.Error)
	assert.False(t, r.Updated)
	assert.Equal(t, newManifest, r.NewRef, "the available update is still reported on the dashboard")
}

func TestCheckAndUpdateBuildNeverPullsDerivedTag(t *testing.T) {
	resetBaseDigestStore()
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
