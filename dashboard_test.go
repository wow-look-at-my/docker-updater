package main

import (
	"context"
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

func TestParseHealth(t *testing.T) {
	assert.Equal(t, "healthy", parseHealth("Up 2 hours (healthy)"))
	assert.Equal(t, "unhealthy", parseHealth("Up 5 minutes (unhealthy)"))
	assert.Equal(t, "starting", parseHealth("Up 3 seconds (health: starting)"))
	assert.Equal(t, "", parseHealth("Up 2 hours"))
	assert.Equal(t, "", parseHealth("Exited (0) 1 hour ago"))
}

func TestContainerName(t *testing.T) {
	assert.Equal(t, "web", containerName([]string{"/web"}))
	assert.Equal(t, "web", containerName([]string{"web"}))
	assert.Equal(t, "", containerName(nil))
}

func TestNonZeroTime(t *testing.T) {
	assert.Nil(t, nonZeroTime(time.Time{}))
	now := time.Now()
	require.NotNil(t, nonZeroTime(now))
	assert.Equal(t, now, *nonZeroTime(now))
}

func mockContainers() []types.Container {
	return []types.Container{
		{
			Names:   []string{"/web"},
			Image:   "nginx:latest",
			ImageID: "sha256:webimageid000000",
			State:   "running",
			Status:  "Up 2 hours (healthy)",
			Created: time.Now().Add(-2 * time.Hour).Unix(),
			Labels:  map[string]string{"docker-updater.enable": "true"},
		},
		{
			Names:   []string{"/api"},
			Image:   "myapi:latest",
			ImageID: "sha256:apiimageid000000",
			State:   "running",
			Status:  "Up 5 minutes (unhealthy)",
			Created: time.Now().Add(-5 * time.Minute).Unix(),
			Labels:  map[string]string{"docker-updater.enable": "true", "docker-updater.mode": "git"},
		},
		{
			Names:   []string{"/cache"},
			Image:   "redis:7",
			ImageID: "sha256:redisimageid0000",
			State:   "running",
			Status:  "Up 3 days",
			Created: time.Now().Add(-72 * time.Hour).Unix(),
			Labels:  map[string]string{},
		},
		{
			Names:   []string{"/old"},
			Image:   "old:1",
			ImageID: "sha256:oldimageid000000",
			State:   "exited",
			Status:  "Exited (0) 1 hour ago",
			Created: time.Now().Add(-96 * time.Hour).Unix(),
			Labels:  map[string]string{},
		},
	}
}

func TestHandleAPIContainers(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
			assert.True(t, opts.All, "dashboard must list stopped containers too")
			return mockContainers(), nil
		},
	}

	store := newStore()
	now := time.Now()
	store.Record([]UpdateResult{
		{
			Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
			OldRef:    "sha256:webcurrentdigest",
			Pulled:    true,
			CheckedAt: now,
		},
		{
			Container:  ContainerInfo{Name: "api", Image: "myapi:latest", Mode: UpdateModeGit},
			Skipped:    true,
			SkipReason: "pre-check failed",
			NewRef:     "abcdef0123456789",
			CheckedAt:  now,
		},
	}, now)

	cfg := Config{Interval: 5 * time.Minute, Label: "docker-updater.enable"}
	s := newDashboardServer(cli, cfg, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
	s.handleAPIContainers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var resp apiResponse
	require.Nil(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "5m0s", resp.Interval)
	assert.Equal(t, "docker-updater.enable", resp.Label)
	assert.False(t, resp.DryRun)
	require.NotNil(t, resp.LastCycle)
	require.NotNil(t, resp.NextCycle)
	assert.Equal(t, resp.LastCycle.Add(5*time.Minute).Unix(), resp.NextCycle.Unix())

	require.Len(t, resp.Containers, 4)

	// Monitored containers sort first, then alphabetically: api, web, then
	// the manual ones cache, old.
	assert.Equal(t, "api", resp.Containers[0].Name)
	assert.Equal(t, "web", resp.Containers[1].Name)
	assert.Equal(t, "cache", resp.Containers[2].Name)
	assert.Equal(t, "old", resp.Containers[3].Name)

	byName := map[string]apiContainer{}
	for _, c := range resp.Containers {
		byName[c.Name] = c
	}

	web := byName["web"]
	assert.True(t, web.AutoUpdate)
	assert.Equal(t, "image", web.Mode)
	assert.Equal(t, "healthy", web.Health)
	assert.False(t, web.UpdateAvailable)
	require.NotNil(t, web.LastChecked)
	require.NotNil(t, web.LastPulled)

	api := byName["api"]
	assert.True(t, api.AutoUpdate)
	assert.Equal(t, "git", api.Mode)
	assert.Equal(t, "unhealthy", api.Health)
	assert.True(t, api.UpdateAvailable)
	assert.True(t, api.Skipped)
	assert.Equal(t, "pre-check failed", api.SkipReason)
	assert.Equal(t, "abcdef012345", api.AvailableRef)
	// Git checks don't pull an image, so last_pulled stays unset.
	assert.Nil(t, api.LastPulled)

	cache := byName["cache"]
	assert.False(t, cache.AutoUpdate)
	assert.Empty(t, cache.Mode)
	assert.Nil(t, cache.LastChecked, "manual containers have no tracked status")

	old := byName["old"]
	assert.False(t, old.AutoUpdate)
	assert.Equal(t, "exited", old.State)
}

