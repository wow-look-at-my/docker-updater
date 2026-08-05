package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Speed up health-check polling and the no-healthcheck grace period so the
	// suite finishes well within the global 30s test timeout. Production code
	// still uses the real defaults.
	healthCheckPollInterval = 10 * time.Millisecond
	execPollInterval = 10 * time.Millisecond
	noHealthcheckGracePeriod = 50 * time.Millisecond
	aliasSettleDelay = 10 * time.Millisecond
	os.Exit(m.Run())
}

// baseInspect returns a ContainerJSON for the initial inspect of the old container.
func baseInspect() types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			Image:      "sha256:olddigest",
			HostConfig: &container.HostConfig{},
		},
		Config:          &container.Config{Image: "myapp:latest"},
		NetworkSettings: &types.NetworkSettings{},
	}
}

// --- recreateContainer: HTTP health check ---

func TestRecreateContainerHTTPHealthSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		HealthCheckURL:     srv.URL + "/health",
		HealthCheckTimeout: 10 * time.Second,
	}

	err := recreateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
}

func TestRecreateContainerHTTPHealthTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var stoppedID string
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
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
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		HealthCheckURL:     srv.URL + "/health",
		HealthCheckTimeout: 50 * time.Millisecond,
	}

	err := recreateContainer(context.Background(), cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
	assert.Equal(t, "new123456789", stoppedID)
}

// --- recreateContainer: exec health check ---

func TestRecreateContainerExecHealthSuccess(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 0}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		HealthCheckCommand: "curl -sf http://localhost:8080/health",
		HealthCheckTimeout: 10 * time.Second,
	}

	err := recreateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
}

func TestRecreateContainerExecHealthTimeout(t *testing.T) {
	var stoppedID string
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedID = id
			return nil
		},
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 1}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		HealthCheckCommand: "curl -sf http://localhost:8080/health",
		HealthCheckTimeout: 50 * time.Millisecond,
	}

	err := recreateContainer(context.Background(), cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
	assert.Equal(t, "new123456789", stoppedID)
}

// --- rollingUpdateContainer: HTTP health check ---

func TestRollingUpdateContainerHTTPHealthSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		Rolling:            true,
		HealthCheckURL:     srv.URL + "/health",
		HealthCheckTimeout: 10 * time.Second,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
}

func TestRollingUpdateContainerHTTPHealthTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		Rolling:            true,
		HealthCheckURL:     srv.URL + "/health",
		HealthCheckTimeout: 50 * time.Millisecond,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
}

// --- rollingUpdateContainer: exec health check ---

func TestRollingUpdateContainerExecHealthSuccess(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 0}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		Rolling:            true,
		HealthCheckCommand: "curl -sf http://localhost:8080/health",
		HealthCheckTimeout: 10 * time.Second,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
}

func TestRollingUpdateContainerExecHealthTimeout(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return baseInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 1}, nil
		},
	}

	info := ContainerInfo{
		ID:                 "old123456789",
		Name:               "myapp",
		Image:              "myapp:latest",
		Rolling:            true,
		HealthCheckCommand: "curl -sf http://localhost:8080/health",
		HealthCheckTimeout: 50 * time.Millisecond,
	}

	err := rollingUpdateContainer(context.Background(), cli, info, "myapp:latest")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "not healthy")
}

// --- post-update fallback to Docker HEALTHCHECK status ---

// When no health-check label is set, waitPostUpdateHealthy falls back to
// Docker's HEALTHCHECK status via waitHealthy (which derives its own budget).
func TestRecreateContainerFallbackDockerHealthy(t *testing.T) {
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return baseInspect(), nil
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
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{
		ID:    "old123456789",
		Name:  "myapp",
		Image: "myapp:latest",
		// no HealthCheckURL / HealthCheckCommand -> Docker status fallback
	}

	err := recreateContainer(context.Background(), cli, info, "myapp:latest")
	require.Nil(t, err)
}

// --- listMonitoredContainers: health-check label parsing ---

