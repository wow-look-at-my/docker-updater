package main

import (
	"fmt"
	"os"
	"strconv"
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

	// An unset variable keeps the default; an explicitly empty value disables
	// the dashboard server.
	if v, ok := os.LookupEnv("DOCKER_UPDATER_DASHBOARD_ADDR"); ok {
		c.DashboardAddr = v
	}

	return c, nil
}
