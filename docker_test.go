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
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

// mockDocker implements DockerClient for testing.
type mockDocker struct {
	infoFn             func(ctx context.Context) (system.Info, error)
	containerListFn    func(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	containerInspectFn func(ctx context.Context, id string) (types.ContainerJSON, error)
	containerStopFn    func(ctx context.Context, id string, options container.StopOptions) error
	containerRemoveFn  func(ctx context.Context, id string, options container.RemoveOptions) error
	containerCreateFn  func(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	containerStartFn   func(ctx context.Context, id string, options container.StartOptions) error
	imagePullFn        func(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	imageInspectFn     func(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
}

func (m *mockDocker) Info(ctx context.Context) (system.Info, error) {
	if m.infoFn != nil {
		return m.infoFn(ctx)
	}
	return system.Info{ServerVersion: "test", Name: "test-host"}, nil
}

func (m *mockDocker) ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error) {
	if m.containerListFn != nil {
		return m.containerListFn(ctx, options)
	}
	return nil, nil
}

func (m *mockDocker) ContainerInspect(ctx context.Context, id string) (types.ContainerJSON, error) {
	if m.containerInspectFn != nil {
		return m.containerInspectFn(ctx, id)
	}
	return types.ContainerJSON{}, nil
}

func (m *mockDocker) ContainerStop(ctx context.Context, id string, options container.StopOptions) error {
	if m.containerStopFn != nil {
		return m.containerStopFn(ctx, id, options)
	}
	return nil
}

func (m *mockDocker) ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error {
	if m.containerRemoveFn != nil {
		return m.containerRemoveFn(ctx, id, options)
	}
	return nil
}

func (m *mockDocker) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	if m.containerCreateFn != nil {
		return m.containerCreateFn(ctx, config, hostConfig, networkingConfig, platform, name)
	}
	return container.CreateResponse{ID: "new-container-id"}, nil
}

func (m *mockDocker) ContainerStart(ctx context.Context, id string, options container.StartOptions) error {
	if m.containerStartFn != nil {
		return m.containerStartFn(ctx, id, options)
	}
	return nil
}

func (m *mockDocker) ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error) {
	if m.imagePullFn != nil {
		return m.imagePullFn(ctx, refStr, options)
	}
	return io.NopCloser(strings.NewReader(`{"status":"Pull complete"}`)), nil
}

func (m *mockDocker) ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error) {
	if m.imageInspectFn != nil {
		return m.imageInspectFn(ctx, imageID)
	}
	return types.ImageInspect{ID: "sha256:testdigest"}, nil, nil
}

func (m *mockDocker) Close() error { return nil }

func TestDockerClientInfo(t *testing.T) {
	cli := &mockDocker{
		infoFn: func(_ context.Context) (system.Info, error) {
			return system.Info{ServerVersion: "27.5.1", Name: "test-host"}, nil
		},
	}

	info, err := cli.Info(context.Background())
	require.Nil(t, err)
	assert.Equal(t, "27.5.1", info.ServerVersion)
	assert.Equal(t, "test-host", info.Name)
}

func TestListMonitoredContainers(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "abc123def456",
					Names: []string{"/test-container"},
					Image: "nginx:latest",
					Labels: map[string]string{
						"docker-updater.enable": "true",
					},
				},
				{
					ID:     "def456",
					Names:  []string{"/unmonitored"},
					Image:  "redis:latest",
					Labels: map[string]string{},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:imagedigest123"},
			}, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, "test-container", c.Name)
	assert.Equal(t, "nginx:latest", c.Image)
	assert.Equal(t, UpdateModeImage, c.Mode)
	assert.Equal(t, "sha256:imagedigest123", c.ImageDigest)
}

func TestListMonitoredContainersGitMode(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "git123",
					Names: []string{"/git-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":   "true",
						"docker-updater.mode":     "git",
						"docker-updater.git-repo": "https://github.com/example/repo",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
			}, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, UpdateModeGit, c.Mode)
	assert.Equal(t, "https://github.com/example/repo", c.GitRepo)
	assert.Equal(t, "refs/heads/main", c.GitRef)
}

func TestListMonitoredContainersEmpty(t *testing.T) {
	cli := &mockDocker{}
	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	assert.Equal(t, 0, len(containers))
}

func TestListMonitoredContainersError(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return nil, errors.New("connection refused")
		},
	}

	_, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.NotNil(t, err)
}

func TestPullImage(t *testing.T) {
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest123"}, nil, nil
		},
	}

	digest, err := pullImage(context.Background(), cli, "nginx:latest")
	require.Nil(t, err)
	assert.Equal(t, "sha256:newdigest123", digest)
}

func TestPullImageError(t *testing.T) {
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("pull failed")
		},
	}

	_, err := pullImage(context.Background(), cli, "broken:latest")
	require.NotNil(t, err)
}

func TestRecreateContainer(t *testing.T) {
	var stoppedID, removedID, createdImage, startedID string

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					Image:      "sha256:olddigest",
					HostConfig: &container.HostConfig{},
				},
				Config: &container.Config{
					Image: "nginx:latest",
					Env:   []string{"FOO=bar"},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {Aliases: []string{"web"}},
					},
				},
			}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedID = id
			return nil
		},
		containerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removedID = id
			return nil
		},
		containerCreateFn: func(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			createdImage = config.Image
			return container.CreateResponse{ID: "new456789012"}, nil
		},
		containerStartFn: func(_ context.Context, id string, _ container.StartOptions) error {
			startedID = id
			return nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.Nil(t, err)
	assert.Equal(t, "old123456789", stoppedID)
	assert.Equal(t, "old123456789", removedID)
	assert.Equal(t, "nginx:latest", createdImage)
	assert.Equal(t, "new456789012", startedID)
}

func TestRecreateContainerStopError(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
				Config:            &container.Config{},
				NetworkSettings:   &types.NetworkSettings{},
			}, nil
		},
		containerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			return errors.New("stop failed")
		},
	}

	err := recreateContainer(context.Background(), cli, ContainerInfo{ID: "old123456789", Name: "test"}, "img")
	require.NotNil(t, err)
}

func TestRecreateContainerRemoveError(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
				Config:            &container.Config{},
				NetworkSettings:   &types.NetworkSettings{},
			}, nil
		},
		containerRemoveFn: func(_ context.Context, _ string, _ container.RemoveOptions) error {
			return errors.New("remove failed")
		},
	}

	err := recreateContainer(context.Background(), cli, ContainerInfo{ID: "old123456789", Name: "test"}, "img")
	require.NotNil(t, err)
}

func TestRecreateContainerCreateError(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
				Config:            &container.Config{},
				NetworkSettings:   &types.NetworkSettings{},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{}, errors.New("create failed")
		},
	}

	err := recreateContainer(context.Background(), cli, ContainerInfo{ID: "old123456789", Name: "test"}, "img")
	require.NotNil(t, err)
}

func TestRecreateContainerStartError(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
				Config:            &container.Config{},
				NetworkSettings:   &types.NetworkSettings{},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			return errors.New("start failed")
		},
	}

	err := recreateContainer(context.Background(), cli, ContainerInfo{ID: "old123456789", Name: "test"}, "img")
	require.NotNil(t, err)
}

func TestRecreateContainerInspectError(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{}, errors.New("inspect failed")
		},
	}

	err := recreateContainer(context.Background(), cli, ContainerInfo{ID: "old123456789", Name: "test"}, "img")
	require.NotNil(t, err)
}