func TestListMonitoredContainersHealthCheck(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "http-hc",
					Names: []string{"/http-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":               "true",
						"docker-updater.health-check.url":     "http://localhost:8080/health",
						"docker-updater.health-check.timeout": "30s",
					},
				},
				{
					ID:    "exec-hc",
					Names: []string{"/exec-app"},
					Image: "otherapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":               "true",
						"docker-updater.health-check.command": "/check.sh",
						"docker-updater.health-check.timeout": "45s",
					},
				},
				{
					ID:    "no-hc",
					Names: []string{"/plain-app"},
					Image: "plainapp:latest",
					Labels: map[string]string{
						"docker-updater.enable": "true",
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
	require.Equal(t, 3, len(containers))

	// HTTP health check
	assert.Equal(t, "http://localhost:8080/health", containers[0].HealthCheckURL)
	assert.Empty(t, containers[0].HealthCheckCommand)
	assert.Equal(t, 30*time.Second, containers[0].HealthCheckTimeout)

	// Exec health check
	assert.Empty(t, containers[1].HealthCheckURL)
	assert.Equal(t, "/check.sh", containers[1].HealthCheckCommand)
	assert.Equal(t, 45*time.Second, containers[1].HealthCheckTimeout)

	// No health check — timeout should be zero (not set)
	assert.Empty(t, containers[2].HealthCheckURL)
	assert.Empty(t, containers[2].HealthCheckCommand)
	assert.Equal(t, time.Duration(0), containers[2].HealthCheckTimeout)
}

func TestListMonitoredContainersHealthCheckURLResolve(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "resolve-hc",
					Names: []string{"/resolve-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":           "true",
						"docker-updater.health-check.url": ":8080/health",
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
						"bridge": {IPAddress: "172.17.0.8"},
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
	assert.Equal(t, "http://172.17.0.8:8080/health", containers[0].HealthCheckURL)
}

// --- post-update health gate: the address must follow the new container ---

// serveOn starts an HTTP server bound to addr on an arbitrary port and returns
// its port. Two loopback addresses stand in for Docker handing the replacement
// container a different IP than the one the update destroyed.
func serveOn(t *testing.T, addr string, status int) int {
	t.Helper()
	ln, err := net.Listen("tcp", addr+":0")
	require.NoError(t, err)
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().(*net.TCPAddr).Port
}

func inspectWithIP(ip string) types.ContainerJSON {
	settings := &types.NetworkSettings{}
	if ip != "" {
		settings.Networks = map[string]*network.EndpointSettings{"bridge": {IPAddress: ip}}
	}
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "myapp:latest"},
		NetworkSettings:   settings,
	}
}

func TestWaitPostUpdateHealthyFollowsNewContainerAddress(t *testing.T) {
	// The new container serves on 127.0.0.2; the URL built before the update
	// still names 127.0.0.1, where nothing answers.
	port := serveOn(t, "127.0.0.2", http.StatusOK)

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			require.Equal(t, "new123456789", id)
			return inspectWithIP("127.0.0.2"), nil
		},
	}

	info := ContainerInfo{
		Name:                        "myapp",
		HealthCheckURL:              fmt.Sprintf("http://127.0.0.1:%d%s", port, wellKnownHealth),
		HealthCheckURLFromContainer: true,
		HealthCheckTimeout:          2 * time.Second,
	}

	start := time.Now()
	require.NoError(t, waitPostUpdateHealthy(context.Background(), cli, "new123456789", info))
	assert.Less(t, time.Since(start), time.Second, "should pass on the new address, not burn the budget on the old one")
}

func TestWaitPostUpdateHealthyKeepsOperatorURL(t *testing.T) {
	// An operator-written absolute URL names something else on purpose. The
	// new container's address must not displace it.
	port := serveOn(t, "127.0.0.2", http.StatusOK)

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return inspectWithIP("127.0.0.1"), nil
		},
	}

	info := ContainerInfo{
		Name:               "myapp",
		HealthCheckURL:     fmt.Sprintf("http://127.0.0.2:%d/health", port),
		HealthCheckTimeout: 2 * time.Second,
	}

	require.NoError(t, waitPostUpdateHealthy(context.Background(), cli, "new123456789", info))
}

func TestWaitPostUpdateHealthyNewContainerHasNoAddress(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return inspectWithIP(""), nil
		},
	}

	info := ContainerInfo{
		Name:                        "myapp",
		HealthCheckURL:              "http://127.0.0.1:9999" + wellKnownHealth,
		HealthCheckURLFromContainer: true,
		HealthCheckTimeout:          2 * time.Second,
	}

	start := time.Now()
	err := waitPostUpdateHealthy(context.Background(), cli, "new123456789", info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no reachable address")
	assert.Less(t, time.Since(start), time.Second, "must fail immediately, not fall back to the old address")
}

func TestWaitPostUpdateHealthyInspectFailsAfterUpdate(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{}, errors.New("no such container")
		},
	}

	info := ContainerInfo{
		Name:                        "myapp",
		HealthCheckURL:              "http://127.0.0.1:9999" + wellKnownHealth,
		HealthCheckURLFromContainer: true,
		HealthCheckTimeout:          2 * time.Second,
	}

	err := waitPostUpdateHealthy(context.Background(), cli, "new123456789", info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such container")
}

func TestWaitPostUpdateHealthyNoContainerToInspect(t *testing.T) {
	// The compose path resolves the replacement container by service name and
	// can come up empty; there is then nothing to re-resolve against.
	info := ContainerInfo{
		Name:                        "myapp",
		HealthCheckURL:              "http://127.0.0.1:9999" + wellKnownHealth,
		HealthCheckURLFromContainer: true,
		HealthCheckTimeout:          2 * time.Second,
	}

	err := waitPostUpdateHealthy(context.Background(), &mockDocker{}, "", info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container")
}
