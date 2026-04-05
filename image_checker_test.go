package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestCheckImageUpdateNoChange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Already exists"}`))
	})
	mux.HandleFunc("/v1.45/images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(dockerImageInspect{ID: "sha256:samedigest"})
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info := ContainerInfo{
		Image:       "nginx:latest",
		ImageDigest: "sha256:samedigest",
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info)
	require.Nil(t, err)
	assert.Equal(t, "", newDigest)
}

func TestCheckImageUpdateChanged(t *testing.T) {
	mux := http.NewServeMux()
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
		Image:       "nginx:latest",
		ImageDigest: "sha256:olddigest",
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info)
	require.Nil(t, err)
	assert.Equal(t, "sha256:newdigest", newDigest)
}

func TestCheckImageUpdatePullError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.45/images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"pull failed"}`))
	})

	cli, cleanup := newTestDockerServer(t, mux)
	defer cleanup()

	info := ContainerInfo{
		Image: "broken:latest",
	}

	_, err := checkImageUpdate(context.Background(), cli, info)
	require.NotNil(t, err)
}
