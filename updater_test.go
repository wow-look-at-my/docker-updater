package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestRunUpdateCheckNoContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg)
	assert.Equal(t, 0, len(results))

}

func TestRunUpdateCheckImageUpToDate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:	"container1",
				Names:	[]string{"/web"},
				Image:	"nginx:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/container1/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:	"container1",
			Image:	"sha256:currentdigest",
		})
	})
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Already exists"}`))
	})
	mux.HandleFunc("/v1.45/images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerImageInspect{ID: "sha256:currentdigest"})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg)

	require.Equal(t, 1, len(results))

	assert.False(t, results[0].Updated)

	assert.Nil(t, results[0].Error)

}

func TestRunUpdateCheckImageDryRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:	"container2",
				Names:	[]string{"/app"},
				Image:	"myapp:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/container2/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:	"container2",
			Image:	"sha256:olddigest",
		})
	})
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Pull complete"}`))
	})
	mux.HandleFunc("/v1.45/images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerImageInspect{ID: "sha256:newdigest"})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	cfg := Config{Label: "docker-updater.enable", DryRun: true}
	results := runUpdateCheck(context.Background(), cli, cfg)

	require.Equal(t, 1, len(results))

	assert.True(t, results[0].Updated)

	assert.True(t, results[0].DryRun)

	assert.Equal(t, "sha256:newdigest", results[0].NewRef)

}

func TestCheckAndUpdateImageUpdate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/oldcontainer/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:	"oldcontainer",
			Image:	"sha256:olddigest",
			Name:	"/web",
			Config: struct {
				Image		string			`json:"Image"`
				Env		[]string		`json:"Env"`
				Cmd		[]string		`json:"Cmd"`
				Entrypoint	[]string		`json:"Entrypoint"`
				WorkingDir	string			`json:"WorkingDir"`
				Labels		map[string]string	`json:"Labels"`
				ExposedPorts	map[string]any		`json:"ExposedPorts"`
				Volumes		map[string]any		`json:"Volumes"`
				User		string			`json:"User"`
			}{
				Image: "nginx:latest",
			},
			HostConfig:	json.RawMessage(`{}`),
		})
	})
	mux.HandleFunc("/v1.45/containers/oldcontainer/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.45/containers/oldcontainer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/v1.45/containers/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(dockerCreateResponse{ID: "newcontainer456"})
	})
	mux.HandleFunc("/v1.45/containers/newcontainer456/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Pull complete"}`))
	})
	mux.HandleFunc("/v1.45/images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerImageInspect{ID: "sha256:newdigest"})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info := ContainerInfo{
		ID:		"oldcontainer",
		Name:		"web",
		Image:		"nginx:latest",
		ImageDigest:	"sha256:olddigest",
		Mode:		UpdateModeImage,
	}

	cfg := Config{Label: "docker-updater.enable"}
	result := UpdateResult{Container: info}
	result = checkAndUpdateImage(context.Background(), cli, info, cfg, result)

	assert.True(t, result.Updated)

	assert.Nil(t, result.Error)

}

func TestCheckAndUpdateGitFirstRun(t *testing.T) {
	// Reset git ref store.
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	// Set up a test git server.
	gitMux := http.NewServeMux()
	gitMux.HandleFunc("/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("001e# service=git-upload-pack\n"))
		w.Write([]byte("0000\n"))
		w.Write([]byte("003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main\n"))
		w.Write([]byte("0000\n"))
	})
	gitServer := httptest.NewServer(gitMux)
	defer gitServer.Close()

	dockerMux := http.NewServeMux()
	dockerMux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
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
		})
	})
	dockerMux.HandleFunc("/v1.45/containers/gitcontainer1/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "gitcontainer1",
			Image: "sha256:digest",
		})
	})

	cli, cleanup := newTestDockerServer(t, dockerMux)
	defer cleanup()

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg)

	require.Equal(t, 1, len(results))
	// First run should not trigger update (no previous ref to compare).
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
		// GitRepo intentionally empty.
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

	// First call sets baseline.
	result := UpdateResult{Container: info, DryRun: true}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.False(t, result.Updated)

	// Second call detects change.
	result = UpdateResult{Container: info, DryRun: true}
	result = checkAndUpdateGit(context.Background(), nil, info, cfg, result)
	assert.True(t, result.Updated)
	assert.True(t, result.DryRun)
}

func TestRunUpdateCheckUnknownMode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:    "unknown1",
				Names: []string{"/unknown-mode"},
				Image: "test:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
					"docker-updater.mode":   "unknown",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/unknown1/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{ID: "unknown1", Image: "sha256:digest"})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	cfg := Config{Label: "docker-updater.enable"}
	results := runUpdateCheck(context.Background(), cli, cfg)
	// Unknown mode containers are skipped, not added to results.
	assert.Equal(t, 0, len(results))
}

func TestRunLoop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	cfg := Config{
		Label:    "docker-updater.enable",
		Interval: 100 * time.Millisecond,
	}

	sigCh := make(chan os.Signal, 1)

	// Run the loop in a goroutine and signal it to stop.
	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), cli, cfg, sigCh)
		close(done)
	}()

	// Let it run one tick, then signal.
	time.Sleep(150 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case <-done:
		// OK - loop exited.
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit after signal")
	}
}
