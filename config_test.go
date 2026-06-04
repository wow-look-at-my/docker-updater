package main

import (
	"os"
	"testing"
	"time"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Clear any env vars that might be set.
	for _, key := range []string{
		"DOCKER_UPDATER_INTERVAL",
		"DOCKER_UPDATER_LABEL",
		"DOCKER_UPDATER_WEBHOOK_URL",
		"DOCKER_UPDATER_WEBHOOK_TYPE",
		"DOCKER_UPDATER_DRY_RUN",
		"DOCKER_UPDATER_CONFIG",
		"DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR",
		"DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET",
		"DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	cfg, err := loadConfig()
	require.Nil(t, err)

	assert.Equal(t, 5*time.Minute, cfg.Interval)

	assert.Equal(t, "docker-updater.enable", cfg.Label)

	assert.Equal(t, "generic", cfg.WebhookType)

	assert.False(t, cfg.DryRun)

	assert.Equal(t, "", cfg.WebhookURL)

	assert.Equal(t, "", cfg.ConfigPath)

	assert.Equal(t, "", cfg.GitHubWebhookAddr)

	assert.Equal(t, "", cfg.GitHubWebhookSecret)

	assert.Equal(t, 0, len(cfg.GitHubWebhookPackages))

}

func TestLoadConfigCustom(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_INTERVAL", "10s")
	t.Setenv("DOCKER_UPDATER_LABEL", "custom.label")
	t.Setenv("DOCKER_UPDATER_WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "discord")
	t.Setenv("DOCKER_UPDATER_DRY_RUN", "true")

	cfg, err := loadConfig()
	require.Nil(t, err)

	assert.Equal(t, 10*time.Second, cfg.Interval)

	assert.Equal(t, "custom.label", cfg.Label)

	assert.Equal(t, "https://example.com/hook", cfg.WebhookURL)

	assert.Equal(t, "discord", cfg.WebhookType)

	assert.True(t, cfg.DryRun)

}

func TestLoadConfigInvalidInterval(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_INTERVAL", "notaduration")

	_, err := loadConfig()
	require.NotNil(t, err)

}

func TestLoadConfigInvalidWebhookType(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "teams")

	_, err := loadConfig()
	require.NotNil(t, err)

}

func TestLoadConfigInvalidDryRun(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_DRY_RUN", "notabool")

	_, err := loadConfig()
	require.NotNil(t, err)

}

func TestLoadConfigDockerConfig(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_CONFIG", "/path/to/config.json")

	cfg, err := loadConfig()
	require.Nil(t, err)

	assert.Equal(t, "/path/to/config.json", cfg.ConfigPath)

}

func TestLoadConfigSlackWebhookType(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_WEBHOOK_TYPE", "slack")

	cfg, err := loadConfig()
	require.Nil(t, err)

	assert.Equal(t, "slack", cfg.WebhookType)

}

func TestLoadConfigGitHubWebhook(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR", ":9000")
	t.Setenv("DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET", "s3cr3t")

	cfg, err := loadConfig()
	require.Nil(t, err)

	assert.Equal(t, ":9000", cfg.GitHubWebhookAddr)

	assert.Equal(t, "s3cr3t", cfg.GitHubWebhookSecret)

}

func TestLoadConfigGitHubWebhookRequiresSecret(t *testing.T) {
	t.Setenv("DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR", ":9000")
	t.Setenv("DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET", "")

	// Enabling a public webhook without a secret must be a hard error rather
	// than silently exposing an unauthenticated endpoint.
	_, err := loadConfig()
	require.NotNil(t, err)

}

func TestLoadConfigGitHubWebhookPackages(t *testing.T) {
	// Comma-separated, with surrounding whitespace and a stray empty entry.
	t.Setenv("DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES", " buildhost , wow-look-at-my/docker-updater ,")

	cfg, err := loadConfig()
	require.Nil(t, err)

	require.Equal(t, 2, len(cfg.GitHubWebhookPackages))

	assert.Equal(t, "buildhost", cfg.GitHubWebhookPackages[0])

	assert.Equal(t, "wow-look-at-my/docker-updater", cfg.GitHubWebhookPackages[1])

}
