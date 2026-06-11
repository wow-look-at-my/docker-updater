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

// These tests cover the robustness fix for containers whose running image has
// lost its RepoTags. Docker's container-list view then degrades the Image
// field to a bare image ID ("sha256:..."), which is not a pullable registry
// reference. The updater must resolve the reference from the container's
// Config.Image (or RepoDigests) instead, and never pull a bare image ID.

func TestListMonitoredContainersUntaggedRunningImage(t *testing.T) {
	// Reproduces the production failure: RepoTags is empty, but RepoDigests and
	// a tagged Config.Image are present. The resolved pull reference must be the
	// repo:tag, never the bare image ID.
	bareID := "sha256:" + strings.Repeat("3", 64)
	runningManifest := "sha256:" + strings.Repeat("a", 64)
	repoDigest := "ghcr.io/wow-look-at-my/buildhost@" + runningManifest

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:     "buildhost-1",
					Names:  []string{"/buildhost"},
					Image:  bareID, // degraded to a bare image ID by the daemon
					Labels: map[string]string{"docker-updater.enable": "true"},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: bareID},
				Config:            &container.Config{Image: "ghcr.io/wow-look-at-my/buildhost:latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{
				ID:          bareID,
				RepoTags:    []string{}, // untagged
				RepoDigests: []string{repoDigest},
			}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, "ghcr.io/wow-look-at-my/buildhost:latest", c.Image)
	assert.NotEqual(t, bareID, c.Image)
	// The running digest is the manifest digest recovered from RepoDigests,
	// not the bare content ID, and does not require a tag.
	assert.Equal(t, runningManifest, c.ImageDigest)
}

func TestListMonitoredContainersUnresolvableSkipped(t *testing.T) {
	// A locally-built image with no registry origin: Config.Image is a bare ID
	// and there are no RepoDigests. It cannot be polled and must be skipped
	// without erroring the whole loop.
	bareID := "sha256:" + strings.Repeat("4", 64)
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:     "local-1",
					Names:  []string{"/local"},
					Image:  bareID,
					Labels: map[string]string{"docker-updater.enable": "true"},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: bareID},
				Config:            &container.Config{Image: bareID},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: bareID, RepoTags: []string{}, RepoDigests: []string{}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	assert.Equal(t, 0, len(containers))
}

func TestListMonitoredContainersConfigImageBareFallsBackToRepoDigest(t *testing.T) {
	// Config.Image is itself a bare ID (container created from a raw image ID),
	// but the running image carries RepoDigests, so the repository is recovered.
	bareID := "sha256:" + strings.Repeat("5", 64)
	manifest := "sha256:" + strings.Repeat("b", 64)
	repoDigest := "ghcr.io/wow-look-at-my/buildhost@" + manifest

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:     "buildhost-2",
					Names:  []string{"/buildhost"},
					Image:  bareID,
					Labels: map[string]string{"docker-updater.enable": "true"},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: bareID},
				Config:            &container.Config{Image: bareID},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: bareID, RepoDigests: []string{repoDigest}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))
	assert.Equal(t, repoDigest, containers[0].Image)
}

func TestPullImageRejectsBareImageID(t *testing.T) {
	// No code path may pass a bare sha256 image ID to a registry pull.
	pullCalled := false
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			pullCalled = true
			return io.NopCloser(strings.NewReader("")), nil
		},
	}

	bareID := "sha256:" + strings.Repeat("3", 64)
	_, _, err := pullImage(context.Background(), cli, bareID, newAuthResolver(nil))
	require.NotNil(t, err)
	assert.False(t, pullCalled, "daemon pull must never be invoked for a bare image ID")
}

func TestRunUpdateCheckUntaggedImageUpdates(t *testing.T) {
	// End-to-end coverage of the production scenario: the running image lost its
	// RepoTags (bare ID in the list view), but the container is still checked
	// correctly and an update is detected when the registry tag advances. DryRun
	// avoids the recreate handoff.
	bareID := "sha256:" + strings.Repeat("3", 64)
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)
	repo := "ghcr.io/wow-look-at-my/buildhost"

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:     "buildhost-1",
					Names:  []string{"/buildhost"},
					Image:  bareID,
					Labels: map[string]string{"docker-updater.enable": "true"},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: bareID},
				Config:            &container.Config{Image: repo + ":latest"},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, ref string) (types.ImageInspect, []byte, error) {
			if strings.HasPrefix(ref, "sha256:") {
				// Running image inspect during listing: untagged, old manifest.
				return types.ImageInspect{ID: bareID, RepoTags: []string{}, RepoDigests: []string{repo + "@" + oldManifest}}, nil, nil
			}
			// Freshly pulled image for the tag: the tag has advanced.
			return types.ImageInspect{ID: "sha256:" + strings.Repeat("c", 64), RepoDigests: []string{repo + "@" + newManifest}}, nil, nil
		},
	}

	cfg := Config{Label: "docker-updater.enable", DryRun: true}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))

	require.Equal(t, 1, len(results))
	assert.Nil(t, results[0].Error)
	assert.True(t, results[0].Updated)
	assert.Equal(t, oldManifest, results[0].OldRef)
	assert.Equal(t, newManifest, results[0].NewRef)
}