func TestHandleAPIContainersRestarts(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{ID: "c-web", Names: []string{"/web"}, Image: "nginx:latest", State: "running", Status: "Up 1 hour", Labels: map[string]string{"docker-updater.enable": "true"}},
				{ID: "c-cache", Names: []string{"/cache"}, Image: "redis:7", State: "running", Status: "Up 2 hours"},
				{ID: "c-broken", Names: []string{"/broken"}, Image: "x:1", State: "exited", Status: "Exited (1) 1 minute ago"},
			}, nil
		},
		containerInspectFn: func(_ context.Context, id string) (types.ContainerJSON, error) {
			switch id {
			case "c-web":
				return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{RestartCount: 3}}, nil
			case "c-cache":
				return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{RestartCount: 0}}, nil
			default:
				return types.ContainerJSON{}, errors.New("inspect failed")
			}
		},
	}

	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, newStore())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
	s.handleAPIContainers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp apiResponse
	require.Nil(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byName := map[string]apiContainer{}
	for _, c := range resp.Containers {
		byName[c.Name] = c
	}

	require.NotNil(t, byName["web"].Restarts)
	assert.Equal(t, 3, *byName["web"].Restarts)

	require.NotNil(t, byName["cache"].Restarts, "zero restarts is reported, not omitted")
	assert.Equal(t, 0, *byName["cache"].Restarts)

	assert.Nil(t, byName["broken"].Restarts, "an inspect failure leaves restarts unknown")
}

func TestHandleAPIContainersListError(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return nil, errors.New("docker daemon down")
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute}, newStore())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
	s.handleAPIContainers(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "docker daemon down")
}

func TestHandleAPIContainersNoCycleYet(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return mockContainers(), nil
		},
	}
	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, newStore())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
	s.handleAPIContainers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp apiResponse
	require.Nil(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.LastCycle, "no cycle recorded yet")
	assert.Nil(t, resp.NextCycle)
	assert.Len(t, resp.Containers, 4)
}

