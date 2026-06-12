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
	// No HEALTHCHECK in the image and no health-check labels: the gate
	// degrades to "stays running for the grace period". The update must
	// succeed -- failing it would make the container permanently
	// un-updatable (and, before the rollback fix, take it offline).
	createCount := 0
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
			createCount++
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.Nil(t, err)
	assert.Equal(t, 1, createCount, "update succeeded; no rollback expected")
}

func TestRecreateContainerNoHealthcheckExitRollsBack(t *testing.T) {
	// No healthcheck and the new container dies during the grace period: the
	// update must fail and roll back to the previous image.
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			inspectCount++
			switch inspectCount {
			case 1:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "nginx:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			case 2:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						State: &types.ContainerState{Running: true, Health: nil},
					},
				}, nil
			default:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						State: &types.ContainerState{Running: false},
					},
				}, nil
			}
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "container exited")
	assert.Contains(t, err.Error(), "rolled back to previous image")
}

func TestRecreateContainerNoHealthcheckRestartRollsBack(t *testing.T) {
	// A restart policy can mask a crash loop: the container is "running" at
	// poll time but its restart count betrays the crash. Must roll back.
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			inspectCount++
			switch inspectCount {
			case 1:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "nginx:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			case 2:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						State: &types.ContainerState{Running: true, Health: nil},
					},
				}, nil
			default:
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						RestartCount: 2,
						State:        &types.ContainerState{Running: true, Health: nil},
					},
				}, nil
			}
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "container restarted")
	assert.Contains(t, err.Error(), "rolled back to previous image")
}

func TestRollingUpdateContainerNoHealthcheck(t *testing.T) {
	// Rolling mode with no healthcheck anywhere: the next container is gated
	// on staying running, then the old container is drained and the next one
	// renamed into place.
	var renamedTo string
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
		containerRenameFn: func(_ context.Context, _ string, newName string) error {
			renamedTo = newName
			return nil
		},
	}

	info := ContainerInfo{
		ID:      "old123456789",
		Name:    "myapp",
		Image:   "myapp:latest",
		Rolling: true,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
	assert.Equal(t, "myapp", renamedTo)
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

func TestRecreateContainerRollsBackOnUnhealthy(t *testing.T) {
	var (
		stops, removes, starts []string
		creates                []string // "image name" per create call
		tagSource, tagTarget   string
	)
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
					Config:          &container.Config{Image: "ghcr.io/org/app:latest"},
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
		containerCreateFn: func(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			creates = append(creates, config.Image+" "+name)
			if len(creates) == 1 {
				return container.CreateResponse{ID: "new123456789"}, nil
			}
			return container.CreateResponse{ID: "restored90123"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stops = append(stops, id)
			return nil
		},
		containerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removes = append(removes, id)
			return nil
		},
		containerStartFn: func(_ context.Context, id string, _ container.StartOptions) error {
			starts = append(starts, id)
			return nil
		},
		imageTagFn: func(_ context.Context, source, target string) error {
			tagSource, tagTarget = source, target
			return nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "ghcr.io/org/app:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "ghcr.io/org/app:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy after update")
	assert.Contains(t, err.Error(), "rolled back to previous image")

	// The failed replacement is stopped and removed...
	assert.Equal(t, []string{"old123456789", "new123456789"}, stops)
	assert.Equal(t, []string{"old123456789", "new123456789"}, removes)

	// ...the previous image is re-tagged to the original reference so the
	// restored container keeps resolving (and retrying) registry updates...
	assert.Equal(t, "sha256:olddigest", tagSource)
	assert.Equal(t, "ghcr.io/org/app:latest", tagTarget)

	// ...and a container on the previous image comes back up under the
	// original name.
	require.Equal(t, 2, len(creates))
	assert.Equal(t, "ghcr.io/org/app:latest test-app", creates[1])
	assert.Equal(t, []string{"new123456789", "restored90123"}, starts)
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
