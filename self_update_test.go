package main

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerIDFromMountinfo(t *testing.T) {
	// A representative slice of a real container's /proc/self/mountinfo: the
	// container ID appears in the host paths Docker bind-mounts over /etc/*.
	id := "0872e0c937f6" + strings.Repeat("a", 52) // a full 64-hex container ID
	mountinfo := strings.Join([]string{
		"1234 1200 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/AAA",
		"1240 1234 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw",
		"1241 1234 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw",
	}, "\n")

	assert.Equal(t, id, containerIDFromMountinfo(mountinfo))
}

func TestContainerIDFromMountinfoNone(t *testing.T) {
	// A host process (not in a container) has no /containers/<id>/ paths.
	mountinfo := "1234 1200 0:50 / / rw,relatime - ext4 /dev/sda1 rw\n"
	assert.Empty(t, containerIDFromMountinfo(mountinfo))
}

func TestContainerIDFromCgroup(t *testing.T) {
	id := "0872e0c937f6" + strings.Repeat("a", 52) // a full 64-hex container ID
	t.Run("cgroup v1 docker path", func(t *testing.T) {
		cgroup := "12:devices:/docker/" + id + "\n11:cpu,cpuacct:/docker/" + id + "\n"
		assert.Equal(t, id, containerIDFromCgroup(cgroup))
	})
	t.Run("systemd scope", func(t *testing.T) {
		cgroup := "0::/system.slice/docker-" + id + ".scope\n"
		assert.Equal(t, id, containerIDFromCgroup(cgroup))
	})
	t.Run("no id", func(t *testing.T) {
		assert.Empty(t, containerIDFromCgroup("0::/\n"))
	})
}

func TestSameContainer(t *testing.T) {
	full := "0872e0c937f6abf0d2f3c4a1b6e7d8c9aa11223344556677889900aabbccddeeff"
	assert.True(t, sameContainer(full, full))
	assert.True(t, sameContainer(full, "0872e0c937f6"), "short-ID prefix matches")
	assert.True(t, sameContainer("0872e0c937f6", full), "prefix in either direction")
	assert.False(t, sameContainer(full, "deadbeef0000"))
	assert.False(t, sameContainer("", full), "empty never matches")
	assert.False(t, sameContainer(full, ""))
}

func TestDockerSocketBinds(t *testing.T) {
	t.Run("from binds", func(t *testing.T) {
		inspect := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				HostConfig: &container.HostConfig{
					Binds: []string{
						"/etc/localtime:/etc/localtime:ro",
						"/var/run/docker.sock:/var/run/docker.sock",
					},
				},
			},
		}
		assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock"}, dockerSocketBinds(inspect))
	})

	t.Run("falls back to mounts", func(t *testing.T) {
		inspect := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				HostConfig: &container.HostConfig{},
			},
			Mounts: []types.MountPoint{
				{Source: "/run/docker.sock", Destination: "/var/run/docker.sock", RW: false},
			},
		}
		assert.Equal(t, []string{"/run/docker.sock:/var/run/docker.sock:ro"}, dockerSocketBinds(inspect))
	})

	t.Run("none", func(t *testing.T) {
		inspect := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		}
		assert.Empty(t, dockerSocketBinds(inspect))
	})
}

func TestDockerHostEnv(t *testing.T) {
	inspect := types.ContainerJSON{
		Config: &container.Config{Env: []string{"FOO=bar", "DOCKER_HOST=tcp://127.0.0.1:2375"}},
	}
	assert.Equal(t, "tcp://127.0.0.1:2375", dockerHostEnv(inspect))

	none := types.ContainerJSON{Config: &container.Config{Env: []string{"FOO=bar"}}}
	assert.Empty(t, dockerHostEnv(none))
}

