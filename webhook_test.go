package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildGenericPayload(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "web", Image: "nginx:latest"},
			Updated:   true,
			OldRef:    "sha256:olddigestvalue1234567890",
			NewRef:    "sha256:newdigestvalue1234567890",
			CheckedAt: time.Now(),
		},
	}

	payload, err := buildGenericPayload(results)
	require.Nil(t, err)

	var p genericPayload
	require.NoError(t, json.Unmarshal(payload, &p))

	require.Equal(t, 1, len(p.Updates))

	assert.Equal(t, "web", p.Updates[0].Container)

	assert.True(t, p.Updates[0].Updated)

	assert.NotEqual(t, "", p.Timestamp)

}

func TestBuildGenericPayloadWithError(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "db", Image: "postgres:16"},
			Error:     errors.New("pull failed"),
			CheckedAt: time.Now(),
		},
	}

	payload, err := buildGenericPayload(results)
	require.Nil(t, err)

	var p genericPayload
	require.NoError(t, json.Unmarshal(payload, &p))

	assert.Equal(t, "pull failed", p.Updates[0].Error)

}

func TestBuildDiscordPayload(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "app", Image: "myapp:latest"},
			Updated:   true,
			OldRef:    "sha256:old1234567890123",
			NewRef:    "sha256:new1234567890123",
		},
	}

	payload, err := buildDiscordPayload(results)
	require.Nil(t, err)

	var p map[string]any
	require.NoError(t, json.Unmarshal(payload, &p))

	embeds, ok := p["embeds"].([]any)
	require.False(t, !ok || len(embeds) != 1)

	embed := embeds[0].(map[string]any)
	assert.Equal(t, "Docker Updater", embed["title"])

	fields := embed["fields"].([]any)
	require.Equal(t, 1, len(fields))

}

func TestBuildSlackPayload(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "worker", Image: "worker:v2"},
			Updated:   true,
			OldRef:    "sha256:old1234567890123",
			NewRef:    "sha256:new1234567890123",
		},
	}

	payload, err := buildSlackPayload(results)
	require.Nil(t, err)

	var p map[string]any
	require.NoError(t, json.Unmarshal(payload, &p))

	blocks, ok := p["blocks"].([]any)
	require.True(t, ok)

	// Header + 1 section
	require.Equal(t, 2, len(blocks))

	header := blocks[0].(map[string]any)
	assert.Equal(t, "header", header["type"])

}

func TestBuildDiscordPayloadWithError(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "broken", Image: "broken:latest"},
			Error:     errors.New("connection refused"),
		},
	}

	payload, err := buildDiscordPayload(results)
	require.Nil(t, err)

	var p map[string]any
	require.NoError(t, json.Unmarshal(payload, &p))

	embeds := p["embeds"].([]any)
	embed := embeds[0].(map[string]any)
	fields := embed["fields"].([]any)
	field := fields[0].(map[string]any)
	value := field["value"].(string)

	assert.Equal(t, "error: connection refused", value)

}

func TestBuildSlackPayloadDryRun(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "test", Image: "test:latest"},
			Updated:   true,
			OldRef:    "sha256:old1234567890123",
			NewRef:    "sha256:new1234567890123",
			DryRun:    true,
		},
	}

	payload, err := buildSlackPayload(results)
	require.Nil(t, err)

	var p map[string]any
	require.NoError(t, json.Unmarshal(payload, &p))

	blocks := p["blocks"].([]any)
	section := blocks[1].(map[string]any)
	text := section["text"].(map[string]any)
	content := text["text"].(string)

	assert.NotEqual(t, "", content)

}

func TestSendWebhookNotificationsNoURL(t *testing.T) {
	cfg := Config{}
	results := []UpdateResult{
		{Updated: true},
	}
	// Should not panic when no webhook URL is configured.
	sendWebhookNotifications(cfg, results)
}

func TestSendWebhookNotificationsNoUpdates(t *testing.T) {
	cfg := Config{WebhookURL: "https://example.com/hook"}
	// No notable results — should return early without sending.
	sendWebhookNotifications(cfg, nil)
	sendWebhookNotifications(cfg, []UpdateResult{
		{Updated: false},
	})
}

func TestSendWebhookNotificationsGeneric(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		WebhookURL:  server.URL,
		WebhookType: "generic",
	}
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "web", Image: "nginx:latest"},
			Updated:   true,
			OldRef:    "sha256:old1234567890123",
			NewRef:    "sha256:new1234567890123",
		},
	}

	sendWebhookNotifications(cfg, results)

	require.NotEqual(t, 0, len(received))
	var p genericPayload
	require.NoError(t, json.Unmarshal(received, &p))
	assert.Equal(t, 1, len(p.Updates))
}

func TestSendWebhookNotificationsDiscord(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		WebhookURL:  server.URL,
		WebhookType: "discord",
	}
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "app", Image: "app:latest"},
			Updated:   true,
		},
	}

	sendWebhookNotifications(cfg, results)

	require.NotEqual(t, 0, len(received))
	var p map[string]any
	require.NoError(t, json.Unmarshal(received, &p))
	assert.NotNil(t, p["embeds"])
}

func TestSendWebhookNotificationsSlack(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		WebhookURL:  server.URL,
		WebhookType: "slack",
	}
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "worker", Image: "worker:v2"},
			Error:     errors.New("test error"),
		},
	}

	sendWebhookNotifications(cfg, results)

	require.NotEqual(t, 0, len(received))
	var p map[string]any
	require.NoError(t, json.Unmarshal(received, &p))
	assert.NotNil(t, p["blocks"])
}

func TestSendWebhookNotificationsServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := Config{
		WebhookURL:  server.URL,
		WebhookType: "generic",
	}
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "test", Image: "test:latest"},
			Updated:   true,
		},
	}

	// Should not panic on server error.
	sendWebhookNotifications(cfg, results)
}
