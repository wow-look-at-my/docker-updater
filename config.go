// Config holds the service configuration parsed from environment variables.

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

	// StatsURL is the base URL of a simple-stats-api instance.
	StatsURL string

	// SelfContainerID is the ID of docker-updater's own container.
	SelfContainerID string

	// Inbound GitHub webhook.
	GitHubWebhookAddr   string
	GitHubWebhookSecret string

	// GitHubWebhookPackages optionally scopes which packages trigger a check.
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
	c.StatsURL = os.Getenv("DOCKER_UPDATER_STATS_URL")

	// Inbound GitHub webhook (opt-in: disabled unless an address is given).
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
