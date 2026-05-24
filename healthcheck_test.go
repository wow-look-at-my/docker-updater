package main

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestRecreateContainerHealthTimeout(t *testing.T) {
	var stoppedID string
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "nginx:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "starting"},
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedID = id
			return nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := recreateContainer(ctx, cli, info, "nginx:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
	assert.Equal(t, "new123456789", stoppedID)
}

func TestRecreateContainerNoHealthcheck(t *testing.T) {
	var stoppedID string
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "nginx:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  nil,
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedID = id
			return nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "no healthcheck defined")
	assert.Equal(t, "new123456789", stoppedID)
}

func TestRollingUpdateContainerNoHealthcheck(t *testing.T) {
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "myapp:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  nil,
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:      "old123456789",
		Name:    "myapp",
		Image:   "myapp:latest",
		Rolling: true,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "no healthcheck defined")
}

func TestRollingUpdateContainerHealthTimeout(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			if id == "old123456789" {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "myapp:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "starting"},
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:      "old123456789",
		Name:    "myapp",
		Image:   "myapp:latest",
		Rolling: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rollingUpdateContainer(ctx, cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
}
