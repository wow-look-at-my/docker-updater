package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// newTestDockerServer creates a test HTTP server listening on a Unix socket.
func newTestDockerServer(t *testing.T, handler http.Handler) (*dockerClient, func()) {
	t.Helper()

	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("failed to create test socket: %v", err)
	}

	server := &http.Server{Handler: handler}
	go server.Serve(listener)

	cli := &dockerClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sock)
				},
			},
		},
	}

	cleanup := func() {
		server.Close()
		listener.Close()
		os.RemoveAll(dir)
	}

	return cli, cleanup
}

func TestDockerClientInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/info", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerInfo{
			ServerVersion: "27.5.1",
			Name:          "test-host",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info, err := cli.info(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ServerVersion != "27.5.1" {
		t.Errorf("expected version 27.5.1, got %q", info.ServerVersion)
	}
	if info.Name != "test-host" {
		t.Errorf("expected name test-host, got %q", info.Name)
	}
}

func TestDockerClientListContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:    "abc123",
				Names: []string{"/test-container"},
				Image: "nginx:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
			{
				ID:     "def456",
				Names:  []string{"/unmonitored"},
				Image:  "redis:latest",
				Labels: map[string]string{},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/abc123/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "abc123",
			Image: "sha256:imagedigest123",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(containers) != 1 {
		t.Fatalf("expected 1 monitored container, got %d", len(containers))
	}

	c := containers[0]
	if c.Name != "test-container" {
		t.Errorf("expected name test-container, got %q", c.Name)
	}
	if c.Image != "nginx:latest" {
		t.Errorf("expected image nginx:latest, got %q", c.Image)
	}
	if c.Mode != UpdateModeImage {
		t.Errorf("expected image mode, got %q", c.Mode)
	}
	if c.ImageDigest != "sha256:imagedigest123" {
		t.Errorf("expected digest, got %q", c.ImageDigest)
	}
}

func TestDockerClientListContainersGitMode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:    "git123",
				Names: []string{"/git-app"},
				Image: "myapp:latest",
				Labels: map[string]string{
					"docker-updater.enable":   "true",
					"docker-updater.mode":     "git",
					"docker-updater.git-repo": "https://github.com/example/repo",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/git123/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "git123",
			Image: "sha256:digest",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	c := containers[0]
	if c.Mode != UpdateModeGit {
		t.Errorf("expected git mode, got %q", c.Mode)
	}
	if c.GitRepo != "https://github.com/example/repo" {
		t.Errorf("expected git repo URL, got %q", c.GitRepo)
	}
	if c.GitRef != "refs/heads/main" {
		t.Errorf("expected default git ref, got %q", c.GitRef)
	}
}

func TestDockerClientPullImage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, r *http.Request) {
		fromImage := r.URL.Query().Get("fromImage")
		if fromImage != "nginx:latest" {
			t.Errorf("expected fromImage=nginx:latest, got %q", fromImage)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Pull complete"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.pullImage(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerClientInspectImage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerImageInspect{
			ID: "sha256:newdigest123",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	inspect, err := cli.inspectImage(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inspect.ID != "sha256:newdigest123" {
		t.Errorf("expected digest, got %q", inspect.ID)
	}
}

func TestDockerClientStopContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.stopContainer(context.Background(), "abc123", 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerClientRemoveContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.removeContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerClientCreateAndStartContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(dockerCreateResponse{ID: "newcontainer123"})
	})
	mux.HandleFunc("/v1.45/containers/newcontainer123/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	payload, _ := json.Marshal(map[string]string{"Image": "nginx:latest"})
	id, err := cli.createContainer(context.Background(), "test", payload)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}
	if id != "newcontainer123" {
		t.Errorf("expected newcontainer123, got %q", id)
	}

	err = cli.startContainer(context.Background(), "newcontainer123")
	if err != nil {
		t.Fatalf("unexpected error starting: %v", err)
	}
}
