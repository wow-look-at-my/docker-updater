package main

import (
	"context"
	"encoding/json"
	"errors"
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
	runningConfigHash = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	fileConfigHash    = "9999cccc8888dddd7777eeee6666ffff5555aaaa4444bbbb3333cccc2222dddd"
	mirrorComposeFile = "/srv/mirror/compose.yml"
	mirrorComposeDir  = "/srv/mirror"
	mirrorApplyCmd    = "docker compose -f /srv/mirror/compose.yml --project-directory /srv/mirror -p mirror up -d --no-deps github-state-mirror"
)

// rollingComposeInfo is the incident's shape: a compose service updated by a
// rolling update, which clones the running container's config.
func rollingComposeInfo() ContainerInfo {
	return ContainerInfo{
		ID:                 "gsm-1",
		Name:               "github-state-mirror",
		Image:              "ghcr.io/wow-look-at-my/github-state-mirror:latest",
		Mode:               UpdateModeImage,
		Rolling:            true,
		ComposeProject:     "mirror",
		ComposeService:     "github-state-mirror",
		ComposeConfigFiles: mirrorComposeFile,
		ComposeWorkingDir:  mirrorComposeDir,
		Labels:             map[string]string{composeConfigHashLabel: runningConfigHash},
	}
}

func TestComposeDriftMatchingHashReportsNothing(t *testing.T) {
	t.Serial()
	runner := &fakeComposeRunner{configHash: runningConfigHash}
	var d *ComposeDrift
	logged := captureLog(t, func() {
		d = checkComposeDrift(context.Background(), runner, rollingComposeInfo(), Config{})
	})

	assert.Nil(t, d)
	assert.NotContains(t, logged, "ERROR")
	require.Len(t, runner.configHashTargets, 1)
	assert.Equal(t, composeTarget{
		ConfigFiles: []string{mirrorComposeFile},
		WorkingDir:  mirrorComposeDir,
		Project:     "mirror",
		Service:     "github-state-mirror",
	}, runner.configHashTargets[0], "the hash must come from the project and files the container names")
}

func TestComposeDriftDifferentHashReportsDrift(t *testing.T) {
	t.Serial()
	runner := &fakeComposeRunner{configHash: fileConfigHash}
	var d *ComposeDrift
	logged := captureLog(t, func() {
		d = checkComposeDrift(context.Background(), runner, rollingComposeInfo(), Config{})
	})

	require.NotNil(t, d)
	assert.Equal(t, driftFound, d.State)
	assert.Equal(t, "github-state-mirror", d.Service)
	assert.Equal(t, []string{mirrorComposeFile}, d.ConfigFiles)
	assert.Equal(t, runningConfigHash, d.RunningHash)
	assert.Equal(t, fileConfigHash, d.FileHash)
	assert.Equal(t, mirrorApplyCmd, d.Command)
	assert.Empty(t, d.Error)
	assert.Empty(t, d.Blocked)
	assert.True(t, d.applies())

	assert.Contains(t, logged, "container github-state-mirror: drifted from its compose file")
	assert.Contains(t, logged, "service github-state-mirror in "+mirrorComposeFile)
	assert.Contains(t, logged, "The updater recreates it from the file")
	assert.NotContains(t, logged, "ERROR", "a drift the updater applies is pending work, not a failure")

	assert.Zero(t, runner.upNoDepsCalls, "the check alone must not recreate anything")
}

// docker-updater cannot run compose against its own container, so its drift
// stays a report with the command for the operator.
func TestComposeDriftOnSelfIsBlocked(t *testing.T) {
	t.Serial()
	runner := &fakeComposeRunner{configHash: fileConfigHash}
	info := rollingComposeInfo()
	info.Rolling = false
	var d *ComposeDrift
	logged := captureLog(t, func() {
		d = checkComposeDrift(context.Background(), runner, info, Config{SelfContainerID: info.ID})
	})

	require.NotNil(t, d)
	assert.Equal(t, driftFound, d.State)
	assert.Equal(t, selfDriftBlocked, d.Blocked)
	assert.False(t, d.applies())
	assert.Contains(t, logged, "ERROR container github-state-mirror: drifted from its compose file")
	assert.Contains(t, logged, mirrorApplyCmd)
}

