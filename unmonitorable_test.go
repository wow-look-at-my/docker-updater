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

// A container the updater can never check must say so in the payload. Without
// this field the row carries the enable label, no error and no available
// update, which the Upstream column reads as "up to date".
func TestHandleAPIContainersUnmonitorable(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{{
				ID:     "c-s3",
				Names:  []string{"/s3"},
				Image:  "sha256:4a721d2e7ba5",
				State:  "running",
				Status: "Up 16 hours",
				Labels: map[string]string{"docker-updater.enable": "true"},
			}}, nil
		},
	}

	store := newStore()
	now := time.Now()
	store.Record([]UpdateResult{{
		Container: ContainerInfo{
			Name:          "s3",
			Image:         "sha256:4a721d2e7ba5",
			Mode:          UpdateModeImage,
			Unmonitorable: "no registry repository to poll",
		},
		CheckedAt: now,
	}}, now)

	s := newDashboardServer(cli, Config{Interval: time.Minute, Label: "docker-updater.enable"}, store)
	rec := httptest.NewRecorder()
	s.handleAPIContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp apiResponse
	require.Nil(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Containers, 1)
	assert.Equal(t, "no registry repository to poll", resp.Containers[0].Unmonitorable)
	assert.Empty(t, resp.Containers[0].Error)
}
