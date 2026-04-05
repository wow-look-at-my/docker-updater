package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Clear any env vars that might be set.
	for _, key := range []string{
		"DOCKER_UPDATER_INTERVAL",
		"DOCKER_UPDATER_LABEL",
		"DOCKER_UPDATER_WEBHOOK_URL",
		"DOCKER_UPDATER_WEBHOOK_TYPE",
		"DOCKER_UPDATER_DRY_RUN",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Interval != 5*time.Minute {
		t.Errorf("expected interval 5m, got %v", cfg.Interval)
	}
	if cfg.Label != "docker-updater.enable" {
		t.Errorf("expected label docker-updater.enable, got %q", cfg.Label)
	}
	if cfg.WebhookType != "generic" {
		t.Errorf("expected webhook type generic, got %q", cfg.WebhookType)
	}
	if cfg.DryRun {
		t.Error("expected dry run false")
	}
	if cfg.WebhookURL != "" {
		t.Errorf("expected empty webhook URL, got %q", cfg.WebhookURL)
	}
}

func TestLoadConfigCustom(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_INTERVAL", "10s")
	t.Setenv("DOCKER_UPDATER_LABEL", "custom.label")
	t.Setenv("DOCKER_UPDATER_WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "discord")
	t.Setenv("DOCKER_UPDATER_DRY_RUN", "true")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Interval != 10*time.Second {
		t.Errorf("expected interval 10s, got %v", cfg.Interval)
	}
	if cfg.Label != "custom.label" {
		t.Errorf("expected label custom.label, got %q", cfg.Label)
	}
	if cfg.WebhookURL != "https://example.com/hook" {
		t.Errorf("expected webhook URL, got %q", cfg.WebhookURL)
	}
	if cfg.WebhookType != "discord" {
		t.Errorf("expected discord, got %q", cfg.WebhookType)
	}
	if !cfg.DryRun {
		t.Error("expected dry run true")
	}
}

func TestLoadConfigInvalidInterval(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_INTERVAL", "notaduration")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid interval")
	}
}

func TestLoadConfigInvalidWebhookType(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "teams")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid webhook type")
	}
}

func TestLoadConfigInvalidDryRun(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_DRY_RUN", "notabool")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid dry run")
	}
}

func TestLoadConfigSlackWebhookType(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "slack")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WebhookType != "slack" {
		t.Errorf("expected slack, got %q", cfg.WebhookType)
	}
}