func TestComposeDriftHashErrorIsUnknownNotClean(t *testing.T) {
	t.Serial()
	runner := &fakeComposeRunner{configHashErr: errors.New("compose file /srv/mirror/compose.yml: permission denied")}
	var d *ComposeDrift
	logged := captureLog(t, func() {
		d = checkComposeDrift(context.Background(), runner, rollingComposeInfo(), Config{})
	})

	require.NotNil(t, d, "a failed comparison must never read as no drift")
	assert.Equal(t, driftUnknown, d.State)
	assert.Contains(t, d.Error, "permission denied")
	assert.Equal(t, mirrorApplyCmd, d.Command)
	assert.Contains(t, logged, "ERROR container github-state-mirror: compose drift unknown")
	assert.Contains(t, logged, "permission denied")
}

func TestComposeDriftMissingHashLabelIsUnknown(t *testing.T) {
	t.Serial()
	info := rollingComposeInfo()
	info.Labels = map[string]string{}
	runner := &fakeComposeRunner{configHash: fileConfigHash}
	var d *ComposeDrift
	captureLog(t, func() {
		d = checkComposeDrift(context.Background(), runner, info, Config{})
	})

	require.NotNil(t, d)
	assert.Equal(t, driftUnknown, d.State)
	assert.Contains(t, d.Error, composeConfigHashLabel)
}

// Every compose-managed container is compared, whatever its update path.
// Compose owns nothing else.
func TestComposeDriftScope(t *testing.T) {
	t.Serial()
	notCompose := rollingComposeInfo()
	notCompose.ComposeService, notCompose.ComposeConfigFiles = "", ""
	composeImage := rollingComposeInfo()
	composeImage.Rolling = false
	composeBuild := rollingComposeInfo()
	composeBuild.Mode = UpdateModeBuild
	composeGit := rollingComposeInfo()
	composeGit.Rolling, composeGit.Mode = false, UpdateModeGit
	self := rollingComposeInfo()
	self.Rolling = false

	cases := []struct {
		name    string
		info    ContainerInfo
		cfg     Config
		checked bool
	}{
		{"non-compose container", notCompose, Config{}, false},
		{"compose image mode", composeImage, Config{}, true},
		{"build mode", composeBuild, Config{}, true},
		{"rolling", rollingComposeInfo(), Config{}, true},
		{"git mode", composeGit, Config{}, true},
		{"self-update", self, Config{SelfContainerID: "gsm-1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeComposeRunner{configHash: fileConfigHash}
			var d *ComposeDrift
			captureLog(t, func() {
				d = checkComposeDrift(context.Background(), runner, tc.info, tc.cfg)
			})
			if tc.checked {
				assert.Len(t, runner.configHashTargets, 1)
				assert.NotNil(t, d)
				return
			}
			assert.Empty(t, runner.configHashTargets, "compose must not be asked about this container")
			assert.Nil(t, d)
		})
	}
}

// driftCycle runs one regular cycle over a rolling compose container whose
// file drifted and whose image is current.
func driftCycle(t *testing.T, cfg Config) ([]UpdateResult, *fakeComposeRunner) {
	t.Helper()
	runner := &fakeComposeRunner{configHash: fileConfigHash}
	saved := defaultComposeRunner
	defaultComposeRunner = runner
	t.Cleanup(func() { defaultComposeRunner = saved })

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:    "gsm-1",
				Names: []string{"/github-state-mirror"},
				Image: "ghcr.io/wow-look-at-my/github-state-mirror:latest",
				State: "running",
				Labels: map[string]string{
					"docker-updater.enable":                   "true",
					"docker-updater.rolling":                  "true",
					"docker-updater.well-known":               "false",
					"com.docker.compose.project":              "mirror",
					"com.docker.compose.service":              "github-state-mirror",
					"com.docker.compose.project.config_files": mirrorComposeFile,
					"com.docker.compose.project.working_dir":  mirrorComposeDir,
					composeConfigHashLabel:                    runningConfigHash,
				},
			}}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					Image: "sha256:current",
					State: &types.ContainerState{Running: true, Health: &types.Health{Status: "healthy"}},
				},
				Config: &container.Config{Image: "ghcr.io/wow-look-at-my/github-state-mirror:latest"},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:current", RepoDigests: []string{"ghcr.io/wow-look-at-my/github-state-mirror@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	cfg.Label = "docker-updater.enable"
	var results []UpdateResult
	captureLog(t, func() {
		results = runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))
	})
	require.Len(t, results, 1)
	return results, runner
}

