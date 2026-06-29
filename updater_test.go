package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunUpdateCheckNoContainers(t *testing.T) {
	cli := &mockDocker{}

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))
	assert.Equal(t, 0, len(results))
}

func TestRunUpdateCheckListError(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return nil, errors.New("connection refused")
		},
	}

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))
	assert.Nil(t, results)
}

func TestRunUpdateCheckImageUpToDate(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "container1",
					Names: []string{"/web"},
					Image: "nginx:latest",
					Labels: map[string]string{
						"docker-updater.enable": "true",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:currentdigest"},
				Config:            &container.Config{Image: "nginx:latest"},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			// Registry image: same manifest before and after the pull (up-to-date).
			return types.ImageInspect{ID: "sha256:currentdigest", RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}}, nil, nil
		},
	}

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))

	require.Equal(t, 1, len(results))
	assert.False(t, results[0].Updated)
	assert.Nil(t, results[0].Error)
	assert.False(t, results[0].Pulled, "an up-to-date check pulls nothing and must not reset the last-pulled time")
}

func TestCheckAndUpdateImagePulledWhenFetched(t *testing.T) {
	// When the pull fetches genuinely new content -- the tag resolves to a
	// different local image after pulling -- result.Pulled must be true. Dry-run
	// keeps the test on the pull/fetch path without the recreate handoff.
	var inspects int
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			inspects++
			if inspects == 1 { // before the pull: the old local image
				return types.ImageInspect{ID: "sha256:olddigest"}, nil, nil
			}
			return types.ImageInspect{ID: "sha256:newdigest"}, nil, nil // after the pull
		},
	}

	info := ContainerInfo{
		Name:        "web",
		Image:       "nginx:latest",
		ImageDigest: "sha256:olddigest",
		Mode:        UpdateModeImage,
	}
	cfg := Config{Label: "docker-updater.enable", DryRun: true}
	result := checkAndUpdateImage(context.Background(), cli, info, cfg, UpdateResult{Container: info, DryRun: true}, newAuthResolver(nil))

	assert.True(t, result.Pulled, "fetching new content must mark the image as pulled")
	assert.True(t, result.Updated, "an update is available")
	assert.Equal(t, "sha256:newdigest", result.NewRef)
}

func TestRunUpdateCheckImageDryRun(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "container2",
					Names: []string{"/app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable": "true",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:olddigest"},
				Config:            &container.Config{Image: "myapp:latest"},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, ref string) (types.ImageInspect, []byte, error) {
			if strings.HasPrefix(ref, "sha256:") {
				// Running image inspect during listing: old manifest.
				return types.ImageInspect{ID: "sha256:olddigest", RepoDigests: []string{"myapp@sha256:" + strings.Repeat("a", 64)}}, nil, nil
			}
			// Freshly pulled image for the tag: the tag has advanced.
			return types.ImageInspect{ID: "sha256:newdigest", RepoDigests: []string{"myapp@sha256:" + strings.Repeat("b", 64)}}, nil, nil
		},
	}

	cfg := Config{Label: "docker-updater.enable", DryRun: true}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))

	require.Equal(t, 1, len(results))
	assert.True(t, results[0].Updated)
	assert.True(t, results[0].DryRun)
	assert.Equal(t, "sha256:"+strings.Repeat("b", 64), results[0].NewRef)
}

func TestCheckAndUpdateImageUpdate(t *testing.T) {
	inspectCount := 0
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "nginx:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "healthy"},
					},
				},
			}, nil
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest"}, nil, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "newcontainer1"}, nil
		},
	}

	info := ContainerInfo{
		ID:          "oldcontainer1",
		Name:        "web",
		Image:       "nginx:latest",
		ImageDigest: "sha256:olddigest",
		Mode:        UpdateModeImage,
	}

	cfg := Config{Label: "docker-updater.enable"}
	result := UpdateResult{Container: info}
	result = checkAndUpdateImage(context.Background(), cli, info, cfg, result, newAuthResolver(nil))

	assert.True(t, result.Updated)
	assert.Nil(t, result.Error)
}

func TestCheckAndUpdateGitFirstRun(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	gitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("001e# service=git-upload-pack\n"))
		w.Write([]byte("0000\n"))
		w.Write([]byte("003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main\n"))
		w.Write([]byte("0000\n"))
	}))
	defer gitServer.Close()

	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "gitcontainer1",
					Names: []string{"/git-app"},
					Image: "myapp:latest",
					Labels: map[string]string{
						"docker-updater.enable":   "true",
						"docker-updater.mode":     "git",
						"docker-updater.git-repo": gitServer.URL,
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
			}, nil
		},
	}

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))

	require.Equal(t, 1, len(results))
	assert.False(t, results[0].Updated)
}

func TestCheckAndUpdateGitNoRepo(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	info := ContainerInfo{
		ID:   "no-repo-container",
		Name: "no-repo",
		Mode: UpdateModeGit,
	}

	cfg := Config{}
	result := UpdateResult{Container: info}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)

	require.NotNil(t, result.Error)
	assert.False(t, result.Updated)
}

func TestCheckAndUpdateGitDryRun(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	callCount := 0
	gitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		sha := "ab3def1234567890ab3def1234567890ab3def12"
		if callCount > 1 {
			sha = "ff3def1234567890ff3def1234567890ff3def12"
		}
		w.Write([]byte("001e# service=git-upload-pack\n"))
		w.Write([]byte("0000\n"))
		w.Write([]byte("003f" + sha + " refs/heads/main\n"))
		w.Write([]byte("0000\n"))
	}))
	defer gitServer.Close()

	info := ContainerInfo{
		ID:      "git-dry-container",
		Name:    "git-dry",
		Mode:    UpdateModeGit,
		GitRepo: gitServer.URL,
		GitRef:  "refs/heads/main",
	}

	cfg := Config{DryRun: true}

	result := UpdateResult{Container: info, DryRun: true}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.False(t, result.Updated)

	result = UpdateResult{Container: info, DryRun: true}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.True(t, result.Updated)
	assert.True(t, result.DryRun)
}

