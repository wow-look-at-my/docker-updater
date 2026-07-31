package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDocker implements DockerClient for testing.
type mockDocker struct {
	infoFn                 func(ctx context.Context) (system.Info, error)
	containerListFn        func(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	containerInspectFn     func(ctx context.Context, id string) (types.ContainerJSON, error)
	containerStopFn        func(ctx context.Context, id string, options container.StopOptions) error
	containerRemoveFn      func(ctx context.Context, id string, options container.RemoveOptions) error
	containerCreateFn      func(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	containerStartFn       func(ctx context.Context, id string, options container.StartOptions) error
	containerRenameFn      func(ctx context.Context, containerID string, newName string) error
	containerExecCreateFn  func(ctx context.Context, containerID string, options container.ExecOptions) (types.IDResponse, error)
	containerExecStartFn   func(ctx context.Context, execID string, config container.ExecStartOptions) error
	containerExecInspectFn func(ctx context.Context, execID string) (container.ExecInspect, error)
	imagePullFn            func(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	imageInspectFn         func(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	imageTagFn             func(ctx context.Context, source, target string) error
	networkConnectFn       func(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	networkDisconnectFn    func(ctx context.Context, networkID, containerID string, force bool) error
}

func (m *mockDocker) NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error {
	if m.networkConnectFn != nil {
		return m.networkConnectFn(ctx, networkID, containerID, config)
	}
	return nil
}

func (m *mockDocker) NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error {
	if m.networkDisconnectFn != nil {
		return m.networkDisconnectFn(ctx, networkID, containerID, force)
	}
	return nil
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

func (m *mockDocker) ContainerRename(ctx context.Context, containerID string, newName string) error {
	if m.containerRenameFn != nil {
		return m.containerRenameFn(ctx, containerID, newName)
	}
	return nil
}

func (m *mockDocker) ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (types.IDResponse, error) {
	if m.containerExecCreateFn != nil {
		return m.containerExecCreateFn(ctx, containerID, options)
	}
	return types.IDResponse{ID: "exec-id"}, nil
}

func (m *mockDocker) ContainerExecStart(ctx context.Context, execID string, config container.ExecStartOptions) error {
	if m.containerExecStartFn != nil {
		return m.containerExecStartFn(ctx, execID, config)
	}
	return nil
}

func (m *mockDocker) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	if m.containerExecInspectFn != nil {
		return m.containerExecInspectFn(ctx, execID)
	}
	return container.ExecInspect{Running: false, ExitCode: 0}, nil
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

func (m *mockDocker) ImageTag(ctx context.Context, source, target string) error {
	if m.imageTagFn != nil {
		return m.imageTagFn(ctx, source, target)
	}
	return nil
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
				Config:            &container.Config{Image: "nginx:latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			// A real pulled registry image carries RepoDigests.
			return types.ImageInspect{ID: "sha256:imagedigest123", RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, "test-container", c.Name)
	assert.Equal(t, "nginx:latest", c.Image)
	assert.Equal(t, UpdateModeImage, c.Mode)
	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), c.ImageDigest)
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

func TestRegistryFromImage(t *testing.T) {
	tests := []struct {
		image    string
		expected string
	}{
		{"nginx:latest", "https://index.docker.io/v1/"},
		{"library/nginx:latest", "https://index.docker.io/v1/"},
		{"docker.io/library/nginx:latest", "https://index.docker.io/v1/"},
		{"ghcr.io/org/image:latest", "ghcr.io"},
		{"ghcr.io/org/image:v1.2.3", "ghcr.io"},
		{"ghcr.io/org/image@sha256:abc123", "ghcr.io"},
		{"myregistry.com:5000/myimage:v1", "myregistry.com:5000"},
		{"registry.example.com/app:latest", "registry.example.com"},
		{"ubuntu", "https://index.docker.io/v1/"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			assert.Equal(t, tt.expected, registryFromImage(tt.image))
		})
	}
}

func TestLoadDockerConfig(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "config-*.json")
	require.Nil(t, err)
	_, err = f.WriteString(`{"auths":{"ghcr.io":{"auth":"dXNlcjp0b2tlbg=="},"https://index.docker.io/v1/":{"auth":"ZG9ja2VyOnBhc3M="}}}`)
	require.Nil(t, err)
	f.Close()

	cfg, err := loadDockerConfig(f.Name())
	require.Nil(t, err)
	assert.Equal(t, 2, len(cfg.Auths))
	assert.Equal(t, "dXNlcjp0b2tlbg==", cfg.Auths["ghcr.io"].Auth)
	assert.Equal(t, "ZG9ja2VyOnBhc3M=", cfg.Auths["https://index.docker.io/v1/"].Auth)
}

func TestLoadDockerConfigMissing(t *testing.T) {
	_, err := loadDockerConfig("/nonexistent/config.json")
	require.NotNil(t, err)
}

func TestEncodeRegistryAuth(t *testing.T) {
	entry := dockerAuthEntry{Auth: "dXNlcjp0b2tlbg=="}
	encoded, err := encodeRegistryAuth(entry, "ghcr.io")
	require.Nil(t, err)
	assert.NotEmpty(t, encoded)

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	require.Nil(t, err)
	var result map[string]string
	require.Nil(t, json.Unmarshal(decoded, &result))
	assert.Equal(t, "user", result["username"])
	assert.Equal(t, "token", result["password"])
	assert.Equal(t, "ghcr.io", result["serveraddress"])
}

func TestRecreateContainer(t *testing.T) {
	var stoppedID, removedID, createdImage, startedID string

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
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "healthy"},
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

func TestListMonitoredContainersPreCheck(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "http-check",
					Names: []string{"/http-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":            "true",
						"docker-updater.pre-check.url":     "http://localhost:8080/ready",
						"docker-updater.pre-check.timeout": "10s",
					},
				},
				{
					ID:    "exec-check",
					Names: []string{"/exec-app"},
					Image: "otherapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":            "true",
						"docker-updater.pre-check.command": "/check-ready.sh",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
				Config:            &container.Config{Image: "myapp:latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:digest", RepoDigests: []string{"myapp@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 2, len(containers))

	assert.Equal(t, "http://localhost:8080/ready", containers[0].PreCheckURL)
	assert.Equal(t, 10*time.Second, containers[0].PreCheckTimeout)

	assert.Equal(t, "/check-ready.sh", containers[1].PreCheckCommand)
	assert.Equal(t, 30*time.Second, containers[1].PreCheckTimeout)
}

func TestListMonitoredContainersPreCheckURLResolve(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "resolve-check",
					Names: []string{"/resolve-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":        "true",
						"docker-updater.pre-check.url": ":8080/ready-to-update",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
				Config:            &container.Config{Image: "myapp:latest"},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {IPAddress: "172.17.0.5"},
					},
				},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:digest", RepoDigests: []string{"myapp@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))
	assert.Equal(t, "http://172.17.0.5:8080/ready-to-update", containers[0].PreCheckURL)
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

func TestListMonitoredContainersRolling(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "rolling-1",
					Names: []string{"/rolling-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":  "true",
						"docker-updater.rolling": "true",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
				Config:            &container.Config{Image: "myapp:latest"},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:digest", RepoDigests: []string{"myapp@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	require.Equal(t, 1, len(containers))
	assert.True(t, containers[0].Rolling)
}

func TestRollingUpdateContainer(t *testing.T) {
	var stoppedID, removedID, renamedID, renamedTo string

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
					Config: &container.Config{Image: "myapp:latest"},
					NetworkSettings: &types.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"internal": {Aliases: []string{"backend"}},
						},
					},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "healthy"},
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			assert.Equal(t, "myapp-next", name)
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedID = id
			return nil
		},
		containerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removedID = id
			return nil
		},
		containerRenameFn: func(_ context.Context, id string, newName string) error {
			renamedID = id
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
	assert.Equal(t, "old123456789", stoppedID)
	assert.Equal(t, "old123456789", removedID)
	assert.Equal(t, "new123456789", renamedID)
	assert.Equal(t, "myapp", renamedTo)
}

// A service alias is the load balancer's whole notion of "the service", and
// Docker's DNS round-robins across every container holding one. Giving the new
// container the alias at CREATE time therefore load-balances live traffic
// across two IMAGE VERSIONS for the entire health wait -- tens of seconds with
// a 30s-interval HEALTHCHECK -- and clients get answers shaped by the build
// being replaced. The alias must move only once the replacement is healthy,
// and the old container must keep serving until after it has.
func TestRollingUpdateContainer_AliasMovesOnlyAfterHealthy(t *testing.T) {
	var events []string
	var createdAliases []string
	var connectedAliases []string

	inspectCount := 0
	healthy := false
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config: &container.Config{Image: "myapp:latest"},
					NetworkSettings: &types.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"internal": {Aliases: []string{"backend", "myapp"}},
						},
					},
				}, nil
			}
			// Report unhealthy once, so "healthy" is a state the test passes
			// through rather than one it starts in.
			status := "starting"
			if healthy {
				status = "healthy"
			}
			healthy = true
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{Running: true, Health: &types.Health{Status: status}},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, nc *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			events = append(events, "create")
			for _, ep := range nc.EndpointsConfig {
				createdAliases = append(createdAliases, ep.Aliases...)
			}
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			events = append(events, "start")
			return nil
		},
		networkDisconnectFn: func(_ context.Context, netName, id string, force bool) error {
			events = append(events, "disconnect")
			assert.Equal(t, "internal", netName)
			assert.Equal(t, "new123456789", id, "only the incoming container is re-wired; disconnecting the old one would sever its in-flight requests")
			assert.False(t, force)
			return nil
		},
		networkConnectFn: func(_ context.Context, netName, id string, cfg *network.EndpointSettings) error {
			events = append(events, "connect")
			assert.Equal(t, "internal", netName)
			assert.Equal(t, "new123456789", id)
			connectedAliases = append(connectedAliases, cfg.Aliases...)
			return nil
		},
		containerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			events = append(events, "stop-old")
			return nil
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.Nil(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))

	assert.Empty(t, createdAliases,
		"the new container must not answer to the service alias before it is healthy")
	assert.Equal(t, []string{"backend", "myapp"}, connectedAliases,
		"every alias the old container served under must move to the new one")
	assert.Equal(t, []string{"create", "start", "disconnect", "connect", "stop-old"}, events,
		"the alias must move while the old container is still serving, so the cutover never leaves the alias pointing at nothing")
}
