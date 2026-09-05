package main

import (
	"context"
	"errors"
	"testing"

	"bytes"
	"log"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// volumes builds a container.Config.Volumes value. The docker API spells that
// field as a bare map, so a real set type cannot stand in for it; this keeps the
// spelling to one place.
func volumes(paths ...string) map[string]struct{} {
	v := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		v[p] = struct{}{}
	}
	return v
}

// captureLog returns everything fn writes to the standard logger.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	fn()
	return buf.String()
}

// The incident this guards: a buildhost container created in August carried the
// entrypoint of the image it was created from, ["buildhost"]. The image later
// switched to an Actually Portable Executable, whose header is a shell script
// the kernel cannot exec. Every rolling update since cloned that entrypoint onto
// the new image, the replacement exited 126 immediately, the update rolled back,
// and the old container -- with the stale entrypoint -- was never replaced.
func TestClearInheritedImageDefaultsDropsTheOldImagesEntrypoint(t *testing.T) {
	t.Serial()
	config := &container.Config{
		Entrypoint: []string{"buildhost"},
		Cmd:        []string{"serve"},
		Image:      "ghcr.io/wow-look-at-my/buildhost:latest",
	}
	img := &container.Config{
		Entrypoint: []string{"buildhost"},
		Cmd:        []string{"serve"},
	}

	clearInheritedImageDefaults(config, img)

	assert.Nil(t, config.Entrypoint, "an inherited entrypoint must not pin the old image's spelling onto the new one")
	assert.Nil(t, config.Cmd)
	assert.Equal(t, "ghcr.io/wow-look-at-my/buildhost:latest", config.Image, "the image is the one field the caller sets deliberately")
}

func TestClearInheritedImageDefaultsKeepsAnOperatorsOwnOverride(t *testing.T) {
	t.Serial()
	config := &container.Config{
		Entrypoint: []string{"/bin/sh", "-c", "sleep infinity"},
		Cmd:        []string{"--debug"},
		User:       "1000:1000",
		WorkingDir: "/work",
		StopSignal: "SIGINT",
	}
	img := &container.Config{
		Entrypoint: []string{"buildhost"},
		Cmd:        []string{"serve"},
		User:       "nonroot",
		WorkingDir: "/",
		StopSignal: "SIGTERM",
	}

	clearInheritedImageDefaults(config, img)

	assert.Equal(t, []string{"/bin/sh", "-c", "sleep infinity"}, []string(config.Entrypoint))
	assert.Equal(t, []string{"--debug"}, []string(config.Cmd))
	assert.Equal(t, "1000:1000", config.User)
	assert.Equal(t, "/work", config.WorkingDir)
	assert.Equal(t, "SIGINT", config.StopSignal)
}

// Env, labels, ports and volumes are unions: the daemon adds the image's
// entries to the operator's, so only the matching ones may be dropped.
func TestClearInheritedImageDefaultsDropsOnlyTheImagesUnionEntries(t *testing.T) {
	t.Serial()
	config := &container.Config{
		Env:          []string{"PATH=/usr/bin", "BUILDHOST_DATA_DIR=/var/lib/buildhost", "BUILDHOST_BASE_URL=https://pazer.build"},
		Labels:       map[string]string{"org.opencontainers.image.source": "https://github.com/wow-look-at-my/buildhost", "docker-updater.enable": "true"},
		ExposedPorts: nat.PortSet{"8080/tcp": {}, "9090/tcp": {}},
		Volumes:      volumes("/var/lib/buildhost", "/scratch"),
	}
	img := &container.Config{
		Env:          []string{"PATH=/usr/bin", "BUILDHOST_DATA_DIR=/var/lib/buildhost"},
		Labels:       map[string]string{"org.opencontainers.image.source": "https://github.com/wow-look-at-my/buildhost"},
		ExposedPorts: nat.PortSet{"8080/tcp": {}},
		Volumes:      volumes("/var/lib/buildhost"),
	}

	clearInheritedImageDefaults(config, img)

	assert.Equal(t, []string{"BUILDHOST_BASE_URL=https://pazer.build"}, config.Env)
	assert.Equal(t, map[string]string{"docker-updater.enable": "true"}, config.Labels)
	assert.Equal(t, nat.PortSet{"9090/tcp": {}}, config.ExposedPorts)
	assert.Equal(t, volumes("/scratch"), config.Volumes)
}

// A healthcheck the image supplied has the same problem as an entrypoint: the
// buildhost one named the APE directly, so it could never have reported healthy
// against the fixed image either.
func TestClearInheritedImageDefaultsDropsAnInheritedHealthcheck(t *testing.T) {
	t.Serial()
	hc := func() *container.HealthConfig {
		return &container.HealthConfig{Test: []string{"CMD", "/usr/local/bin/buildhost", "healthcheck"}, Retries: 3}
	}
	config := &container.Config{Healthcheck: hc()}

	clearInheritedImageDefaults(config, &container.Config{Healthcheck: hc()})
	assert.Nil(t, config.Healthcheck)

	config = &container.Config{Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}}
	clearInheritedImageDefaults(config, &container.Config{Healthcheck: hc()})
	assert.Equal(t, []string{"NONE"}, config.Healthcheck.Test, "an operator's own healthcheck stays")
}

// A pruned old image leaves the config as inspected -- the update still runs --
// but the operator has to be able to find out why a default did not apply.
func TestClearInheritedDefaultsForReportsAnUninspectableImage(t *testing.T) {
	t.Serial()
	config := &container.Config{Entrypoint: []string{"buildhost"}}
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{}, nil, errors.New("no such image")
		},
	}

	logged := captureLog(t, func() {
		clearInheritedDefaultsFor(context.Background(), cli, config, "sha256:gone", "myapp")
	})

	assert.Equal(t, []string{"buildhost"}, []string(config.Entrypoint), "an unknown old image means nothing is known to be inherited")
	assert.Contains(t, logged, "ERROR")
	assert.Contains(t, logged, "myapp")
}

// The whole path, as the deployment runs it: a container whose entrypoint came
// from the image it was created from must not carry that entrypoint onto the
// replacement.
func TestRollingUpdateDoesNotPinTheOldImagesEntrypoint(t *testing.T) {
	t.Serial()
	var created *container.Config

	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config: &container.Config{
						Image:      "myapp:latest",
						Entrypoint: []string{"myapp"},
						Cmd:        []string{"serve"},
					},
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
		imageInspectFn: func(_ context.Context, imageID string) (types.ImageInspect, []byte, error) {
			require.Equal(t, "sha256:olddigest", imageID, "the config's defaults come from the image the container was created from")
			return types.ImageInspect{Config: &container.Config{
				Entrypoint: []string{"myapp"},
				Cmd:        []string{"serve"},
			}}, nil, nil
		},
		containerCreateFn: func(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			created = config
			return container.CreateResponse{ID: "new123456789"}, nil
		},
	}

	info := ContainerInfo{ID: "old123456789", Name: "myapp", Image: "myapp:latest", Rolling: true}
	require.Nil(t, rollingUpdateContainer(context.Background(), cli, info, "myapp:latest"))

	require.NotNil(t, created)
	assert.Nil(t, created.Entrypoint, "the replacement must take the new image's entrypoint")
	assert.Nil(t, created.Cmd)
	assert.Equal(t, "myapp:latest", created.Image)
}
