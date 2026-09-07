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

func TestResetToImageDefaults(t *testing.T) {
	cfg := &container.Config{
		Entrypoint:  []string{"/myapp"},
		Cmd:         []string{"serve"},
		User:        "nonroot",
		WorkingDir:  "/data",
		Healthcheck: &container.HealthConfig{Test: []string{"CMD", "/myapp", "healthcheck"}},
		Env:         []string{"KEEP=me"},
		Image:       "myapp:latest",
	}
	assert.True(t, resetToImageDefaults(cfg))
	assert.Nil(t, cfg.Entrypoint)
	assert.Nil(t, cfg.Cmd)
	assert.Empty(t, cfg.User)
	assert.Empty(t, cfg.WorkingDir)
	assert.Nil(t, cfg.Healthcheck)
	// Env and Image are not process fields: the daemon unions Env with the
	// image's, and Image is the whole point of the update.
	assert.Equal(t, []string{"KEEP=me"}, cfg.Env)
	assert.Equal(t, "myapp:latest", cfg.Image)

	assert.False(t, resetToImageDefaults(cfg), "nothing left to strip")
	assert.False(t, resetToImageDefaults(nil))
}

// wedgedInspect answers the first inspect with a container whose entrypoint an
// operator set by hand, so clearInheritedImageDefaults cannot attribute it, and
// answers every later inspect as healthy.
func wedgedInspect(entrypoint []string) func(context.Context, string) (types.ContainerJSON, error) {
	n := 0
	return func(_ context.Context, _ string) (types.ContainerJSON, error) {
		n++
		if n == 1 {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					Image:      "sha256:olddigest",
					HostConfig: &container.HostConfig{},
				},
				Config: &container.Config{Image: "myapp:latest", Entrypoint: entrypoint},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"internal": {Aliases: []string{"backend"}},
					},
				},
			}, nil
		}
		return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{
			State: &types.ContainerState{Running: true, Health: &types.Health{Status: "healthy"}},
		}}, nil
	}
}

// The wedge this fixes: the container carries an entrypoint the new image does
// not have, and it is not the old image's either, so clearing what the old image
// supplied leaves it in place. The kernel cannot exec it. Without the retry the
// updater rolls back on this cycle and every later one, and the deployment stays
// on the version it already runs.
func TestRollingUpdateRetriesOnTheImagesOwnDefaults(t *testing.T) {
	t.Serial()
	var configs []container.Config
	starts := 0

	cli := &mockDocker{
		containerInspectFn: wedgedInspect([]string{"/gone-from-this-image"}),
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			// The old image's entrypoint differs, so nothing is cleared.
			return types.ImageInspect{Config: &container.Config{Entrypoint: []string{"/myapp"}}}, nil, nil
		},
		containerCreateFn: func(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			configs = append(configs, *config)
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			starts++
			if starts == 1 {
				return errors.New("exec /gone-from-this-image: no such file or directory")
			}
			return nil
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.NoError(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))

	require.Len(t, configs, 2, "the failed start is retried once")
	assert.Equal(t, []string{"/gone-from-this-image"}, []string(configs[0].Entrypoint),
		"the first try carries what the container recorded")
	assert.Nil(t, configs[1].Entrypoint, "the retry takes the image's own entrypoint")
}

// A retry that also fails reports the original cause. Inventing a second reason
// for one failure sends the reader after the wrong thing.
func TestRollingUpdateReportsTheFirstCauseWhenTheRetryFails(t *testing.T) {
	t.Serial()
	cli := &mockDocker{
		containerInspectFn: wedgedInspect([]string{"/gone-from-this-image"}),
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{Config: &container.Config{Entrypoint: []string{"/myapp"}}}, nil, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			return errors.New("exec /gone-from-this-image: no such file or directory")
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such file or directory")
	assert.Contains(t, err.Error(), "retry on the image's own defaults also failed")
}

// A container carrying nothing of its own has nothing to strip, so a failure is
// the image's and the updater must not spend a second start on it.
func TestRollingUpdateDoesNotRetryWhenThereIsNothingToStrip(t *testing.T) {
	t.Serial()
	starts := 0
	cli := &mockDocker{
		containerInspectFn: wedgedInspect(nil),
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{Config: &container.Config{}}, nil, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			starts++
			return errors.New("the image itself is broken")
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.Error(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))
	assert.Equal(t, 1, starts, "no retry when the config carries nothing of its own")
}
