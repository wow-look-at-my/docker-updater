package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driftedContainer is a container configured with a registry image and started
// from a local build of it: Config.Image names the registry, but the running
// image carries no RepoDigests. This is the s3 container that sat unupdated for
// three days while the registry answered every manifest request.
func driftedContainer(pullErr error) (*mockDocker, *[]string) {
	repo := "oci.pazer.build/go-s3-server"
	localID := "sha256:" + strings.Repeat("4", 64)
	manifest := "sha256:" + strings.Repeat("5", 64)
	var pulled []string
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "s3-1",
				Names:  []string{"/s3"},
				Image:  localID,
				State:  "running",
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: localID},
				Config:            &container.Config{Image: repo + ":latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, ref string) (types.ImageInspect, []byte, error) {
			// The running image is the local CI build; the tag, once pulled,
			// resolves to the registry manifest.
			if ref == localID || len(pulled) == 0 {
				return types.ImageInspect{ID: localID, RepoTags: []string{"go-s3-server:ci-verify"}, RepoDigests: []string{}}, nil, nil
			}
			return types.ImageInspect{ID: "sha256:" + strings.Repeat("6", 64), RepoDigests: []string{repo + "@" + manifest}}, nil, nil
		},
		imagePullFn: func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
			if pullErr != nil {
				return nil, pullErr
			}
			pulled = append(pulled, ref)
			return io.NopCloser(strings.NewReader(`{"status":"Pull complete"}`)), nil
		},
	}
	return cli, &pulled
}

// The registry decides whether an image is pullable, not the running image's
// provenance. A configured reference that resolves is an update waiting to be
// applied, however the running image got there.
func TestImageModeUpdatesADriftedLocalBuild(t *testing.T) {
	cli, pulled := driftedContainer(nil)

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Len(t, containers, 1)
	assert.Empty(t, containers[0].Unmonitorable, "a registry reference is never judged local from RepoDigests")
	assert.Equal(t, "oci.pazer.build/go-s3-server:latest", containers[0].Image)

	results := runUpdateCheck(context.Background(), cli, Config{Label: "docker-updater.enable", DryRun: true}, newAuthResolver(nil))
	require.Len(t, results, 1)
	assert.Equal(t, []string{"oci.pazer.build/go-s3-server:latest"}, *pulled, "the configured reference is what the registry is asked about")
	assert.Nil(t, results[0].Error)
	assert.NotEmpty(t, results[0].NewRef, "a running image that differs from what the tag resolves to is an update")
	assert.True(t, results[0].Updated)
}

// When the registry itself says the image is absent, the report carries the
// registry's own words, and only then is build mode named as a remedy.
func TestImageModePullFailureNamesTheRegistryError(t *testing.T) {
	cli, _ := driftedContainer(errors.New("pull access denied for go-s3-server, repository does not exist or may require 'docker login'"))

	results := runUpdateCheck(context.Background(), cli, Config{Label: "docker-updater.enable", DryRun: true}, newAuthResolver(nil))
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "repository does not exist")
	assert.Contains(t, results[0].Error.Error(), "docker-updater.mode=build")
	assert.Empty(t, results[0].Container.Unmonitorable, "a failed check is an error that the next cycle retries")
}

// A registry that cannot be reached is not a missing image: sending the
// operator to build mode would hide a network fault behind a config change.
func TestImageModePullFailureOnUnreachableRegistryDoesNotSuggestBuildMode(t *testing.T) {
	cli, _ := driftedContainer(errors.New(`Get "https://oci.pazer.build/v2/": dial tcp: lookup oci.pazer.build: no such host`))

	results := runUpdateCheck(context.Background(), cli, Config{Label: "docker-updater.enable", DryRun: true}, newAuthResolver(nil))
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "no such host")
	assert.NotContains(t, results[0].Error.Error(), "mode=build")
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

// A digest-pinned reference with no RepoDigests recorded is monitored like any
// other registry reference.
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
	assert.Empty(t, containers[0].Unmonitorable)
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
