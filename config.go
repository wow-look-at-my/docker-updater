package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the service configuration parsed from environment variables.
type Config struct {
	Interval      time.Duration
	Label         string
	WebhookURL    string
	WebhookType   string
	DryRun        bool
	ConfigPath    string
	DashboardAddr string

	// SelfContainerID is the ID of docker-updater's own container. When set,
	// an update that targets this container is performed via a detached helper
	// (see self_update.go) instead of an inline recreate, which would kill the
	// updater mid-swap. Auto-detected at startup; DOCKER_UPDATER_CONTAINER_ID
	// overrides detection for environments where it fails.
	SelfContainerID string

	// Inbound GitHub webhook. When GitHubWebhookAddr is set, the updater
	// listens on it for authenticated GitHub "package" deliveries (a ghcr image
	// being published/updated) and runs a check immediately instead of waiting
	// out the rest of the interval. The endpoint is meant to be exposed
	// publicly, so GitHubWebhookSecret is mandatory whenever it is enabled.
	GitHubWebhookAddr   string
	GitHubWebhookSecret string

	// GitHubWebhookPackages optionally scopes which packages trigger a check.
	// This matters for an org-level webhook, which fires for every package in
	// the org: when non-empty, only deliveries whose package name (or its
	// "namespace/name" form) is listed trigger a check; everything else is
	// acknowledged but ignored. Empty means any package event triggers.
	GitHubWebhookPackages []string
}

func loadConfig() (Config, error) {
	c := Config{
		Interval:      5 * time.Minute,
		Label:         "docker-updater.enable",
		WebhookType:   "generic",
		DashboardAddr: ":8080",
	}

	if v := os.Getenv("DOCKER_UPDATER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("invalid DOCKER_UPDATER_INTERVAL %q: %w", v, err)
		}
		c.Interval = d
	}

	if v := os.Getenv("DOCKER_UPDATER_LABEL"); v != "" {
		c.Label = v
	}

	c.WebhookURL = os.Getenv("DOCKER_UPDATER_WEBHOOK_URL")

	if v := os.Getenv("DOCKER_UPDATER_WEBHOOK_TYPE"); v != "" {
		switch v {
		case "generic", "discord", "slack":
			c.WebhookType = v
		default:
			return c, fmt.Errorf("invalid DOCKER_UPDATER_WEBHOOK_TYPE %q: must be generic, discord, or slack", v)
		}
	}

	if v := os.Getenv("DOCKER_UPDATER_DRY_RUN"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("invalid DOCKER_UPDATER_DRY_RUN %q: %w", v, err)
		}
		c.DryRun = b
	}

	c.ConfigPath = os.Getenv("DOCKER_UPDATER_CONFIG")

	// Optional override for own-container detection (see SelfContainerID).
	c.SelfContainerID = os.Getenv("DOCKER_UPDATER_CONTAINER_ID")

	// An unset variable keeps the default; an explicitly empty value disables
	// the dashboard server.
	if v, ok := os.LookupEnv("DOCKER_UPDATER_DASHBOARD_ADDR"); ok {
		c.DashboardAddr = v
	}

	// Inbound GitHub webhook (opt-in: disabled unless an address is given).
	// Because this listener is intended to be reachable from the public
	// internet, refuse to start it without a secret -- an unauthenticated
	// trigger endpoint would let anyone force update checks.
	c.GitHubWebhookAddr = os.Getenv("DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR")
	c.GitHubWebhookSecret = os.Getenv("DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET")
	if c.GitHubWebhookAddr != "" && c.GitHubWebhookSecret == "" {
		return c, fmt.Errorf("DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR is set but DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET is empty: refusing to expose an unauthenticated webhook")
	}
	for _, p := range strings.Split(os.Getenv("DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.GitHubWebhookPackages = append(c.GitHubWebhookPackages, p)
		}
	}

	return c, nil
}