// selfInspect returns a ContainerJSON describing docker-updater's own container
// with the Docker socket mounted -- the shape selfUpdate inspects.
func selfInspect() types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:   "selfid",
			Name: "/docker-updater",
			HostConfig: &container.HostConfig{
				Binds:       []string{"/var/run/docker.sock:/var/run/docker.sock"},
				NetworkMode: "host",
			},
		},
		Config: &container.Config{Image: "ghcr.io/wow-look-at-my/docker-updater:latest"},
	}
}

func TestSelfUpdateSpawnsHelper(t *testing.T) {
	var createdName string
	var createdCfg *container.Config
	var createdHost *container.HostConfig
	started := false
	var stoppedIDs, removedIDs []string

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return selfInspect(), nil
		},
		containerCreateFn: func(_ context.Context, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			createdName, createdCfg, createdHost = name, cfg, host
			return container.CreateResponse{ID: "helperid"}, nil
		},
		containerStartFn: func(_ context.Context, id string, _ container.StartOptions) error {
			started = true
			assert.Equal(t, "helperid", id)
			return nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stoppedIDs = append(stoppedIDs, id)
			return nil
		},
		containerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removedIDs = append(removedIDs, id)
			return nil
		},
	}

	const newImage = "ghcr.io/wow-look-at-my/docker-updater:latest"
	info := ContainerInfo{ID: "selfid", Name: "docker-updater", Image: newImage}
	require.NoError(t, selfUpdate(context.Background(), cli, info, newImage))

	assert.True(t, started, "helper container must be started")
	assert.Equal(t, "docker-updater-self-update", createdName)
	require.NotNil(t, createdCfg)
	assert.Equal(t, newImage, createdCfg.Image)
	assert.Equal(t,
		[]string{"finish-self-update", "--target", "selfid", "--name", "docker-updater", "--image", newImage},
		[]string(createdCfg.Cmd))
	assert.Equal(t, "true", createdCfg.Labels[selfUpdateHelperLabel])
	require.NotNil(t, createdHost)
	assert.True(t, createdHost.AutoRemove, "helper is a one-shot")
	assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock"}, createdHost.Binds)
	assert.Equal(t, container.NetworkMode("host"), createdHost.NetworkMode)

	// The whole point: self-update must never tear down our own container
	// inline -- that is what kills the updater mid-swap.
	assert.NotContains(t, stoppedIDs, "selfid")
	assert.NotContains(t, removedIDs, "selfid")
}

func TestSelfUpdateNoSocketFails(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
				Config:            &container.Config{},
			}, nil
		},
	}
	info := ContainerInfo{ID: "selfid", Name: "docker-updater"}
	err := selfUpdate(context.Background(), cli, info, "img")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Docker socket mount")
}

func TestUpdateContainerRoutesSelfToHelper(t *testing.T) {
	helperCreated := false
	selfStopped := false

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return selfInspect(), nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			if strings.HasSuffix(name, "-self-update") {
				helperCreated = true
			}
			return container.CreateResponse{ID: "helperid"}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			if id == "selfid" {
				selfStopped = true
			}
			return nil
		},
	}

	info := ContainerInfo{ID: "selfid", Name: "docker-updater", Image: "img"}
	cfg := Config{SelfContainerID: "selfid"}
	require.NoError(t, updateContainer(context.Background(), cli, info, cfg))

	assert.True(t, helperCreated, "updating our own container must go through the helper")
	assert.False(t, selfStopped, "our own container must not be stopped inline")
}

func TestUpdateContainerNonSelfRecreatesInline(t *testing.T) {
	stopped := false
	inspectCount := 0

	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:old",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "img"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			// New container reports healthy immediately so the recreate path
			// completes without a rollback.
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{Running: true, Health: &types.Health{Status: "healthy"}},
				},
			}, nil
		},
		containerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			if id == "appid" {
				stopped = true
			}
			return nil
		},
	}

	info := ContainerInfo{ID: "appid", Name: "app", Image: "img"}
	cfg := Config{SelfContainerID: "selfid"} // not us
	require.NoError(t, updateContainer(context.Background(), cli, info, cfg))
	assert.True(t, stopped, "a non-self container is recreated inline (old one stopped)")
}
