package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveJSON(t *testing.T, addr, body string) int {
	t.Helper()
	ln, err := net.Listen("tcp", addr+":0")
	require.NoError(t, err)
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().(*net.TCPAddr).Port
}

func inspectStartedAt(ip string, startedAt time.Time) types.ContainerJSON {
	inspect := inspectWithIP(ip)
	inspect.State = &types.ContainerState{Running: true, StartedAt: startedAt.Format(time.RFC3339Nano)}
	return inspect
}

func startedInfo(port int, timeout time.Duration) ContainerInfo {
	return ContainerInfo{
		Name:                        "myapp",
		HealthCheckURL:              fmt.Sprintf("http://127.0.0.1:%d%s", port, wellKnownHealth),
		HealthCheckURLFromContainer: true,
		HealthCheckTimeout:          timeout,
	}
}

func TestWaitPostUpdateHealthyRejectsAnswerFromOlderProcess(t *testing.T) {
	containerStart := time.Now().UTC()
	stale := containerStart.Add(-time.Hour).Format(time.RFC3339)
	port := serveJSON(t, "127.0.0.2", `{"status":"ok","started":"`+stale+`"}`)
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return inspectStartedAt("127.0.0.2", containerStart), nil
		},
	}

	err := waitPostUpdateHealthy(context.Background(), cli, "new123456789", startedInfo(port, 300*time.Millisecond))
	require.Error(t, err, "a 200 from a process older than the container must not pass the gate")
	assert.Contains(t, err.Error(), "before the new container started")
}

func TestWaitPostUpdateHealthyAcceptsAnswerFromNewProcess(t *testing.T) {
	containerStart := time.Now().UTC().Add(-time.Minute)
	fresh := containerStart.Add(time.Second).Format(time.RFC3339)
	port := serveJSON(t, "127.0.0.2", `{"status":"ok","started":"`+fresh+`"}`)
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return inspectStartedAt("127.0.0.2", containerStart), nil
		},
	}

	require.NoError(t, waitPostUpdateHealthy(context.Background(), cli, "new123456789", startedInfo(port, 2*time.Second)))
}

func TestStaleAnswer(t *testing.T) {
	start := time.Date(2026, 10, 1, 5, 0, 0, 900_000_000, time.UTC)
	assert.Empty(t, staleAnswer([]byte(`{"started":"2026-10-01T05:00:00Z"}`), start), "a whole-second started time matches its own container")
	assert.Empty(t, staleAnswer([]byte(`not json`), start), "a body that is not JSON falls back to the status code")
	assert.Empty(t, staleAnswer([]byte(`{"status":"ok"}`), start), "a body without started falls back to the status code")
	assert.Empty(t, staleAnswer([]byte(`{"started":"2020-01-01T00:00:00Z"}`), time.Time{}), "no container start time turns the check off")
	assert.Contains(t, staleAnswer([]byte(`{"started":"2026-10-01T04:59:59Z"}`), start), "before the new container started")
	assert.Contains(t, staleAnswer([]byte(`{"started":"yesterday"}`), start), "not an RFC 3339 time")
}
