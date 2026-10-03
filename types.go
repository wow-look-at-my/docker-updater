package main

import "time"

// UpdateMode determines how a container is checked for updates.
type UpdateMode string

const (
	UpdateModeImage UpdateMode = "image"
	UpdateModeGit   UpdateMode = "git"
	// UpdateModeBuild watches the base image of a locally-built (compose build:) service.
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
	// State is Docker's container state ("running", "restarting", "exited", ...).
	State string

	// Git-mode fields
	GitRepo string
	GitRef  string

	// Compose fields.
	ComposeProject     string // com.docker.compose.project
	ComposeService     string // com.docker.compose.service
	ComposeConfigFiles string // com.docker.compose.project.config_files (comma-separated)
	ComposeWorkingDir  string // com.docker.compose.project.working_dir
	// BaseImage is the registry image whose digest changing triggers a rebuild.
	BaseImage string

	// Pre-update check fields. If PreCheckURL is set, an HTTP GET is sent and 2xx means ready.
	PreCheckURL     string
	PreCheckCommand string
	PreCheckTimeout time.Duration
	// PreCheckStandard marks PreCheckURL as the discovered standard endpoint rather than an operator-configured one.
	PreCheckStandard bool

	// Discovery inputs for the standard /.well-known/docker-updater/ endpoints: where to reach the container.
	Address      string // the container's own IP; empty when it has none
	ExposedPorts []int  // declared TCP ports, ascending
	// AddressNetwork is the ID of the network Address belongs to.
	AddressNetwork string
	// DockerHealthcheck reports whether the container has an effective Docker HEALTHCHECK.
	DockerHealthcheck bool

	// Unmonitorable, when set, is why this container can never be checked as it stands: no registry repository to poll.
	Unmonitorable string

	// Rolling update: start new container before stopping old.
	Rolling bool

	// Force is set when an operator asked for this update from the dashboard.
	Force bool

	// ComposeDrifted is set when the compose file no longer matches this container and the updater applies it.
	ComposeDrifted bool

	// Post-update health check fields. If HealthCheckURL is set, HTTP GET is polled until 2xx.
	HealthCheckURL     string
	HealthCheckCommand string
	HealthCheckTimeout time.Duration
	// HealthCheckURLFromContainer marks HealthCheckURL as built from the container's own address rather than written.
	HealthCheckURLFromContainer bool
}

// UpdateResult records the outcome of an update check/action.
type UpdateResult struct {
	Container ContainerInfo
	Updated   bool
	OldRef    string // old digest or commit SHA
	NewRef    string // new digest or commit SHA
	// The commit the new image was built from, and its page, when its labels name one. Image mode only.
	NewCommit    string
	NewCommitURL string
	Error        error
	CheckedAt    time.Time
	DryRun       bool
	Skipped      bool
	SkipReason   string
	Pulled       bool // a registry pull was performed during this check
	// Warnings are operator-actionable notes about how this container is configured.
	Warnings []string
	// Drift is set when a compose-managed container no longer matches its compose file, or when the comparison failed.
	Drift *ComposeDrift
	// Resync is set when the action this check took or held back is a recreate from the compose file, not an update.
	Resync bool
}
