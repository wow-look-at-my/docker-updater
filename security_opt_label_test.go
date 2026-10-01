package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
)

func imageWithLabels(labels map[string]string) *mockDocker {
	return &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{Config: &container.Config{Labels: labels}}, nil, nil
		},
	}
}

func inspectWith(binds, secOpts []string) types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{Binds: binds, SecurityOpt: secOpts},
		},
	}
}

func TestImageSecurityOptsAddedForDockerSocketHolder(t *testing.T) {
	cli := imageWithLabels(map[string]string{securityOptLabel: "seccomp=unconfined  systempaths=unconfined"})
	inspect := inspectWith([]string{"/var/run/docker.sock:/var/run/docker.sock"}, []string{"label=disable"})

	got := withImageSecurityOpts(context.Background(), cli, inspect, "runner:new", "runner")

	assert.Equal(t, []string{"label=disable", "seccomp=unconfined", "systempaths=unconfined"}, got.SecurityOpt)
	assert.Equal(t, []string{"label=disable"}, inspect.HostConfig.SecurityOpt, "the rollback reuses the original HostConfig, so it must stay unchanged")
}

func TestImageSecurityOptsRefusedWithoutDockerSocket(t *testing.T) {
	cli := imageWithLabels(map[string]string{securityOptLabel: "seccomp=unconfined"})
	inspect := inspectWith([]string{"/data:/data"}, nil)

	got := withImageSecurityOpts(context.Background(), cli, inspect, "app:new", "app")

	assert.Empty(t, got.SecurityOpt, "a container without the Docker socket must not lift its own confinement")
}

func TestImageSecurityOptsAlreadyPresentAreNotDuplicated(t *testing.T) {
	cli := imageWithLabels(map[string]string{securityOptLabel: "seccomp=unconfined"})
	inspect := inspectWith([]string{"/var/run/docker.sock:/var/run/docker.sock"}, []string{"seccomp=unconfined"})

	got := withImageSecurityOpts(context.Background(), cli, inspect, "runner:new", "runner")

	assert.Equal(t, []string{"seccomp=unconfined"}, got.SecurityOpt)
}

func TestImageSecurityOptsNoLabelLeavesHostConfig(t *testing.T) {
	cli := imageWithLabels(nil)
	inspect := inspectWith([]string{"/var/run/docker.sock:/var/run/docker.sock"}, nil)

	got := withImageSecurityOpts(context.Background(), cli, inspect, "runner:new", "runner")

	assert.Same(t, inspect.HostConfig, got)
}

// Compose owns a compose-managed container, so a label it cannot apply must be
// reported, naming the file to edit.
func TestComposeSecurityOptGapNamesTheComposeFile(t *testing.T) {
	cli := imageWithLabels(map[string]string{securityOptLabel: "seccomp=unconfined systempaths=unconfined"})
	inspect := inspectWith(nil, []string{"seccomp=unconfined"})
	inspect.Image = "sha256:new"
	inspect.Config = &container.Config{}
	info := ContainerInfo{Name: "webhook-runner", ComposeService: "webhook-runner", ComposeConfigFiles: "/srv/compose.yml"}

	msg := composeSecurityOptGap(context.Background(), cli, inspect, info)
	assert.Contains(t, msg, "ERROR container webhook-runner")
	assert.Contains(t, msg, "--security-opt systempaths=unconfined")
	assert.NotContains(t, msg, "seccomp=unconfined systempaths", "an option the container already has is not reported")
	assert.Contains(t, msg, "service webhook-runner in /srv/compose.yml")

	inspect.HostConfig.SecurityOpt = []string{"seccomp=unconfined", "systempaths=unconfined"}
	assert.Empty(t, composeSecurityOptGap(context.Background(), cli, inspect, info), "nothing missing, nothing reported")
}

func TestImageSecurityOptsInspectFailureLeavesHostConfig(t *testing.T) {
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{}, nil, errors.New("no such image")
		},
	}
	inspect := inspectWith([]string{"/var/run/docker.sock:/var/run/docker.sock"}, nil)

	got := withImageSecurityOpts(context.Background(), cli, inspect, "runner:new", "runner")

	assert.Same(t, inspect.HostConfig, got)
}
