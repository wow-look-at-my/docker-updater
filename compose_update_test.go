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

const (
	oldServiceContainerID = "svc-old"
	newServiceContainerID = "svc-new"
	serviceImageRef       = "oci.example.test/app:latest"
	oldServiceImageID     = "sha256:oldimage"
	serviceComposeFile    = "/srv/claude-host/docker-compose.yml"
	serviceComposeDir     = "/srv/claude-host"
)

// composeServiceInfo is a compose-managed image-mode container: the shape of
// every service started by `docker compose up -d`.
func composeServiceInfo() ContainerInfo {
	return ContainerInfo{
		ID:                 oldServiceContainerID,
		Name:               "claude-host-server",
		Image:              serviceImageRef,
		Mode:               UpdateModeImage,
		ComposeProject:     "claude-host",
		ComposeService:     "server",
		ComposeConfigFiles: serviceComposeFile,
		ComposeWorkingDir:  serviceComposeDir,
	}
}

// composeDocker serves every update path from one fixture: the pre-update
// container reports the config the raw-API paths clone, and any other container
// is the replacement reporting health. The compose path re-finds its container
// by compose label, the raw paths health-check the one they just created, so
// neither needs to redefine inspection.
func composeDocker(health string) *mockDocker {
	return &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID: newServiceContainerID,
				Labels: map[string]string{
					"com.docker.compose.project": "claude-host",
					"com.docker.compose.service": "server",
				},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			if id == oldServiceContainerID {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      oldServiceImageID,
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: serviceImageRef},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{Running: true, Health: &types.Health{Status: health}},
				},
			}, nil
		},
	}
}

// recordCreates reports the names the client is asked to create containers
// under, which is how the raw-API paths are told apart from a compose converge.
func recordCreates(cli *mockDocker, names *[]string) {
	cli.containerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
		*names = append(*names, name)
		return container.CreateResponse{ID: "svc-next"}, nil
	}
}

// A compose-managed service must be converged with compose, never rebuilt from
// the old container's stored config: that is the only way an edited compose
// file (a newly added mount) reaches the running service.
func TestUpdateContainerConvergesComposeServiceThroughCompose(t *testing.T) {
	cli := composeDocker("healthy")
	var creates []string
	recordCreates(cli, &creates)
	runner := &fakeComposeRunner{}

	err := updateContainer(context.Background(), cli, runner, composeServiceInfo(), Config{SelfContainerID: "updater"})

	require.NoError(t, err)
	assert.Empty(t, creates, "a compose service must not be recreated from the old container's config")
	require.Equal(t, 1, runner.upNoDepsCalls, "the service must be converged through compose")
	assert.Equal(t, []string{"server"}, runner.upNoDepsServices)
	assert.Equal(t, [][]string{{serviceComposeFile}}, runner.upNoDepsFiles)
	assert.Equal(t, []string{serviceComposeDir}, runner.upNoDepsDirs)
}

// Converging one service must never sweep its dependencies along: the compose
// invocation carries --no-deps so a docker-in-docker daemon holding live
// containers is not recreated because a sibling took a new image.
func TestComposeConvergeIsScopedToTheServiceAlone(t *testing.T) {
	runner := &fakeComposeRunner{}

	require.NoError(t, recreateViaCompose(context.Background(), composeDocker("healthy"), runner, composeServiceInfo()))

	assert.Equal(t, 1, runner.upNoDepsCalls)
	assert.Zero(t, runner.upCalls, "a dependency-including `up -d` would restart dependencies")
}

// The rollback contract survives the move to compose: an unhealthy replacement
// re-tags the pinned previous image and converges the service back onto it.
func TestComposeConvergeRollsBackOnUnhealthy(t *testing.T) {
	cli := composeDocker("unhealthy")
	var tagged [][2]string
	cli.imageTagFn = func(_ context.Context, source, target string) error {
		tagged = append(tagged, [2]string{source, target})
		return nil
	}
	runner := &fakeComposeRunner{}

	err := recreateViaCompose(context.Background(), cli, runner, composeServiceInfo())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not healthy after update")
	assert.Contains(t, err.Error(), "rolled back to previous image")
	assert.Equal(t, [][2]string{{oldServiceImageID, serviceImageRef}}, tagged,
		"the previous image must be re-tagged so later cycles still see updates")
	assert.Equal(t, 2, runner.upNoDepsCalls, "rollback converges the service again on the previous image")
}

