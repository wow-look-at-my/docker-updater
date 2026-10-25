package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bifrostRunningImage = "bifrost-patched:2.3.0"
	bifrostFileImage    = "ghcr.io/wow-look-at-my/bifrost:latest"
)

// uncheckableCycle runs one cycle over a compose service whose running image
// is in no registry, so the image check fails. Its file names another image.
// force names the container to force, or is "".
func uncheckableCycle(t *testing.T, cfg Config, queue recreateQueue, labels map[string]string, force string) ([]UpdateResult, *fakeComposeRunner, string) {
	t.Helper()
	runner := &fakeComposeRunner{
		configHash:       fileConfigHash,
		createRequestErr: errors.New("docker compose up --pull never: exit status 1: Error response from daemon: No such image: " + bifrostFileImage),
	}
	saved := defaultComposeRunner
	defaultComposeRunner = runner
	t.Cleanup(func() { defaultComposeRunner = saved })

	all := map[string]string{
		"docker-updater.enable":                   "true",
		"docker-updater.well-known":               "false",
		"com.docker.compose.project":              "litellm",
		"com.docker.compose.service":              "bifrost",
		"com.docker.compose.project.config_files": "/srv/litellm/docker-compose.yml",
		"com.docker.compose.project.working_dir":  "/srv/litellm",
		composeConfigHashLabel:                    runningConfigHash,
	}
	for k, v := range labels {
		all[k] = v
	}
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "bifrost-1",
				Names:  []string{"/bifrost"},
				Image:  bifrostRunningImage,
				State:  "running",
				Labels: all,
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					Image: "sha256:patched",
					State: &types.ContainerState{Running: true, Health: &types.Health{Status: "healthy"}},
				},
				Config: &container.Config{Image: bifrostRunningImage},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("pull access denied for bifrost-patched, repository does not exist")
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:patched"}, nil, nil
		},
	}

	cfg.Label = "docker-updater.enable"
	var results []UpdateResult
	logged := captureLog(t, func() {
		if force != "" {
			results = runForceUpdate(context.Background(), cli, cfg, newAuthResolver(nil), queue, force)
			return
		}
		results = runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil), queue)
	})
	require.Len(t, results, 1)
	return results, runner, logged
}

// A queued recreate runs when the check of the running image fails: the file
// names another image, and the recreate moves the container onto it.
func TestUncheckableImageQueuedRecreate(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(refusing.Close)
	gate := map[string]string{"docker-updater.pre-check.url": refusing.URL, "docker-updater.pre-check.timeout": "1s"}

	cases := []struct {
		name         string
		cfg          Config
		queued       bool
		labels       map[string]string
		force        bool
		wantRecreate bool
		wantError    bool
		wantSkipped  bool
		wantUpdated  bool
		wantStuck    bool
	}{
		{name: "queued", queued: true, wantRecreate: true, wantUpdated: true},
		{name: "nothing queued", wantError: true, wantStuck: true},
		{name: "queued, pre-check refuses", queued: true, labels: gate, wantError: true, wantSkipped: true, wantStuck: true},
		{name: "queued, pre-check refuses, forced", queued: true, labels: gate, force: true, wantRecreate: true, wantUpdated: true},
		{name: "queued, dry-run", cfg: Config{DryRun: true}, queued: true, wantError: true, wantUpdated: true, wantStuck: true},
		{name: "queued, dry-run, forced", cfg: Config{DryRun: true}, queued: true, force: true, wantRecreate: true, wantUpdated: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Serial()
			store := newStore()
			if tc.queued {
				store.QueueRecreate("bifrost", fileConfigHash, time.Now())
			}
			force := ""
			if tc.force {
				force = "bifrost"
			}
			results, runner, _ := uncheckableCycle(t, tc.cfg, store, tc.labels, force)
			r := results[0]

			assert.Equal(t, tc.wantError, r.Error != nil, "error: %v", r.Error)
			assert.Equal(t, tc.wantSkipped, r.Skipped)
			assert.Equal(t, tc.wantUpdated, r.Updated)
			if tc.wantRecreate {
				assert.Equal(t, 1, runner.upNoDepsCalls)
				assert.Equal(t, []string{"bifrost"}, runner.upNoDepsServices)
				assert.Nil(t, r.Drift, "the recreate applied the file")
				_, still := store.QueuedRecreate("bifrost")
				assert.False(t, still, "a done recreate leaves the queue")
			} else {
				assert.Zero(t, runner.upNoDepsCalls)
				require.NotNil(t, r.Drift)
				assert.True(t, r.Drift.applies())
			}
			if tc.wantSkipped {
				assert.Contains(t, r.SkipReason, "503")
			}
			if tc.wantError {
				assert.Contains(t, r.Error.Error(), "is not in its registry")
				assert.Contains(t, r.Error.Error(), "The compose file names "+bifrostFileImage)
			}

			// A second cycle with the same outcome makes the run STUCK. A recreate ends it.
			store.Record(results, time.Now())
			store.Record(results, time.Now())
			st := store.Snapshot().Statuses["bifrost"]
			assert.Equal(t, tc.wantStuck, st.Stuck())
			if !tc.wantStuck {
				assert.Empty(t, st.LastError, "the error was about the image the recreate replaced")
			}
			if tc.wantSkipped {
				assert.Equal(t, r.SkipReason, st.SkipReason, "the dashboard says why the queued recreate waits")
			}
		})
	}
}

