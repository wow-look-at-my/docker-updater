package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A gate that says only "it restarted" leaves the operator to read the logs of
// a container the rollback has already replaced. Two entries sat stuck for
// hours that way, with the reason sitting in a log line nobody was shown.
func TestWaitStaysRunning_ReportsWhyItDied(t *testing.T) {
	t.Serial()
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State:        &types.ContainerState{Running: true},
					RestartCount: 3,
				},
			}, nil
		},
		containerLogsFn: func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(frame(2, "runner: config.yml: no such file\n"))), nil
		},
	}

	err := waitStaysRunning(context.Background(), cli, "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restarted within")
	assert.Contains(t, err.Error(), "runner: config.yml: no such file")
}

// Docker gives an endpoint an IP only while the container runs, so a crash
// loop and host networking produce the same empty address. Naming the network
// mode for both sends the operator to a label that changes nothing.
func TestNoEndpointError_NotRunningIsNotANetworkMode(t *testing.T) {
	t.Serial()
	cli := &mockDocker{
		containerLogsFn: func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(frame(2, "exec: /app: permission denied\n"))), nil
		},
	}
	inspect := types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{
		State: &types.ContainerState{Running: false, Status: "restarting", ExitCode: 126},
	}}

	err := noEndpointError(context.Background(), cli, "abc123", inspect)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restarting")
	assert.Contains(t, err.Error(), "exit 126")
	assert.Contains(t, err.Error(), "exec: /app: permission denied")
	assert.NotContains(t, err.Error(), "health-check.url",
		"a container that is not running does not need a different label")
}

func TestNoEndpointError_RunningKeepsTheNetworkModeAdvice(t *testing.T) {
	t.Serial()
	inspect := types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{
		State: &types.ContainerState{Running: true, Status: "running"},
	}}

	err := noEndpointError(context.Background(), &mockDocker{}, "abc123", inspect)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "health-check.url")
}
