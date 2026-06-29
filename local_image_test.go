package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageModeSkipsLocallyBuiltImage covers the container-mode skip-guard: an
// image-mode container whose running image has no RepoDigests (locally built,
// no registry origin, e.g. a compose `build:` tag like `opencode:local`) must
// be detected and skipped -- never pulled -- with an actionable warning.
func TestImageModeSkipsLocallyBuiltImage(t *testing.T) {
	pullCalled := false
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "local-1",
				Names:  []string{"/opencode"},
				Image:  "opencode:local",
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:" + strings.Repeat("9", 64)},
				Config:            &container.Config{Image: "opencode:local"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			// Locally built: a real tag, but NO RepoDigests.
			return types.ImageInspect{ID: "sha256:" + strings.Repeat("9", 64), RepoDigests: []string{}}, nil, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			pullCalled = true
			return io.NopCloser(strings.NewReader("")), nil
		},
	}

	// At listing time the locally-built container is filtered out (skipped).
	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	assert.Equal(t, 0, len(containers), "a locally-built image in image mode is skipped")

	// And a full cycle never attempts a registry pull for it.
	results := runUpdateCheck(context.Background(), cli, Config{Label: "docker-updater.enable", DryRun: true}, newAuthResolver(nil))
	assert.Equal(t, 0, len(results))
	assert.False(t, pullCalled, "the locally-built local tag must never be pulled")
}

// TestImageModeRegistryImageStillIncluded proves backward compatibility: a real
// registry image (RepoDigests present) is still monitored in image mode.
func TestImageModeRegistryImageStillIncluded(t *testing.T) {
	repo := "ghcr.io/wow-look-at-my/buildhost"
	manifest := "sha256:" + strings.Repeat("a", 64)
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "bh-1",
				Names:  []string{"/buildhost"},
				Image:  repo + ":latest",
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:img"},
				Config:            &container.Config{Image: repo + ":latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:img", RepoDigests: []string{repo + "@" + manifest}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))
	assert.Equal(t, repo+":latest", containers[0].Image)
	assert.Equal(t, UpdateModeImage, containers[0].Mode)
}

// TestImageModeDigestPinnedNotSkipped proves a digest-pinned reference (already
// canonical) is exempt from the skip-guard even with no RepoDigests recorded.
func TestImageModeDigestPinnedNotSkipped(t *testing.T) {
	repo := "ghcr.io/org/app"
	pinned := repo + "@sha256:" + strings.Repeat("c", 64)
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "p-1",
				Names:  []string{"/app"},
				Image:  pinned,
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:img"},
				Config:            &container.Config{Image: pinned},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:img", RepoDigests: []string{}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers), "a digest-pinned ref is pullable and not skipped")
}

// TestListMonitoredContainersBuildMode covers build-mode label parsing: the
// compose project/service/config-files/working-dir labels and base-image
// resolution.
func TestListMonitoredContainersBuildMode(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:    "build-1",
				Names: []string{"/opencode"},
				Image: "opencode:local",
				Labels: map[string]string{
					"docker-updater.enable":                   "true",
					"docker-updater.mode":                     "build",
					"docker-updater.base-image":               "ghcr.io/anomalyco/opencode:latest",
					"com.docker.compose.project":              "demo",
					"com.docker.compose.service":              "opencode",
					"com.docker.compose.project.config_files": "/srv/demo/docker-compose.yml",
					"com.docker.compose.project.working_dir":  "/srv/demo",
				},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:derived"},
				Config:            &container.Config{Image: "opencode:local"},
			}, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, UpdateModeBuild, c.Mode)
	assert.Equal(t, "demo", c.ComposeProject)
	assert.Equal(t, "opencode", c.ComposeService)
	assert.Equal(t, "/srv/demo/docker-compose.yml", c.ComposeConfigFiles)
	assert.Equal(t, "/srv/demo", c.ComposeWorkingDir)
	assert.Equal(t, "ghcr.io/anomalyco/opencode:latest", c.BaseImage)
	// Build mode never resolves the local derived tag to a registry ref; the
	// running image content ID is the recorded digest.
	assert.Equal(t, "sha256:derived", c.ImageDigest)
}