func TestRunUpdateCheckUnknownMode(t *testing.T) {
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			return []types.Container{
				{
					ID:    "unknown1",
					Names: []string{"/unknown-mode"},
					Image: "test:latest",
					Labels: map[string]string{
						"docker-updater.enable": "true",
						"docker-updater.mode":   "unknown",
					},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{Image: "sha256:digest"},
			}, nil
		},
	}

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg, newAuthResolver(nil))
	assert.Equal(t, 0, len(results))
}

func TestCheckAndUpdateImagePreCheckFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest"}, nil, nil
		},
	}

	info := ContainerInfo{
		ID:              "precheck-container",
		Name:            "precheck-app",
		Image:           "myapp:latest",
		ImageDigest:     "sha256:olddigest",
		Mode:            UpdateModeImage,
		PreCheckURL:     server.URL + "/ready",
		PreCheckTimeout: 5 * time.Second,
	}

	cfg := Config{Label: "docker-updater.enable"}
	result := UpdateResult{Container: info}
	result = checkAndUpdateImage(context.Background(), cli, info, cfg, result, newAuthResolver(nil))

	assert.False(t, result.Updated)
	assert.True(t, result.Skipped)
	assert.Contains(t, result.SkipReason, "status 503")
}

func TestCheckAndUpdateImagePreCheckPasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	inspectCount := 0
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest"}, nil, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			inspectCount++
			if inspectCount == 1 {
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						Image:      "sha256:olddigest",
						HostConfig: &container.HostConfig{},
					},
					Config:          &container.Config{Image: "myapp:latest"},
					NetworkSettings: &types.NetworkSettings{},
				}, nil
			}
			return types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					State: &types.ContainerState{
						Running: true,
						Health:  &types.Health{Status: "healthy"},
					},
				},
			}, nil
		},
		containerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "new-container"}, nil
		},
	}

	info := ContainerInfo{
		ID:              "precheck-container",
		Name:            "precheck-app",
		Image:           "myapp:latest",
		ImageDigest:     "sha256:olddigest",
		Mode:            UpdateModeImage,
		PreCheckURL:     server.URL + "/ready",
		PreCheckTimeout: 5 * time.Second,
	}

	cfg := Config{Label: "docker-updater.enable"}
	result := UpdateResult{Container: info}
	result = checkAndUpdateImage(context.Background(), cli, info, cfg, result, newAuthResolver(nil))

	assert.True(t, result.Updated)
	assert.False(t, result.Skipped)
}

func TestCheckAndUpdateGitPreCheckFails(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	callCount := 0
	gitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		sha := "ab3def1234567890ab3def1234567890ab3def12"
		if callCount > 1 {
			sha = "ff3def1234567890ff3def1234567890ff3def12"
		}
		w.Write([]byte("001e# service=git-upload-pack\n"))
		w.Write([]byte("0000\n"))
		w.Write([]byte("003f" + sha + " refs/heads/main\n"))
		w.Write([]byte("0000\n"))
	}))
	defer gitServer.Close()

	preCheckServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer preCheckServer.Close()

	info := ContainerInfo{
		ID:              "git-precheck",
		Name:            "git-precheck-app",
		Mode:            UpdateModeGit,
		GitRepo:         gitServer.URL,
		GitRef:          "refs/heads/main",
		PreCheckURL:     preCheckServer.URL + "/ready",
		PreCheckTimeout: 5 * time.Second,
	}

	cfg := Config{}

	// First call seeds the ref store.
	result := UpdateResult{Container: info}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.False(t, result.Updated)
	assert.False(t, result.Skipped)

	// Second call detects change but pre-check fails.
	result = UpdateResult{Container: info}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.False(t, result.Updated)
	assert.True(t, result.Skipped)
	assert.Contains(t, result.SkipReason, "status 503")
}

func TestRunLoop(t *testing.T) {
	cli := &mockDocker{}

	cfg := Config{
		Label:    "docker-updater.enable",
		Interval: 100 * time.Millisecond,
	}

	sigCh := make(chan os.Signal, 1)

	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), cli, cfg, sigCh, newAuthResolver(nil), newStore(), nil)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after signal")
	}
}

// waitFor polls cond until it is true or the deadline elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestRunLoopWebhookTrigger proves a webhook trigger runs a check immediately,
// without waiting for the (here, deliberately huge) interval to elapse.
func TestRunLoopWebhookTrigger(t *testing.T) {
	var checks atomic.Int64
	cli := &mockDocker{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
			checks.Add(1)
			return nil, nil // no monitored containers; a check still ran
		},
	}

	cfg := Config{
		Label:    "docker-updater.enable",
		Interval: time.Hour, // long enough that the ticker never fires in this test
	}

	sigCh := make(chan os.Signal, 1)
	trigger := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), cli, cfg, sigCh, newAuthResolver(nil), newStore(), trigger)
		close(done)
	}()

	// The initial check runs once on startup.
	waitFor(t, func() bool { return checks.Load() >= 1 }, "initial check did not run")

	// Firing the trigger must run a second check despite the hour-long interval.
	trigger <- struct{}{}
	waitFor(t, func() bool { return checks.Load() >= 2 }, "webhook trigger did not run a check")

	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after signal")
	}
}
