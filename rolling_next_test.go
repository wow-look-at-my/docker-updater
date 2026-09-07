package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRollingBaseName(t *testing.T) {
	base, ok := rollingBaseName("buildhost-next")
	assert.True(t, ok)
	assert.Equal(t, "buildhost", base)

	_, ok = rollingBaseName("buildhost")
	assert.False(t, ok)
	_, ok = rollingBaseName("-next")
	assert.False(t, ok, "a bare suffix names no container")
}

// The scratch container carries the opt-in label, because it is created from
// the config of the container it replaces. Adopting it as a target is how the
// updater came to be building a "-next-next".
func TestListMonitoredSkipsTheRollingScratchContainer(t *testing.T) {
	t.Serial()
	labels := map[string]string{"docker-updater.enable": "true"}
	cli := &mockDocker{
		containerListFn: func(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
			require.True(t, opts.All, "a crash-looping container is the one that most needs an update")
			return []types.Container{
				{ID: "a", Names: []string{"/buildhost"}, Image: "ghcr.io/acme/buildhost:latest", Labels: labels},
				{ID: "b", Names: []string{"/buildhost-next"}, Image: "ghcr.io/acme/buildhost:latest", Labels: labels},
				// An operator's own container that ends the same way, with no
				// container of the base name behind it.
				{ID: "c", Names: []string{"/queue-next"}, Image: "ghcr.io/acme/queue:latest", Labels: labels},
			}, nil
		},
		// An image reference that resolves, or every container is skipped for a
		// reason that has nothing to do with what this test asks.
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			image := "ghcr.io/acme/buildhost:latest"
			if id == "c" {
				image = "ghcr.io/acme/queue:latest"
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:" + id},
				Config:            &container.Config{Image: image},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{RepoDigests: []string{"ghcr.io/acme/buildhost@sha256:abc"}}, nil, nil
		},
	}

	got, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.NoError(t, err)

	names := make([]string, 0, len(got))
	for _, c := range got {
		names = append(names, c.Name)
	}
	assert.ElementsMatch(t, []string{"buildhost", "queue-next"}, names,
		"only the transient beside a container that is still here is skipped")
}

// The wedge: a next container that failed to start is RESTARTING, a plain
// remove refuses it, the name stays taken, and every later cycle dies on the
// conflict without reaching a create.
func TestRollingUpdateClearsAStaleNextContainer(t *testing.T) {
	t.Serial()
	var removed []string
	var forced []bool
	creates := 0

	cli := &mockDocker{
		containerInspectFn: wedgedInspect(nil),
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{Config: &container.Config{}}, nil, nil
		},
		containerRemoveFn: func(_ context.Context, id string, opts container.RemoveOptions) error {
			removed = append(removed, id)
			forced = append(forced, opts.Force)
			return nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			creates++
			require.Equal(t, "myapp-next", name)
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.NoError(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))

	require.NotEmpty(t, removed)
	assert.Equal(t, "myapp-next", removed[0], "the stale name is cleared before the create needs it")
	assert.True(t, forced[0], "a restarting container refuses a plain remove")
	assert.Equal(t, 1, creates)
}

// A next container that fails its health check is removed with force too, so
// the name is free for the cycle after this one.
func TestRollingUpdateForceRemovesAFailedNextContainer(t *testing.T) {
	t.Serial()
	var forcedIDs []string
	cli := &mockDocker{
		containerInspectFn: wedgedInspect(nil),
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{Config: &container.Config{}}, nil, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			return errors.New("exec /nope: no such file or directory")
		},
		containerRemoveFn: func(_ context.Context, id string, opts container.RemoveOptions) error {
			if opts.Force {
				forcedIDs = append(forcedIDs, id)
			}
			return nil
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.Error(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))
	assert.Contains(t, forcedIDs, "new123456789", "the container that would not start is force-removed")
}
