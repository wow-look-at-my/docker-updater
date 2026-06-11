package main

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestHealthCheckTimeout(t *testing.T) {
	t.Run("nil config returns fallback", func(t *testing.T) {
		assert.Equal(t, 5*time.Minute, healthCheckTimeout(types.ContainerJSON{}))
	})

	t.Run("nil healthcheck returns fallback", func(t *testing.T) {
		assert.Equal(t, 5*time.Minute, healthCheckTimeout(types.ContainerJSON{
			Config: &container.Config{},
		}))
	})

	t.Run("custom config", func(t *testing.T) {
		inspect := types.ContainerJSON{
			Config: &container.Config{
				Healthcheck: &container.HealthConfig{
					StartPeriod: 2 * time.Minute,
					Interval:    10 * time.Second,
					Timeout:     5 * time.Second,
					Retries:     3,
				},
			},
		}
		// 2m + (3 * 10s) + 5s = 2m35s
		assert.Equal(t, 2*time.Minute+35*time.Second, healthCheckTimeout(inspect))
	})

	t.Run("zero fields use defaults", func(t *testing.T) {
		inspect := types.ContainerJSON{
			Config: &container.Config{
				Healthcheck: &container.HealthConfig{
					// All zero — should use defaults: 30s interval, 3 retries, 30s timeout
				},
			},
		}
		// 0 + (3 * 30s) + 30s = 2m
		assert.Equal(t, 2*time.Minute, healthCheckTimeout(inspect))
	})
}

func TestRecreateContainerUnhealthy(t *testing.T) {
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
						Health:  &types.Health{Status: "unhealthy"},
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
	assert.Contains(t, err.Error(), "not healthy")
	assert.Equal(t, "new123456789", stoppedID)
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
