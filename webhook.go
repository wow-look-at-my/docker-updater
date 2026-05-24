package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// sendWebhookNotifications sends update results to the configured webhook.
func sendWebhookNotifications(cfg Config, results []UpdateResult) {
	if cfg.WebhookURL == "" {
		return
	}

	var notable []UpdateResult
	for _, r := range results {
		if r.Updated || r.Error != nil || r.Skipped {
			notable = append(notable, r)
		}
	}

	if len(notable) == 0 {
		return
	}

	var payload []byte
	var err error

	switch cfg.WebhookType {
	case "discord":
		payload, err = buildDiscordPayload(notable)
	case "slack":
		payload, err = buildSlackPayload(notable)
	default:
		payload, err = buildGenericPayload(notable)
	}

	if err != nil {
		log.Printf("error building webhook payload: %v", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("error sending webhook: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("webhook returned status %d", resp.StatusCode)
	}
}

type genericPayload struct {
	Timestamp string              `json:"timestamp"`
	Updates   []genericUpdateItem `json:"updates"`
}

type genericUpdateItem struct {
	Container  string `json:"container"`
	Image      string `json:"image"`
	Updated    bool   `json:"updated"`
	OldRef     string `json:"old_ref,omitempty"`
	NewRef     string `json:"new_ref,omitempty"`
	Error      string `json:"error,omitempty"`
	DryRun     bool   `json:"dry_run"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
}

func buildGenericPayload(results []UpdateResult) ([]byte, error) {
	p := genericPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	for _, r := range results {
		item := genericUpdateItem{
			Container:  r.Container.Name,
			Image:      r.Container.Image,
			Updated:    r.Updated,
			OldRef:     shortID(r.OldRef),
			NewRef:     shortID(r.NewRef),
			DryRun:     r.DryRun,
			Skipped:    r.Skipped,
			SkipReason: r.SkipReason,
		}
		if r.Error != nil {
			item.Error = r.Error.Error()
		}
		p.Updates = append(p.Updates, item)
	}

	return json.Marshal(p)
}

func buildDiscordPayload(results []UpdateResult) ([]byte, error) {
	var fields []map[string]any
	for _, r := range results {
		value := "up-to-date"
		if r.Error != nil {
			value = fmt.Sprintf("error: %v", r.Error)
		} else if r.Skipped {
			value = fmt.Sprintf("skipped: %s", r.SkipReason)
		} else if r.Updated {
			value = fmt.Sprintf("%s -> %s", shortID(r.OldRef), shortID(r.NewRef))
			if r.DryRun {
				value += " (dry-run)"
			}
		}

		fields = append(fields, map[string]any{
			"name":   r.Container.Name,
			"value":  value,
			"inline": true,
		})
	}

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":       "Docker Updater",
				"description": fmt.Sprintf("Checked %d container(s)", len(results)),
				"color":       3447003, // blue
				"fields":      fields,
				"timestamp":   time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	return json.Marshal(payload)
}

func buildSlackPayload(results []UpdateResult) ([]byte, error) {
	var blocks []map[string]any

	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": "Docker Updater",
		},
	})

	for _, r := range results {
		status := ":white_check_mark: up-to-date"
		if r.Error != nil {
			status = fmt.Sprintf(":x: error: %v", r.Error)
		} else if r.Skipped {
			status = fmt.Sprintf(":warning: skipped: %s", r.SkipReason)
		} else if r.Updated {
			status = fmt.Sprintf(":arrows_counterclockwise: %s -> %s", shortID(r.OldRef), shortID(r.NewRef))
			if r.DryRun {
				status += " (dry-run)"
			}
		}

		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*%s* (%s)\n%s", r.Container.Name, r.Container.Image, status),
			},
		})
	}

	payload := map[string]any{"blocks": blocks}
	return json.Marshal(payload)
}
