package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

// newTestDockerServer creates a test HTTP server listening on a Unix socket.
func newTestDockerServer(t *testing.T, handler http.Handler) (*dockerClient, func()) {
	t.Helper()

	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")

	listener, err := net.Listen("unix", sock)
	require.Nil(t, err)

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
			ServerVersion:	"27.5.1",
			Name:		"test-host",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info, err := cli.info(context.Background())
	require.Nil(t, err)

	assert.Equal(t, "27.5.1", info.ServerVersion)

	assert.Equal(t, "test-host", info.Name)

}

func TestDockerClientListContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:	"abc123",
				Names:	[]string{"/test-container"},
				Image:	"nginx:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
			{
				ID:	"def456",
				Names:	[]string{"/unmonitored"},
				Image:	"redis:latest",
				Labels:	map[string]string{},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/abc123/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:	"abc123",
			Image:	"sha256:imagedigest123",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)

	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, "test-container", c.Name)

	assert.Equal(t, "nginx:latest", c.Image)

	assert.Equal(t, UpdateModeImage, c.Mode)

	assert.Equal(t, "sha256:imagedigest123", c.ImageDigest)

}

func TestDockerClientListContainersGitMode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:	"git123",
				Names:	[]string{"/git-app"},
				Image:	"myapp:latest",
				Labels: map[string]string{
					"docker-updater.enable":	"true",
					"docker-updater.mode":		"git",
					"docker-updater.git-repo":	"https://github.com/example/repo",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/git123/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:	"git123",
			Image:	"sha256:digest",
		})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)

	require.Equal(t, 1, len(containers))

	c := containers[0]
	assert.Equal(t, UpdateModeGit, c.Mode)

	assert.Equal(t, "https://github.com/example/repo", c.GitRepo)

	assert.Equal(t, "refs/heads/main", c.GitRef)

}

func TestDockerClientPullImage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, r *http.Request) {
		fromImage := r.URL.Query().Get("fromImage")
		assert.Equal(t, "nginx:latest", fromImage)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Pull complete"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.pullImage(context.Background(), "nginx:latest")
	require.Nil(t, err)

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
	require.Nil(t, err)

	assert.Equal(t, "sha256:newdigest123", inspect.ID)

}

func TestDockerClientStopContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.stopContainer(context.Background(), "abc123", 30)
	require.Nil(t, err)

}

func TestDockerClientRemoveContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "DELETE", r.Method)

		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.removeContainer(context.Background(), "abc123")
	require.Nil(t, err)

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
	require.Nil(t, err)

	assert.Equal(t, "newcontainer123", id)

	err = cli.startContainer(context.Background(), "newcontainer123")
	require.Nil(t, err)

}

func TestDockerClientStopContainerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"internal error"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.stopContainer(context.Background(), "abc123", 30)
	require.NotNil(t, err)
}

func TestDockerClientRemoveContainerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"message":"conflict"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.removeContainer(context.Background(), "abc123")
	require.NotNil(t, err)
}

func TestDockerClientCreateContainerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"bad request"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	_, err := cli.createContainer(context.Background(), "test", []byte(`{}`))
	require.NotNil(t, err)
}

func TestDockerClientStartContainerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/abc123/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"start failed"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.startContainer(context.Background(), "abc123")
	require.NotNil(t, err)
}

func TestDockerClientPullImageError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"image not found"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	err := cli.pullImage(context.Background(), "nonexistent:latest")
	require.NotNil(t, err)
}

func TestDockerClientClose(t *testing.T) {
	cli := newDockerClient()
	cli.close() // should not panic
}

func TestListMonitoredContainersEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	containers, err := listMonitoredContainers(context.Background(), cli, "docker-updater.enable")
	require.Nil(t, err)
	assert.Equal(t, 0, len(containers))
}

func TestRecreateContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/old123/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "old123",
			Image: "sha256:olddigest",
			Name:  "/test-app",
			Config: struct {
				Image        string            `json:"Image"`
				Env          []string          `json:"Env"`
				Cmd          []string          `json:"Cmd"`
				Entrypoint   []string          `json:"Entrypoint"`
				WorkingDir   string            `json:"WorkingDir"`
				Labels       map[string]string `json:"Labels"`
				ExposedPorts map[string]any    `json:"ExposedPorts"`
				Volumes      map[string]any    `json:"Volumes"`
				User         string            `json:"User"`
			}{
				Image: "nginx:latest",
				Env:   []string{"FOO=bar"},
			},
			HostConfig: json.RawMessage(`{"RestartPolicy":{"Name":"always"}}`),
		})
	})
	mux.HandleFunc("/v1.45/containers/old123/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.45/containers/old123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/v1.45/containers/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(dockerCreateResponse{ID: "new456789012"})
	})
	mux.HandleFunc("/v1.45/containers/new456789012/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info := ContainerInfo{
		ID:    "old123",
		Name:  "test-app",
		Image: "nginx:latest",
	}

	err := recreateContainer(context.Background(), cli, info, "nginx:latest")
	require.Nil(t, err)
}