// A drift with no image update behind it recreates the container from its
// file, as an update would. The rolling clone never runs: it would keep the drift.
func TestRunUpdateCheckRecreatesADriftedContainer(t *testing.T) {
	t.Serial()
	results, runner := driftCycle(t, Config{})

	r := results[0]
	require.NoError(t, r.Error)
	assert.True(t, r.Updated)
	assert.True(t, r.Resync)
	assert.Empty(t, r.NewRef, "no image update was found")
	assert.Nil(t, r.Drift, "the recreate applied the file")
	require.Equal(t, 1, runner.upNoDepsCalls, "the file applies through compose")
	assert.Equal(t, []string{"github-state-mirror"}, runner.upNoDepsServices)
	assert.Zero(t, runner.upCalls, "dependencies must not be recreated")

	store := newStore()
	store.Record(results, time.Now())
	st := store.Snapshot().Statuses["github-state-mirror"]
	assert.False(t, st.UpdateAvailable)
	assert.Nil(t, st.Drift)
	assert.False(t, st.LastUpdated.IsZero(), "the row shows the recreate like an applied update")
}

// Dry-run holds a drift recreate back like an update, and the row keeps the
// drift so the force button can apply it.
func TestRunUpdateCheckDryRunHoldsTheDrift(t *testing.T) {
	t.Serial()
	results, runner := driftCycle(t, Config{DryRun: true})

	r := results[0]
	assert.True(t, r.Updated)
	assert.True(t, r.DryRun)
	assert.True(t, r.Resync)
	require.NotNil(t, r.Drift)
	assert.Zero(t, runner.upNoDepsCalls, "dry-run must change nothing")

	store := newStore()
	store.Record(results, time.Now())
	st := store.Snapshot().Statuses["github-state-mirror"]
	assert.False(t, st.UpdateAvailable, "a held drift is not an image update")
	assert.True(t, st.Drift.applies())
}

// The pre-update gate holds a drift recreate exactly as it holds an update.
func TestApplyComposeDriftAsksThePreCheck(t *testing.T) {
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(gate.Close)
	info := composeServiceInfo()
	info.PreCheckURL = gate.URL
	info.PreCheckTimeout = time.Second
	runner := &fakeComposeRunner{}

	var r UpdateResult
	captureLog(t, func() {
		r = applyComposeDrift(context.Background(), composeDocker("healthy"), runner, info, Config{},
			UpdateResult{Container: info, Drift: &ComposeDrift{State: driftFound}})
	})

	assert.True(t, r.Skipped)
	assert.True(t, r.Resync)
	assert.NotEmpty(t, r.SkipReason)
	assert.Zero(t, runner.upNoDepsCalls)

	store := newStore()
	store.Record([]UpdateResult{r}, time.Now())
	st := store.Snapshot().Statuses[info.Name]
	assert.True(t, st.Skipped)
	assert.False(t, st.UpdateAvailable)
}

// Compose keeps no copy of the config, so an unhealthy recreate fails loudly
// and does not claim a rollback.
func TestApplyComposeDriftUnhealthyFailsWithoutRollbackClaim(t *testing.T) {
	info := composeServiceInfo()
	runner := &fakeComposeRunner{}

	var r UpdateResult
	captureLog(t, func() {
		r = applyComposeDrift(context.Background(), composeDocker("unhealthy"), runner, info, Config{},
			UpdateResult{Container: info, Drift: &ComposeDrift{State: driftFound}})
	})

	require.Error(t, r.Error)
	assert.Contains(t, r.Error.Error(), "not healthy after the recreate from its compose file")
	assert.Contains(t, r.Error.Error(), "compose cannot restore the old config")
	assert.NotContains(t, r.Error.Error(), "rolled back")
	assert.False(t, r.Updated)
	assert.NotNil(t, r.Drift, "the drift stays on the row while the recreate fails")
	assert.Equal(t, 1, runner.upNoDepsCalls)
}

func TestApplyComposeDriftLeavesOtherOutcomesAlone(t *testing.T) {
	drift := &ComposeDrift{State: driftFound}
	info := composeServiceInfo()
	cases := []struct {
		name      string
		in        UpdateResult
		wantDrift bool
	}{
		{"no drift", UpdateResult{Container: info}, false},
		{"unknown drift", UpdateResult{Container: info, Drift: &ComposeDrift{State: driftUnknown}}, true},
		{"blocked drift", UpdateResult{Container: info, Drift: &ComposeDrift{State: driftFound, Blocked: selfDriftBlocked}}, true},
		{"an applied update applied the file", UpdateResult{Container: info, Drift: drift, Updated: true, NewRef: "sha256:new"}, false},
		{"a held update applies the file when it goes", UpdateResult{Container: info, Drift: drift, NewRef: "sha256:new", Skipped: true}, true},
		{"a failed check", UpdateResult{Container: info, Drift: drift, Error: errors.New("pull failed")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeComposeRunner{}
			r := applyComposeDrift(context.Background(), composeDocker("healthy"), runner, info, Config{}, tc.in)
			assert.Zero(t, runner.upNoDepsCalls)
			assert.False(t, r.Resync)
			assert.Equal(t, tc.wantDrift, r.Drift != nil)
		})
	}
}

