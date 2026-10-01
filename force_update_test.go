package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forceServer is a dashboard server with a force queue, and a store in which
// "held" has an update pending and "current" does not.
func forceServer(t *testing.T, depth int) (*dashboardServer, chan string) {
	t.Helper()
	store := newStore()
	store.Record([]UpdateResult{
		{Container: ContainerInfo{Name: "held"}, OldRef: "sha256:aaa", NewRef: "sha256:bbb", Skipped: true, SkipReason: "busy"},
		{Container: ContainerInfo{Name: "current"}, OldRef: "sha256:aaa"},
	}, time.Now())
	s := newDashboardServer(&mockDocker{}, Config{Interval: time.Minute}, store)
	force := make(chan string, depth)
	s.force = force
	return s, force
}

func forceRequest(method, name string, header bool) *http.Request {
	r := httptest.NewRequest(method, "/api/force-update?container="+name, nil)
	if header {
		r.Header.Set(forceUpdateHeader, "force-update")
	}
	return r
}

func TestForceUpdateQueuesPendingContainer(t *testing.T) {
	s, force := forceServer(t, 1)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, forceRequest(http.MethodPost, "held", true))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Len(t, force, 1)
	assert.Equal(t, "held", <-force)
}

func TestForceUpdateRefusals(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		req    *http.Request
		status int
	}{
		{"GET is refused", forceRequest(http.MethodGet, "held", true), http.StatusMethodNotAllowed},
		{"a request without the header is refused", forceRequest(http.MethodPost, "held", false), http.StatusForbidden},
		{"a container with no pending update is refused", forceRequest(http.MethodPost, "current", true), http.StatusConflict},
		{"an unknown container is refused", forceRequest(http.MethodPost, "nope", true), http.StatusConflict},
		{"a missing name is refused", forceRequest(http.MethodPost, "", true), http.StatusBadRequest},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			s, force := forceServer(t, 1)
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, tc.req)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Empty(t, force, "a refused request must queue nothing")
		})
	}
}

func TestForceUpdateFullQueue(t *testing.T) {
	s, force := forceServer(t, 1)
	force <- "other"
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, forceRequest(http.MethodPost, "held", true))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Len(t, force, 1)
}

func TestForceUpdateDisabledWithoutQueue(t *testing.T) {
	s, _ := forceServer(t, 1)
	s.force = nil
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, forceRequest(http.MethodPost, "held", true))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestForceSkipsPreCheckAndDryRun(t *testing.T) {
	var asked atomic.Int64
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer gate.Close()

	info := ContainerInfo{Name: "app", State: "running", PreCheckURL: gate.URL, PreCheckTimeout: time.Second}
	assert.NotEmpty(t, preCheckRefuses(context.Background(), nil, info), "an unforced update waits for the gate")
	info.Force = true
	assert.Empty(t, preCheckRefuses(context.Background(), nil, info), "a forced update goes ahead")
	assert.Equal(t, int64(1), asked.Load(), "a forced update must not ask the gate")

	assert.True(t, dryRun(Config{DryRun: true}, ContainerInfo{}))
	assert.False(t, dryRun(Config{DryRun: true}, ContainerInfo{Force: true}), "a forced update applies in dry-run mode")
}

func TestRunLoopForceUpdate(t *testing.T) {
	t.Serial()
	var checks atomic.Int64
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			checks.Add(1)
			return nil, errors.New("no daemon")
		},
	}
	cfg := Config{Label: "docker-updater.enable", Interval: time.Hour}
	sigCh := make(chan os.Signal, 1)
	force := make(chan string, 1)
	store := newStore()

	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), cli, cfg, sigCh, newAuthResolver(nil), store, nil, force)
		close(done)
	}()

	waitFor(t, func() bool { return checks.Load() >= 1 }, "initial check did not run")
	lastCycle := store.Snapshot().LastCycle

	force <- "held"
	waitFor(t, func() bool { return checks.Load() >= 2 }, "a force request did not run a check")
	assert.Equal(t, lastCycle, store.Snapshot().LastCycle, "a forced update is not a full cycle")

	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after signal")
	}
}

func TestCheckContainersForceSelectsOneContainer(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{ID: "a", Names: []string{"/one"}, Image: "sha256:abc", Labels: map[string]string{"docker-updater.enable": "true"}},
				{ID: "b", Names: []string{"/two"}, Image: "sha256:abc", Labels: map[string]string{"docker-updater.enable": "true"}},
			}, nil
		},
		// A bare image ID makes each container unmonitorable, so the check
		// probes no network and pulls nothing.
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:abc", HostConfig: &container.HostConfig{}},
				Config:            &container.Config{Image: "sha256:abc"},
				NetworkSettings:   &types.NetworkSettings{},
			}, nil
		},
	}
	results := runForceUpdate(context.Background(), cli, Config{Label: "docker-updater.enable"}, newAuthResolver(nil), "two")
	require.Len(t, results, 1)
	assert.Equal(t, "two", results[0].Container.Name)
	assert.True(t, results[0].Container.Force)
}
