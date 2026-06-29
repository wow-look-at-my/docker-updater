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

	// Build-mode fields. Populated for UpdateModeBuild from the container's
	// compose labels and (optionally) the docker-updater.base-image label.
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

	// Rolling update: start new container before stopping old.
	Rolling bool

	// Post-update health check fields.
	// If HealthCheckURL is set, HTTP GET is polled until 2xx.
	// Otherwise if HealthCheckCommand is set, it is exec'd via sh -c.
	// Falls back to Docker HEALTHCHECK status when neither is set.
	HealthCheckURL     string
	HealthCheckCommand string
	HealthCheckTimeout time.Duration
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
}
