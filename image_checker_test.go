package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestCheckImageUpdateNoChange(t *testing.T) {
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:samedigest"}, nil, nil
		},
	}

	info := ContainerInfo{
		Image:       "nginx:latest",
		ImageDigest: "sha256:samedigest",
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "", newDigest)
}

func TestCheckImageUpdateChanged(t *testing.T) {
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest"}, nil, nil
		},
	}

	info := ContainerInfo{
		Image:       "nginx:latest",
		ImageDigest: "sha256:olddigest",
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "sha256:newdigest", newDigest)
}

func TestCheckImageUpdateUntaggedUsesRepoDigests(t *testing.T) {
	// The running image is untagged; the comparison is by registry manifest
	// digest (RepoDigests) and detects the advanced tag.
	repo := "ghcr.io/wow-look-at-my/buildhost"
	oldManifest := "sha256:" + strings.Repeat("a", 64)
	newManifest := "sha256:" + strings.Repeat("b", 64)

	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{
				ID:          "sha256:" + strings.Repeat("c", 64),
				RepoDigests: []string{repo + "@" + newManifest},
			}, nil, nil
		},
	}

	info := ContainerInfo{
		Image:       repo + ":latest",
		ImageDigest: oldManifest, // running manifest digest, no tag required
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, newManifest, newDigest)
}

func TestCheckImageUpdateUntaggedNoChange(t *testing.T) {
	repo := "ghcr.io/wow-look-at-my/buildhost"
	manifest := "sha256:" + strings.Repeat("a", 64)

	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{
				ID:          "sha256:" + strings.Repeat("c", 64),
				RepoDigests: []string{repo + "@" + manifest},
			}, nil, nil
		},
	}

	info := ContainerInfo{
		Image:       repo + ":latest",
		ImageDigest: manifest,
	}

	newDigest, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "", newDigest)
}

func TestCheckImageUpdatePullError(t *testing.T) {
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("pull failed")
		},
	}

	info := ContainerInfo{Image: "broken:latest"}

	_, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.NotNil(t, err)
}

func TestCheckImageUpdateInspectError(t *testing.T) {
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{}, nil, errors.New("inspect failed")
		},
	}

	info := ContainerInfo{Image: "broken:latest"}

	_, err := checkImageUpdate(context.Background(), cli, info, newAuthResolver(nil))
	require.NotNil(t, err)
}