// With nothing queued, the error names the recreate as the way off the image.
func TestUncheckableImageErrorSuggestsTheRecreate(t *testing.T) {
	t.Serial()
	results, _, _ := uncheckableCycle(t, Config{}, newStore(), nil, "")

	r := results[0]
	require.Error(t, r.Error)
	assert.True(t, r.CheckFailed)
	assert.Contains(t, r.Error.Error(), "queue a recreate from the compose file on the dashboard to move the container off this image")

	var lines []string
	store := newStore()
	store.Record(results, time.Now())
	store.Record(results, time.Now())
	reportStuck(store.Snapshot(), time.Now(), func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "bifrost is STUCK")
	assert.Contains(t, lines[0], "queue a recreate from the compose file on the dashboard")
}

// A recreate the operator queued while the check fails still applies. Its
// differences say the image is not pulled instead of a raw compose error.
func TestDriftWithMissingFileImageStillApplies(t *testing.T) {
	t.Serial()
	results, _, _ := uncheckableCycle(t, Config{}, newStore(), nil, "")

	d := results[0].Drift
	require.NotNil(t, d)
	assert.True(t, d.applies(), "an unlisted difference never holds back the recreate button")
	assert.Equal(t, bifrostFileImage, d.MissingImage)
	assert.Equal(t, bifrostFileImage+" is not pulled yet; the recreate pulls it", d.DifferencesError)
	assert.Empty(t, d.Differences)
}

func TestComposeDriftFileImage(t *testing.T) {
	cases := []struct {
		name string
		d    *ComposeDrift
		want string
	}{
		{"no drift", nil, ""},
		{"missing image", &ComposeDrift{MissingImage: bifrostFileImage}, bifrostFileImage},
		{"image difference", &ComposeDrift{Differences: []ComposeDifference{{Field: "Config.Image", File: `"` + bifrostFileImage + `"`, Container: `"` + bifrostRunningImage + `"`}}}, bifrostFileImage},
		{"same image", &ComposeDrift{MissingImage: bifrostRunningImage}, ""},
		{"other differences only", &ComposeDrift{Differences: []ComposeDifference{{Field: "HostConfig.Memory", File: "1", Container: "2"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.d.fileImage(bifrostRunningImage))
		})
	}
}

// Only a failed check lets a queued recreate run. A failed update never runs it a second time.
func TestApplyComposeDriftAfterAnError(t *testing.T) {
	info := queuedInfo()
	cases := []struct {
		name        string
		checkFailed bool
		wantCalls   int
	}{
		{"failed check", true, 1},
		{"failed update", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeComposeRunner{}
			in := UpdateResult{Container: info, Drift: &ComposeDrift{State: driftFound}, Error: errors.New("boom"), CheckFailed: tc.checkFailed}
			var r UpdateResult
			captureLog(t, func() {
				r = applyComposeDrift(context.Background(), composeDocker("healthy"), runner, info, Config{}, in)
			})
			assert.Equal(t, tc.wantCalls, runner.upNoDepsCalls)
			if tc.wantCalls == 0 {
				assert.EqualError(t, r.Error, "boom")
				assert.NotNil(t, r.Drift)
				return
			}
			assert.NoError(t, r.Error)
			assert.True(t, r.Updated)
			assert.Nil(t, r.Drift)
		})
	}
}

// The real recreate lets compose pull an image the daemon does not have. The
// flags that stop the differences capture from pulling or building stay there.
func TestUpNoDepsArgs(t *testing.T) {
	t.Serial()
	captureComposeLog(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(file, []byte("services: {}\n"), 0o644))
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsFile+"\"\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := execComposeRunner{}.UpNoDeps(context.Background(), composeTarget{
		ConfigFiles: []string{file},
		WorkingDir:  dir,
		Project:     "litellm",
		Service:     "bifrost",
	})
	require.NoError(t, err)

	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	assert.Equal(t, []string{"compose", "-f", file, "--project-directory", dir, "-p", "litellm", "up", "-d", "--no-deps", "bifrost"}, args)
	assert.NotContains(t, args, "--pull")
	assert.NotContains(t, args, "never")
	assert.NotContains(t, args, "--no-build")
}
