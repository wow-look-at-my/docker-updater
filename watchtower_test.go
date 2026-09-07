package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdaterOf(t *testing.T) {
	const ours = "docker-updater.enable"

	assert.Empty(t, updaterOf(map[string]string{}, ours), "no label means no updater")
	assert.Equal(t, updaterDockerUpdater, updaterOf(map[string]string{ours: "true"}, ours))
	assert.Equal(t, updaterWatchtower, updaterOf(map[string]string{watchtowerEnableLabel: "true"}, ours))
	assert.Equal(t, updaterWatchtower, updaterOf(map[string]string{watchtowerScopeLabel: "prod"}, ours),
		"a scope pins the container to a watchtower instance, whatever its value")
	assert.Equal(t, updaterBoth, updaterOf(map[string]string{ours: "true", watchtowerEnableLabel: "true"}, ours))

	// enable=false is watchtower's opt-OUT under its default all-containers
	// mode. Reading it as ownership would claim every excluded container.
	assert.Empty(t, updaterOf(map[string]string{watchtowerEnableLabel: "false"}, ours))
	assert.Equal(t, updaterDockerUpdater, updaterOf(map[string]string{ours: "true", watchtowerEnableLabel: "false"}, ours))
}

func TestWatchtowerOwns(t *testing.T) {
	assert.True(t, watchtowerOwns(updaterWatchtower))
	assert.True(t, watchtowerOwns(updaterBoth), "a shared claim is still watchtower's")
	assert.False(t, watchtowerOwns(updaterDockerUpdater))
	assert.False(t, watchtowerOwns(""))
}

// The API is where the dashboard reads this from, so the field has to survive
// the whole handler, and a container claimed twice has to carry the warning
// that names the fix.
func TestAPIContainersReportsTheUpdater(t *testing.T) {
	t.Serial()
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{ID: "a1", Names: []string{"/ours"}, State: "running", Labels: map[string]string{"docker-updater.enable": "true"}},
				{ID: "b2", Names: []string{"/theirs"}, State: "running", Labels: map[string]string{watchtowerEnableLabel: "true"}},
				{ID: "c3", Names: []string{"/nobodys"}, State: "running", Labels: map[string]string{}},
				{ID: "d4", Names: []string{"/contested"}, State: "running", Labels: map[string]string{
					"docker-updater.enable": "true", watchtowerScopeLabel: "prod",
				}},
			}, nil
		},
	}

	srv := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, newStore())
	rec := httptest.NewRecorder()
	srv.handleAPIContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp apiResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byName := map[string]apiContainer{}
	for _, c := range resp.Containers {
		byName[c.Name] = c
	}
	assert.Equal(t, updaterDockerUpdater, byName["ours"].Updater)
	assert.Equal(t, updaterWatchtower, byName["theirs"].Updater)
	assert.Empty(t, byName["nobodys"].Updater)
	assert.Equal(t, updaterBoth, byName["contested"].Updater)
	assert.Contains(t, byName["contested"].Warnings, bothUpdatersWarning)
	assert.Empty(t, byName["ours"].Warnings, "one claim is not a conflict")

	// A container claimed twice leads, then ours, then watchtower's, then the
	// one nothing updates.
	order := make([]string, 0, len(resp.Containers))
	for _, c := range resp.Containers {
		order = append(order, c.Name)
	}
	assert.Equal(t, []string{"contested", "ours", "theirs", "nobodys"}, order)
}
