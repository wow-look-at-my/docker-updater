package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for pullImage: digest/fetched reporting, auth resolution, and pull
// progress-stream decoding (split from docker_test.go for file length).

func TestPullImage(t *testing.T) {
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:newdigest123"}, nil, nil
		},
	}

	noAuth := newAuthResolver(nil)
	digest, _, err := pullImage(context.Background(), cli, "nginx:latest", noAuth)
	require.Nil(t, err)
	assert.Equal(t, "sha256:newdigest123", digest)
}

func TestPullImageFetchedReportsNewContent(t *testing.T) {
	// The reference resolves to one image before the pull and a different one
	// after: the pull fetched new content, so fetched must be true.
	var calls int
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			calls++
			if calls == 1 {
				return types.ImageInspect{ID: "sha256:oldlocal"}, nil, nil
			}
			return types.ImageInspect{ID: "sha256:newlocal"}, nil, nil
		},
	}

	digest, fetched, err := pullImage(context.Background(), cli, "nginx:latest", newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "sha256:newlocal", digest)
	assert.True(t, fetched, "content ID changed across the pull")
}

func TestPullImageFetchedFalseWhenUpToDate(t *testing.T) {
	// The reference resolves to the same image before and after: the pull found
	// the local image already current and downloaded nothing, so fetched is false.
	cli := &mockDocker{
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:unchanged"}, nil, nil
		},
	}

	digest, fetched, err := pullImage(context.Background(), cli, "nginx:latest", newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "sha256:unchanged", digest)
	assert.False(t, fetched, "up-to-date pull must not report fetched content")
}

func TestPullImageError(t *testing.T) {
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("pull failed")
		},
	}

	noAuth := newAuthResolver(nil)
	_, _, err := pullImage(context.Background(), cli, "broken:latest", noAuth)
	require.NotNil(t, err)
}

func TestPullImageSurfacesMidStreamError(t *testing.T) {
	// The daemon reports mid-pull failures (registry 429/5xx, dropped
	// connections) as in-band error records in a cleanly terminated stream.
	// They must fail the pull -- a blind drain would silently no-op, and the
	// caller would compare against the stale local image and log
	// "up-to-date" forever.
	inspects := 0
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			stream := `{"status":"Pulling from org/image"}` + "\n" +
				`{"errorDetail":{"message":"received unexpected HTTP status: 500 Internal Server Error"},"error":"received unexpected HTTP status: 500 Internal Server Error"}` + "\n"
			return io.NopCloser(strings.NewReader(stream)), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			inspects++
			return types.ImageInspect{ID: "sha256:stale-local"}, nil, nil
		},
	}

	_, _, err := pullImage(context.Background(), cli, "ghcr.io/org/image:latest", newAuthResolver(nil))
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "500 Internal Server Error")
	assert.Equal(t, 1, inspects, "a failed pull must not inspect the stale local image as its result")
}

func TestPullImageDecodesProgressStream(t *testing.T) {
	// A realistic multi-record progress stream (statuses, layer progress,
	// digest line) decodes cleanly and does not fail the pull.
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			stream := `{"status":"Pulling from org/image","id":"latest"}` + "\n" +
				`{"status":"Downloading","progressDetail":{"current":512,"total":1024},"id":"e1744b3"}` + "\n" +
				`{"status":"Pull complete","id":"e1744b3"}` + "\n" +
				`{"status":"Digest: sha256:8e1744b3"}` + "\n" +
				`{"status":"Status: Downloaded newer image for org/image:latest"}` + "\n"
			return io.NopCloser(strings.NewReader(stream)), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:fresh"}, nil, nil
		},
	}

	digest, _, err := pullImage(context.Background(), cli, "org/image:latest", newAuthResolver(nil))
	require.Nil(t, err)
	assert.Equal(t, "sha256:fresh", digest)
}

func TestPullImageWithAuth(t *testing.T) {
	var capturedAuth string
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, opts image.PullOptions) (io.ReadCloser, error) {
			capturedAuth = opts.RegistryAuth
			return io.NopCloser(strings.NewReader(`{"status":"Pull complete"}`)), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:authdigest"}, nil, nil
		},
	}

	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"ghcr.io": {Auth: "dXNlcjp0b2tlbg=="},
		},
	}
	resolver := newAuthResolver(cfg)

	digest, _, err := pullImage(context.Background(), cli, "ghcr.io/org/image:latest", resolver)
	require.Nil(t, err)
	assert.Equal(t, "sha256:authdigest", digest)
	assert.NotEmpty(t, capturedAuth)
}

func TestPullImageAnonymousFallback(t *testing.T) {
	var capturedAuth string
	cli := &mockDocker{
		imagePullFn: func(_ context.Context, _ string, opts image.PullOptions) (io.ReadCloser, error) {
			capturedAuth = opts.RegistryAuth
			return io.NopCloser(strings.NewReader(`{"status":"Pull complete"}`)), nil
		},
		imageInspectFn: func(_ context.Context, _ string) (types.ImageInspect, []byte, error) {
			return types.ImageInspect{ID: "sha256:anondigest"}, nil, nil
		},
	}

	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"ghcr.io": {Auth: "dXNlcjp0b2tlbg=="},
		},
	}
	resolver := newAuthResolver(cfg)

	digest, _, err := pullImage(context.Background(), cli, "nginx:latest", resolver)
	require.Nil(t, err)
	assert.Equal(t, "sha256:anondigest", digest)
	assert.Empty(t, capturedAuth)
}
