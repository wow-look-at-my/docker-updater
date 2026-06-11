package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunHTTPCheckSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	info := ContainerInfo{
		Name:            "test",
		PreCheckURL:     server.URL + "/ready",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runPreCheck(context.Background(), nil, info)
	assert.Nil(t, err)
}

func TestRunHTTPCheckNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	info := ContainerInfo{
		Name:            "test",
		PreCheckURL:     server.URL + "/ready",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runPreCheck(context.Background(), nil, info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "status 503")
}

func TestRunHTTPCheckNoURL(t *testing.T) {
	info := ContainerInfo{
		Name:            "test",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runHTTPCheck(context.Background(), info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "url is empty")
}

func TestRunHTTPCheckConnectionError(t *testing.T) {
	info := ContainerInfo{
		Name:            "test",
		PreCheckURL:     "http://127.0.0.1:1/nonexistent",
		PreCheckTimeout: 1 * time.Second,
	}

	err := runPreCheck(context.Background(), nil, info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "pre-check HTTP request failed")
}

func TestRunExecCheckSuccess(t *testing.T) {
	cli := &mockDocker{
		containerExecCreateFn: func(_ context.Context, _ string, opts container.ExecOptions) (types.IDResponse, error) {
			assert.Equal(t, []string{"sh", "-c", "/check.sh"}, opts.Cmd)
			return types.IDResponse{ID: "exec-1"}, nil
		},
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 0}, nil
		},
	}

	info := ContainerInfo{
		ID:              "container-1",
		Name:            "test",
		PreCheckCommand: "/check.sh",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runPreCheck(context.Background(), cli, info)
	assert.Nil(t, err)
}

func TestRunExecCheckNonZeroExit(t *testing.T) {
	cli := &mockDocker{
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 1}, nil
		},
	}

	info := ContainerInfo{
		ID:              "container-1",
		Name:            "test",
		PreCheckCommand: "/check.sh",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runPreCheck(context.Background(), cli, info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "exited with code 1")
}

func TestRunExecCheckNoCommand(t *testing.T) {
	info := ContainerInfo{
		Name:            "test",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runExecCheck(context.Background(), nil, info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "command is empty")
}

func TestRunExecCheckTimeout(t *testing.T) {
	cli := &mockDocker{
		containerExecInspectFn: func(_ context.Context, _ string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: true}, nil
		},
	}

	info := ContainerInfo{
		ID:              "container-1",
		Name:            "test",
		PreCheckCommand: "/slow-check.sh",
		PreCheckTimeout: 1 * time.Second,
	}

	err := runPreCheck(context.Background(), cli, info)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestRunPreCheckURLTakesPrecedence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	info := ContainerInfo{
		Name:            "test",
		PreCheckURL:     server.URL + "/ready",
		PreCheckCommand: "/should-not-run.sh",
		PreCheckTimeout: 5 * time.Second,
	}

	err := runPreCheck(context.Background(), nil, info)
	assert.Nil(t, err)
}

func TestRunHTTPCheck2xxRange(t *testing.T) {
	for _, code := range []int{200, 201, 204, 299} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		info := ContainerInfo{
			Name:            "test",
			PreCheckURL:     server.URL,
			PreCheckTimeout: 5 * time.Second,
		}

		err := runPreCheck(context.Background(), nil, info)
		assert.Nil(t, err, "expected success for status %d", code)
		server.Close()
	}
}
