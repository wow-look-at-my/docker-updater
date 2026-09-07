package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frame wraps payload in Docker's 8-byte stream header, which is what a
// container without a TTY actually sends.
func frame(stream byte, payload string) []byte {
	var b bytes.Buffer
	b.Write([]byte{stream, 0, 0, 0})
	_ = binary.Write(&b, binary.BigEndian, uint32(len(payload)))
	b.WriteString(payload)
	return b.Bytes()
}

func TestDemuxDockerStream(t *testing.T) {
	multiplexed := append(frame(1, "starting\n"), frame(2, "exec /buildhost: no such file or directory\n")...)
	assert.Equal(t, "starting\nexec /buildhost: no such file or directory\n", string(demuxDockerStream(multiplexed)))

	// A TTY container sends no headers, and eating eight bytes of its first
	// line would be worse than doing nothing.
	plain := []byte("plain line with no framing at all\n")
	assert.Equal(t, plain, demuxDockerStream(plain))
	assert.Empty(t, demuxDockerStream(nil))

	// A truncated final frame yields what arrived rather than an error: the tail
	// is cut by the read limit, and a partial last line still says something.
	cut := append(frame(1, "kept\n"), []byte{1, 0, 0, 0, 0, 0, 0, 40}...)
	cut = append(cut, []byte("half a line")...)
	assert.Equal(t, "kept\nhalf a line", string(demuxDockerStream(cut)))
}

func TestLogExcerpt(t *testing.T) {
	logs := "2026-09-07T03:58:16.780049995Z starting\n" +
		"2026-09-07T03:58:16.999999999Z exec /buildhost: no such file or directory\n\n"
	assert.Equal(t, "exec /buildhost: no such file or directory", logExcerpt([]byte(logs)))

	// A line whose first word is not a timestamp keeps every character.
	assert.Equal(t, "panic: nil map", logExcerpt([]byte("panic: nil map\n")))
	assert.Equal(t, "2026-really-not-a-date here", logExcerpt([]byte("2026-really-not-a-date here\n")))
	assert.Empty(t, logExcerpt(nil))
	assert.Empty(t, logExcerpt([]byte("\n\n  \n")))
}

func TestIsFailing(t *testing.T) {
	assert.True(t, isFailing("restarting", "Restarting (126) 4 seconds ago"))
	assert.True(t, isFailing("dead", "Dead"))
	assert.True(t, isFailing("exited", "Exited (137) 13 days ago"))
	// A job that finished is not a failure, and reading its logs every poll
	// would be a Docker call for nothing.
	assert.False(t, isFailing("exited", "Exited (0) 10 days ago"))
	assert.False(t, isFailing("running", "Up 6 days (healthy)"))
	assert.False(t, isFailing("created", "Created"))
}

func TestHandleAPILogs(t *testing.T) {
	t.Serial()
	var gotName string
	var gotOpts container.LogsOptions
	cli := &mockDocker{
		containerLogsFn: func(_ context.Context, name string, opts container.LogsOptions) (io.ReadCloser, error) {
			gotName, gotOpts = name, opts
			return io.NopCloser(bytes.NewReader(frame(2, "exec /buildhost: no such file or directory\n"))), nil
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, newStore())

	rec := httptest.NewRecorder()
	s.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?container=buildhost-next&tail=50", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "buildhost-next", gotName, "the dashboard addresses a container by the name that survives a recreate")
	assert.True(t, gotOpts.ShowStdout && gotOpts.ShowStderr, "a process that dies usually says so on stderr")
	assert.Equal(t, "50", gotOpts.Tail)
	assert.Contains(t, rec.Body.String(), "exec /buildhost: no such file or directory")
	assert.NotContains(t, rec.Body.String(), "\x02", "the stream framing is stripped")
}

func TestHandleAPILogsRejectsAndBounds(t *testing.T) {
	t.Serial()
	var gotTail string
	cli := &mockDocker{
		containerLogsFn: func(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
			gotTail = opts.Tail
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute}, newStore())

	rec := httptest.NewRecorder()
	s.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "no container names nothing to read")

	rec = httptest.NewRecorder()
	s.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?container=a&tail=-3", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// An unbounded tail turns one click into a whole container's history.
	rec = httptest.NewRecorder()
	s.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?container=a&tail=999999", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2000", gotTail)

	// A container that logged nothing gets a sentence, not a blank panel that
	// reads as the dashboard failing.
	assert.Contains(t, rec.Body.String(), "written nothing")
}

func TestHandleAPILogsReportsADockerFailure(t *testing.T) {
	t.Serial()
	cli := &mockDocker{
		containerLogsFn: func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
			return nil, errors.New("no such container")
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute}, newStore())

	rec := httptest.NewRecorder()
	s.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?container=gone", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "no such container")
}

// The row carries the reason without a click, for a failing container only.
func TestAPIContainersCarriesTheReasonLine(t *testing.T) {
	t.Serial()
	var readFor []string
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{ID: "loop", Names: []string{"/buildhost-next"}, State: "restarting", Status: "Restarting (126) 4 seconds ago"},
				{ID: "ok", Names: []string{"/nginx"}, State: "running", Status: "Up 6 days (healthy)"},
			}, nil
		},
		containerLogsFn: func(_ context.Context, name string, _ container.LogsOptions) (io.ReadCloser, error) {
			readFor = append(readFor, name)
			return io.NopCloser(bytes.NewReader(frame(2, "2026-09-07T03:58:16Z exec /buildhost: no such file or directory\n"))), nil
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, newStore())

	rec := httptest.NewRecorder()
	s.handleAPIContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp apiResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byName := map[string]apiContainer{}
	for _, c := range resp.Containers {
		byName[c.Name] = c
	}
	assert.Equal(t, "exec /buildhost: no such file or directory", byName["buildhost-next"].LogExcerpt)
	assert.Empty(t, byName["nginx"].LogExcerpt, "a healthy container's last line is noise")
	assert.Equal(t, []string{"loop"}, readFor, "only the failing container costs a Docker call")
}