// A failed compose invocation must also restore the service rather than leave
// it wherever compose stopped.
func TestComposeConvergeRollsBackWhenComposeFails(t *testing.T) {
	runner := &fakeComposeRunner{upNoDepsErr: errors.New("compose config invalid")}

	err := recreateViaCompose(context.Background(), composeDocker("healthy"), runner, composeServiceInfo())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compose config invalid")
	assert.Contains(t, err.Error(), "ROLLBACK FAILED")
}

// Containers compose does not own (plain `docker run`) keep the raw-API
// recreate: there is no compose file to converge them against.
func TestUpdateContainerKeepsRawRecreateWithoutComposeLabels(t *testing.T) {
	cli := composeDocker("healthy")
	var creates []string
	recordCreates(cli, &creates)
	runner := &fakeComposeRunner{}

	info := ContainerInfo{ID: oldServiceContainerID, Name: "plain", Image: serviceImageRef, Mode: UpdateModeImage}
	require.NoError(t, updateContainer(context.Background(), cli, runner, info, Config{SelfContainerID: "updater"}))

	assert.Equal(t, []string{"plain"}, creates, "a non-compose container is recreated under its own name")
	assert.Zero(t, runner.upNoDepsCalls, "a non-compose container has no compose file to converge")
}

// Rolling updates must keep starting the replacement before draining the old
// container: compose cannot express that, so they stay on the raw-API path.
func TestRollingUpdateStaysOnRawPathForComposeService(t *testing.T) {
	cli := composeDocker("healthy")
	var creates []string
	recordCreates(cli, &creates)
	info := composeServiceInfo()
	info.Rolling = true
	runner := &fakeComposeRunner{}

	require.NoError(t, updateContainer(context.Background(), cli, runner, info, Config{SelfContainerID: "updater"}))

	assert.Equal(t, []string{"claude-host-server-next"}, creates,
		"rolling starts the replacement alongside the old container")
	assert.Zero(t, runner.upNoDepsCalls, "compose up would stop the old container first, breaking zero downtime")
}

// Git mode recreates a container whose image and compose config are unchanged,
// so it must not be routed through a converge that would be a no-op.
func TestGitModeKeepsForcedRecreate(t *testing.T) {
	cli := composeDocker("healthy")
	var creates []string
	recordCreates(cli, &creates)
	info := composeServiceInfo()
	info.Mode = UpdateModeGit
	runner := &fakeComposeRunner{}

	require.NoError(t, updateContainer(context.Background(), cli, runner, info, Config{SelfContainerID: "updater"}))

	assert.Equal(t, []string{"claude-host-server"}, creates, "git updates need a forced recreate")
	assert.Zero(t, runner.upNoDepsCalls, "`up -d` would skip a recreate when nothing drifted")
}

// Compose metadata must be captured for every mode, not just build mode:
// image-mode services are the common case and need it to converge.
func TestListMonitoredContainersCapturesComposeMetadataForImageMode(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:    "svc",
				Names: []string{"/claude-host-server"},
				Image: serviceImageRef,
				Labels: map[string]string{
					"docker-updater.enable":                   "true",
					"com.docker.compose.project":              "claude-host",
					"com.docker.compose.service":              "server",
					"com.docker.compose.project.config_files": serviceComposeFile,
					"com.docker.compose.project.working_dir":  serviceComposeDir,
				},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:img"},
				Config:            &container.Config{Image: serviceImageRef},
			}, nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{
				ID:          "sha256:img",
				RepoDigests: []string{"oci.example.test/app@sha256:abc"},
			}, nil, nil
		},
	}

	got, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, UpdateModeImage, got[0].Mode)
	assert.True(t, composeManaged(got[0]), "an image-mode compose service must be recognised as compose-managed")
	assert.Equal(t, "server", got[0].ComposeService)
	assert.Equal(t, serviceComposeDir, got[0].ComposeWorkingDir)
}
