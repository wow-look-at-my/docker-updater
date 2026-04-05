package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p genericPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(p.Updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(p.Updates))
	}
	if p.Updates[0].Container != "web" {
		t.Errorf("expected container web, got %q", p.Updates[0].Container)
	}
	if !p.Updates[0].Updated {
		t.Error("expected updated=true")
	}
	if p.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p genericPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if p.Updates[0].Error != "pull failed" {
		t.Errorf("expected error message, got %q", p.Updates[0].Error)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	embeds, ok := p["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatal("expected 1 embed")
	}

	embed := embeds[0].(map[string]any)
	if embed["title"] != "Docker Updater" {
		t.Errorf("expected title Docker Updater, got %v", embed["title"])
	}

	fields := embed["fields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(fields))
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	blocks, ok := p["blocks"].([]any)
	if !ok {
		t.Fatal("expected blocks array")
	}

	// Header + 1 section
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	header := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("expected header block, got %v", header["type"])
	}
}

func TestBuildDiscordPayloadWithError(t *testing.T) {
	results := []UpdateResult{
		{
			Container: ContainerInfo{Name: "broken", Image: "broken:latest"},
			Error:     errors.New("connection refused"),
		},
	}

	payload, err := buildDiscordPayload(results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	embeds := p["embeds"].([]any)
	embed := embeds[0].(map[string]any)
	fields := embed["fields"].([]any)
	field := fields[0].(map[string]any)
	value := field["value"].(string)

	if value != "error: connection refused" {
		t.Errorf("expected error message in field, got %q", value)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	blocks := p["blocks"].([]any)
	section := blocks[1].(map[string]any)
	text := section["text"].(map[string]any)
	content := text["text"].(string)

	if content == "" {
		t.Error("expected non-empty text")
	}
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
