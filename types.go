package main

import "time"

// UpdateMode determines how a container is checked for updates.
type UpdateMode string

const (
	UpdateModeImage UpdateMode = "image"
	UpdateModeGit   UpdateMode = "git"
)

// PreCheckType determines how a container is checked before updating.
type PreCheckType string

const (
	PreCheckNone PreCheckType = ""
	PreCheckHTTP PreCheckType = "http"
	PreCheckExec PreCheckType = "exec"
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

	// Pre-update check fields
	PreCheck        PreCheckType
	PreCheckURL     string
	PreCheckCommand string
	PreCheckTimeout time.Duration
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
}
