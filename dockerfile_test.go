package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBaseImageSingleStage(t *testing.T) {
	df := "FROM ghcr.io/anomalyco/opencode:latest\nRUN echo hi\n"
	assert.Equal(t, "ghcr.io/anomalyco/opencode:latest", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageWithComments(t *testing.T) {
	df := `# syntax=docker/dockerfile:1
# this is a comment
FROM golang:1.25 AS builder
RUN go build

# final stage
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app /app
`
	// Final stage base is the distroless image, not the builder alias.
	assert.Equal(t, "gcr.io/distroless/static:nonroot", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageMultiStageWatchesFinal(t *testing.T) {
	df := `FROM node:22 AS frontend
RUN build-frontend

FROM golang:1.25 AS backend
RUN build-backend

FROM debian:bookworm-slim
COPY --from=backend /server /server
`
	assert.Equal(t, "debian:bookworm-slim", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageFinalStageReferencesEarlierStage(t *testing.T) {
	// A final `FROM builder` (an AS-name) must resolve back to builder's own
	// registry base, not the build-local alias.
	df := `FROM ghcr.io/org/base:1.2 AS builder
RUN make

FROM builder
CMD ["/run"]
`
	assert.Equal(t, "ghcr.io/org/base:1.2", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageTransitiveStageReference(t *testing.T) {
	df := `FROM ubuntu:24.04 AS a
FROM a AS b
FROM b
`
	assert.Equal(t, "ubuntu:24.04", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageIgnoresPlatformFlag(t *testing.T) {
	df := "FROM --platform=linux/amd64 alpine:3.20\n"
	assert.Equal(t, "alpine:3.20", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageLineContinuation(t *testing.T) {
	df := "FROM \\\n  ghcr.io/org/app:latest \\\n  AS final\n"
	assert.Equal(t, "ghcr.io/org/app:latest", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageEmpty(t *testing.T) {
	assert.Equal(t, "", parseBaseImageFromDockerfile(""))
	assert.Equal(t, "", parseBaseImageFromDockerfile("# only comments\n\n   \n"))
}

func TestParseBaseImageScratch(t *testing.T) {
	df := "FROM scratch\nCOPY app /app\n"
	assert.Equal(t, "scratch", parseBaseImageFromDockerfile(df))
}

func TestParseBaseImageCaseInsensitiveFrom(t *testing.T) {
	df := "from alpine:3.20 as base\n"
	assert.Equal(t, "alpine:3.20", parseBaseImageFromDockerfile(df))
}

func TestIsRegistryBase(t *testing.T) {
	assert.True(t, isRegistryBase("ghcr.io/anomalyco/opencode:latest"))
	assert.True(t, isRegistryBase("alpine:3.20"))
	assert.False(t, isRegistryBase("scratch"))
	assert.False(t, isRegistryBase("SCRATCH"))
	assert.False(t, isRegistryBase(""))
	assert.False(t, isRegistryBase("${BASE_IMAGE}"))
	assert.False(t, isRegistryBase("$BASE"))
}
