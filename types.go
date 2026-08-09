package main

import "time"

// UpdateMode determines how a container is checked for updates.
type UpdateMode string

const (
	UpdateModeImage UpdateMode = "image"
	UpdateModeGit   UpdateMode = "git"
	// UpdateModeBuild watches the base image of a locally-built (compose
	// build:) service. The local derived tag is never pulled; instead the base
	// image is pulled and, when its digest changes, the service is rebuilt and
	// recreated via docker compose.
	UpdateModeBuild UpdateMode = "build"
)

// ContainerInfo holds metadata about a monitored container.
type ContainerInfo struct {
	ID          string
	Name        string
	Image       string
	ImageDigest string
	Mode        UpdateMode
	Labels      map[string]string

	// Git-mode fields
	GitRepo string
	GitRef  string

	// Compose fields. Populated for every mode from the container's compose
	// labels; empty for containers not created by docker compose. Image mode
	// recreates a compose-managed service through `docker compose up -d` so
	// compose-file edits apply, and build mode additionally rebuilds it.
	ComposeProject     string // com.docker.compose.project
	ComposeService     string // com.docker.compose.service
	ComposeConfigFiles string // com.docker.compose.project.config_files (comma-separated)
	ComposeWorkingDir  string // com.docker.compose.project.working_dir
	// BaseImage is the registry image whose digest changing triggers a
	// rebuild. Taken from the docker-updater.base-image label if set, else
	// parsed from the service's Dockerfile FROM. Empty means unresolved
	// (the container is skipped).
	BaseImage string

	// Pre-update check fields.
	// If PreCheckURL is set, an HTTP GET is sent and 2xx means ready.
	// Otherwise if PreCheckCommand is set, it is exec'd via sh -c.
	PreCheckURL     string
	PreCheckCommand string
	PreCheckTimeout time.Duration
	// PreCheckStandard marks PreCheckURL as the discovered standard endpoint
	// rather than an operator-configured one. The standard pre-update endpoint
	// is optional, so a 404 (or an unreachable container) means "no opinion,
	// go ahead" instead of holding the update back.
	PreCheckStandard bool

	// Discovery inputs for the standard /.well-known/docker-updater/
	// endpoints: where to reach the container, and which TCP ports it declares.
	Address      string // container IP, or 127.0.0.1 under host networking
	ExposedPorts []int  // declared TCP ports, ascending
	// DockerHealthcheck reports whether the container has an effective Docker
	// HEALTHCHECK -- the same signal waitHealthy branches on (State.Health).
	// It is what makes a discovery warning able to name the fallback the
	// container will ACTUALLY get instead of the one it might have had.
	DockerHealthcheck bool

	// Rolling update: start new container before stopping old.
	Rolling bool

	// Post-update health check fields.
	// If HealthCheckURL is set, HTTP GET is polled until 2xx.
	// Otherwise if HealthCheckCommand is set, it is exec'd via sh -c.
	// Falls back to Docker HEALTHCHECK status when neither is set.
	HealthCheckURL     string
	HealthCheckCommand string
	HealthCheckTimeout time.Duration
	// HealthCheckURLFromContainer marks HealthCheckURL as built from the
	// container's own address rather than written by an operator. Such a URL
	// names the container being replaced, so the gate re-resolves its host
	// against the container the update just started. An operator-written
	// absolute URL names something else on purpose and is polled as written.
	HealthCheckURLFromContainer bool
}

// UpdateResult records the outcome of an update check/action.
type UpdateResult struct {
	Container  ContainerInfo
	Updated    bool
	OldRef     string // old digest or commit SHA
	NewRef     string // new digest or commit SHA
	Error      error
	CheckedAt  time.Time
	DryRun     bool
	Skipped    bool
	SkipReason string
	Pulled     bool // a registry pull was performed during this check
	// Warnings are operator-actionable notes about how this container is
	// configured (no standard endpoints, nonstandard label overrides). They
	// describe configuration, not the outcome of this cycle.
	Warnings []string
}
