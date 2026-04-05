package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
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
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
}

func TestRunUpdateCheckImageUpToDate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:    "container1",
				Names: []string{"/web"},
				Image: "nginx:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/container1/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "container1",
			Image: "sha256:currentdigest",
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

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Updated {
		t.Error("expected not updated (image is current)")
	}
	if results[0].Error != nil {
		t.Errorf("unexpected error: %v", results[0].Error)
	}
}

func TestRunUpdateCheckImageDryRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{
				ID:    "container2",
				Names: []string{"/app"},
				Image: "myapp:latest",
				Labels: map[string]string{
					"docker-updater.enable": "true",
				},
			},
		})
	})
	mux.HandleFunc("/v1.45/containers/container2/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "container2",
			Image: "sha256:olddigest",
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

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Updated {
		t.Error("expected updated=true in dry-run")
	}
	if !results[0].DryRun {
		t.Error("expected dry_run=true")
	}
	if results[0].NewRef != "sha256:newdigest" {
		t.Errorf("expected new digest, got %q", results[0].NewRef)
	}
}

func TestCheckAndUpdateImageUpdate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/containers/oldcontainer/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerContainerInspect{
			ID:    "oldcontainer",
			Image: "sha256:olddigest",
			Name:  "/web",
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
			},
			HostConfig: json.RawMessage(`{}`),
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
		ID:          "oldcontainer",
		Name:        "web",
		Image:       "nginx:latest",
		ImageDigest: "sha256:olddigest",
		Mode:        UpdateModeImage,
	}

	cfg := Config{Label: "docker-updater.enable"}
	result := UpdateResult{Container: info}
	result = checkAndUpdateImage(context.Background(), cli, info, cfg, result)

	if !result.Updated {
		t.Error("expected updated=true")
	}
	if result.Error != nil {
		t.Errorf("unexpected error: %v", result.Error)
	}
}