func TestDashboardServesStaticAssets(t *testing.T) {
	s := newDashboardServer(&mockDocker{}, Config{Interval: time.Minute}, newStore())
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	cases := []struct {
		path     string
		wantBody string
	}{
		{"/", "docker-updater"},
		{"/dashboard.css", ":root"},
		{"/dashboard.js", "REFRESH_SECONDS"},
		{"/healthz", "ok"},
	}
	for _, tc := range cases {
		resp, err := http.Get(ts.URL + tc.path)
		require.Nil(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, tc.path)
		assert.Contains(t, string(body), tc.wantBody, tc.path)
	}

	// The JSON API is reachable through the mux too.
	resp, err := http.Get(ts.URL + "/api/containers")
	require.Nil(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

// TestDashboardAssetCaching locks in the validator contract for the embedded
// assets: strong SHA-256 ETags computed from the embedded bytes, Cache-Control
// no-cache so every load (browser or intermediary cache) revalidates, 304 on a
// matching If-None-Match, and no Last-Modified (embedded files have a zero,
// meaningless modtime). Without this a cache in front could keep serving an
// old dashboard.js against a new index.html after a deploy — the compiled JS
// then dereferences element ids the other version's markup doesn't have.
func TestDashboardAssetCaching(t *testing.T) {
	s := newDashboardServer(&mockDocker{}, Config{Interval: time.Minute}, newStore())
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	etags := map[string]string{}
	for _, path := range []string{"/", "/index.html", "/dashboard.css", "/dashboard.js"} {
		resp, err := http.Get(ts.URL + path)
		require.Nil(t, err, path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"), path)
		assert.Empty(t, resp.Header.Get("Last-Modified"), "zero-modtime embed must not fabricate Last-Modified: %s", path)
		assert.NotEmpty(t, body, path)

		etag := resp.Header.Get("ETag")
		require.NotEmpty(t, etag, path)
		assert.Regexp(t, `^"[0-9a-f]{64}"$`, etag, "strong quoted hex SHA-256 ETag: %s", path)
		etags[path] = etag

		// A conditional GET with the matching validator revalidates to a
		// bodyless 304 — the cheap steady-state path.
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		require.Nil(t, err, path)
		req.Header.Set("If-None-Match", etag)
		resp, err = http.DefaultClient.Do(req)
		require.Nil(t, err, path)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotModified, resp.StatusCode, path)
		assert.Empty(t, body, path)

		// A stale validator (old deploy's hash) gets the full new body.
		req, err = http.NewRequest(http.MethodGet, ts.URL+path, nil)
		require.Nil(t, err, path)
		req.Header.Set("If-None-Match", `"`+strings.Repeat("0", 64)+`"`)
		resp, err = http.DefaultClient.Do(req)
		require.Nil(t, err, path)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.NotEmpty(t, body, path)
	}

	// "/" is index.html under another name.
	assert.Equal(t, etags["/index.html"], etags["/"])
	// Distinct files carry distinct validators, so each asset revalidates
	// independently after a deploy.
	assert.NotEqual(t, etags["/dashboard.js"], etags["/dashboard.css"])
	assert.NotEqual(t, etags["/dashboard.js"], etags["/index.html"])

	// Unknown paths still 404 — the asset handler never falls back to index.html.
	resp, err := http.Get(ts.URL + "/no-such-file.txt")
	require.Nil(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestDashboardVersionStampedAssetURLs locks in the cache-key busting: the
// served index.html references its assets as dashboard.js?v=<hash-prefix> /
// dashboard.css?v=<hash-prefix>, stamped from each asset's own content hash.
// Any HTTP cache — including a cache-everything edge rule, whose default cache
// key includes the query string — then stores each asset version under a
// distinct key, so an index.html can only ever pull the exact js/css it
// shipped with; an old-script/new-page pairing is impossible. The query string
// must be a pure cache key: serving ignores it.
func TestDashboardVersionStampedAssetURLs(t *testing.T) {
	s := newDashboardServer(&mockDocker{}, Config{Interval: time.Minute}, newStore())
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(ts.URL + path)
		require.Nil(t, err, path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	_, page := get("/")

	for asset, attr := range map[string]string{
		"dashboard.js":  "src",
		"dashboard.css": "href",
	} {
		resp, plain := get("/" + asset)
		require.Equal(t, http.StatusOK, resp.StatusCode, asset)
		etag := resp.Header.Get("ETag")
		require.Regexp(t, `^"[0-9a-f]{64}"$`, etag, asset)

		// index.html references the asset stamped with a prefix of the very
		// hash the asset itself is served under — never the bare name.
		stampedRef := attr + `="` + asset + `?v=` + strings.Trim(etag, `"`)[:assetVersionLen] + `"`
		assert.Contains(t, page, stampedRef, "index.html must reference %s stamped with its content hash", asset)
		assert.NotContains(t, page, attr+`="`+asset+`"`, "bare (unstamped) %s reference must be gone", asset)

		// The stamped URL — any ?v=, in fact — resolves to the same bytes and
		// validator as the bare path.
		respV, stamped := get("/" + asset + "?v=anything")
		assert.Equal(t, http.StatusOK, respV.StatusCode, asset)
		assert.Equal(t, etag, respV.Header.Get("ETag"), asset)
		assert.Equal(t, plain, stamped, asset)
		assert.Equal(t, "no-cache", respV.Header.Get("Cache-Control"), asset)
	}
}

func TestDashboardServerRunShutsDown(t *testing.T) {
	s := newDashboardServer(&mockDocker{}, Config{DashboardAddr: "127.0.0.1:0", Interval: time.Minute}, newStore())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx)
		close(done)
	}()

	// Give the server a moment to bind, then cancel the context.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dashboard server did not shut down after context cancellation")
	}
}

func TestUptimeTextHelperViaJSAssetExists(t *testing.T) {
	// Guard against accidentally dropping the embedded JS that powers the UI,
	// or shipping a stale dashboard.js compiled before a feature landed in
	// dashboard.ts. Each entry is a function declaration tsc preserves
	// verbatim in the compiled artifact.
	data, err := dashboardAssets.ReadFile("dashboard/dashboard.js")
	require.Nil(t, err)
	js := string(data)
	for _, fn := range []string{
		"function uptimeText",            // status-string helper (original canary)
		"function isOnline",              // four-group split (managed/unmanaged × online/offline)
		"function onSearchInput",         // search/filter box handler
		"function updatedHighlight",      // recently-updated green fade
		"function renderOutOfSyncBanner", // mixed-asset startup guard
	} {
		assert.True(t, strings.Contains(js, fn), "compiled dashboard.js is missing %q — run `cd dashboard && npm run build` and commit the result", fn)
	}
}