// An update of a drifted service goes through compose, so the file applies
// with the new image. A rolling clone would keep the drift.
func TestUpdateContainerTakesADriftedRollingServiceThroughCompose(t *testing.T) {
	cli := composeDocker("healthy")
	var creates []string
	recordCreates(cli, &creates)
	runner := &fakeComposeRunner{}
	info := composeServiceInfo()
	info.Rolling = true
	info.ComposeDrifted = true

	require.NoError(t, updateContainer(context.Background(), cli, runner, info, Config{}))

	assert.Empty(t, creates, "a drifted service must not be cloned from its old config")
	assert.Equal(t, 1, runner.upNoDepsCalls)
}

// The dashboard row carries the drift, so it is visible without the logs.
func TestHandleAPIContainersComposeDrift(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "gsm-1",
				Names:  []string{"/github-state-mirror"},
				State:  "running",
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
	}
	drift := &ComposeDrift{
		State:       driftFound,
		Service:     "github-state-mirror",
		ConfigFiles: []string{mirrorComposeFile},
		RunningHash: runningConfigHash,
		FileHash:    fileConfigHash,
		Command:     mirrorApplyCmd,
	}
	store := newStore()
	now := time.Now()
	store.Record([]UpdateResult{{Container: rollingComposeInfo(), CheckedAt: now, Drift: drift}}, now)

	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, store)
	rec := httptest.NewRecorder()
	s.handleAPIContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp apiResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Containers, 1)
	assert.Equal(t, drift, resp.Containers[0].ComposeDrift)
	assert.Contains(t, rec.Body.String(), `"compose_drift"`)

	// A later clean check clears the row.
	store.Record([]UpdateResult{{Container: rollingComposeInfo(), CheckedAt: now}}, now)
	assert.Nil(t, store.Snapshot().Statuses["github-state-mirror"].Drift)
}

func TestExecConfigHashReportsAnInvisibleFile(t *testing.T) {
	_, err := execComposeRunner{}.ConfigHash(context.Background(), composeTarget{
		ConfigFiles: []string{"/nonexistent/compose.yml"},
		Service:     "web",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not visible to docker-updater")
}

// fakeDockerOnPath puts a `docker` script first on PATH. It prints its
// arguments to args.txt and then runs body.
func fakeDockerOnPath(t *testing.T, body string) string {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" > " + dir + "/args.txt\n" + body + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestExecConfigHashRunsComposeConfigHash(t *testing.T) {
	file := filepath.Join(t.TempDir(), "compose.yml")
	require.NoError(t, os.WriteFile(file, []byte("services: {}\n"), 0o644))
	dir := fakeDockerOnPath(t, "echo 'web "+fileConfigHash+"'")

	got, err := execComposeRunner{}.ConfigHash(context.Background(), composeTarget{
		ConfigFiles: []string{file}, WorkingDir: "/srv/w", Project: "p", Service: "web",
	})

	require.NoError(t, err)
	assert.Equal(t, fileConfigHash, got)
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	require.NoError(t, err)
	assert.Equal(t, "compose -f "+file+" --project-directory /srv/w -p p config --hash web\n", string(args))
}

func TestExecConfigHashCarriesComposeStderr(t *testing.T) {
	file := filepath.Join(t.TempDir(), "compose.yml")
	require.NoError(t, os.WriteFile(file, []byte("services: {}\n"), 0o644))
	fakeDockerOnPath(t, "echo 'no such service: web' >&2; exit 1")

	_, err := execComposeRunner{}.ConfigHash(context.Background(), composeTarget{ConfigFiles: []string{file}, Service: "web"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such service: web")
}

func TestParseConfigHash(t *testing.T) {
	out := "db 12a5cec450c1\nweb b185e92cc84b\n"
	got, err := parseConfigHash(out, "web")
	require.NoError(t, err)
	assert.Equal(t, "b185e92cc84b", got)

	_, err = parseConfigHash(out, "cache")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no hash for service cache")
}

func TestShellJoinQuotesWhatAShellSplits(t *testing.T) {
	assert.Equal(t, "docker compose -f '/srv/my stack/compose.yml' -p 'it'\\''s'",
		"docker "+shellJoin([]string{"compose", "-f", "/srv/my stack/compose.yml", "-p", "it's"}))
	assert.Equal(t, "''", shellJoin([]string{""}))
}
