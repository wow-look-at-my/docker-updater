package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

// sign produces a GitHub-style "sha256=<hex>" HMAC-SHA256 signature.
func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestValidSignatureGitHubVector checks our HMAC against the exact known-answer
// vector published in GitHub's "validating webhook deliveries" docs, proving the
// scheme is implemented compatibly with what GitHub actually sends.
func TestValidSignatureGitHubVector(t *testing.T) {
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"

	assert.Equal(t, want, sign(secret, body))
	assert.True(t, validSignature(secret, body, want))
}

func TestValidSignatureRejects(t *testing.T) {
	secret := []byte("topsecret")
	body := []byte(`{"action":"published"}`)
	good := sign(secret, body)

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
	}{
		{"wrong secret", []byte("nope"), body, good},
		{"tampered body", secret, []byte(`{"action":"deleted"}`), good},
		{"missing prefix", secret, body, strings.TrimPrefix(good, "sha256=")},
		{"wrong prefix", secret, body, "sha1=" + strings.TrimPrefix(good, "sha256=")},
		{"not hex", secret, body, "sha256=zzzz"},
		{"empty header", secret, body, ""},
		{"empty secret", nil, body, good},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, validSignature(tt.secret, tt.body, tt.header))
		})
	}
}

// newTestWebhook builds a server with a depth-1 trigger channel for inspection.
func newTestWebhook(secret string, packages ...string) (*githubWebhookServer, chan struct{}) {
	trigger := make(chan struct{}, 1)
	return newGitHubWebhookServer(":0", secret, packages, trigger), trigger
}

func postWebhook(s *githubWebhookServer, event string, secret, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)
	return rec
}

func TestWebhookPackageEventTriggers(t *testing.T) {
	secret := []byte("shhh")
	s, trigger := newTestWebhook(string(secret))
	body := []byte(`{"action":"published","package":{"name":"docker-updater","package_type":"CONTAINER"}}`)

	rec := postWebhook(s, "package", secret, body)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, 1, len(trigger), "package event must fire the trigger")
}

func TestWebhookRegistryPackageEventTriggers(t *testing.T) {
	secret := []byte("shhh")
	s, trigger := newTestWebhook(string(secret))
	body := []byte(`{"action":"updated","registry_package":{"name":"x"}}`)

	rec := postWebhook(s, "registry_package", secret, body)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 1, len(trigger))
}

func TestWebhookPingDoesNotTrigger(t *testing.T) {
	secret := []byte("shhh")
	s, trigger := newTestWebhook(string(secret))
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":123}`)

	rec := postWebhook(s, "ping", secret, body)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, len(trigger), "ping must not fire the trigger")
}

func TestWebhookUnrelatedEventDoesNotTrigger(t *testing.T) {
	secret := []byte("shhh")
	s, trigger := newTestWebhook(string(secret))
	body := []byte(`{"ref":"refs/heads/main"}`)

	rec := postWebhook(s, "push", secret, body)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, len(trigger))
}

func TestWebhookInvalidSignatureRejected(t *testing.T) {
	s, trigger := newTestWebhook("shhh")
	body := []byte(`{"action":"published","package":{"name":"x"}}`)

	// Signed with the wrong secret.
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "package")
	req.Header.Set("X-Hub-Signature-256", sign([]byte("attacker"), body))
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, len(trigger), "an unauthenticated request must never fire the trigger")
}

func TestWebhookMissingSignatureRejected(t *testing.T) {
	s, trigger := newTestWebhook("shhh")
	body := []byte(`{"action":"published"}`)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "package")
	// No X-Hub-Signature-256 header at all.
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, len(trigger))
}

func TestWebhookWrongMethodRejected(t *testing.T) {
	s, trigger := newTestWebhook("shhh")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	assert.Equal(t, 0, len(trigger))
}

func TestWebhookBodyTooLargeRejected(t *testing.T) {
	s, trigger := newTestWebhook("shhh")
	body := bytes.Repeat([]byte("a"), maxWebhookBody+1)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "package")
	req.Header.Set("X-Hub-Signature-256", sign([]byte("shhh"), body))
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, 0, len(trigger))
}

// TestWebhookFireCoalesces proves a burst of deliveries can never block the
// handler or queue more than one pending check.
func TestWebhookFireCoalesces(t *testing.T) {
	s, trigger := newTestWebhook("shhh")

	for i := 0; i < 5; i++ {
		s.fire() // must not block even though the channel fills after the first
	}

	assert.Equal(t, 1, len(trigger))
}

func TestParsePackage(t *testing.T) {
	t.Run("modern package event", func(t *testing.T) {
		body := []byte(`{"action":"published","package":{"name":"buildhost","namespace":"wow-look-at-my","package_type":"CONTAINER"}}`)
		p := parsePackage(body)
		assert.Equal(t, "published", p.action)
		assert.Equal(t, "buildhost", p.name)
		assert.Equal(t, "wow-look-at-my/buildhost", p.fullName())
		assert.Equal(t, "CONTAINER", p.pkgType)
	})

	t.Run("deprecated registry_package event", func(t *testing.T) {
		body := []byte(`{"action":"updated","registry_package":{"name":"docker-updater","namespace":"wow-look-at-my"}}`)
		p := parsePackage(body)
		assert.Equal(t, "docker-updater", p.name)
		assert.Equal(t, "wow-look-at-my/docker-updater", p.fullName())
	})

	t.Run("malformed payload yields zero value, no panic", func(t *testing.T) {
		assert.NotPanics(t, func() {
			p := parsePackage([]byte("this is not json"))
			assert.Equal(t, "", p.name)
			assert.Equal(t, "", p.fullName())
			_ = parsePackage(nil)
		})
	})
}

// TestWebhookAllowlistMatch covers the org-level scoping knob: an allowlist that
// matches lets the event through; one that doesn't is acknowledged but ignored.
func TestWebhookAllowlistMatch(t *testing.T) {
	secret := []byte("shhh")

	tests := []struct {
		name        string
		allow       []string
		body        string
		wantTrigger bool
	}{
		{
			name:        "match by name",
			allow:       []string{"buildhost"},
			body:        `{"action":"published","package":{"name":"buildhost","namespace":"wow-look-at-my"}}`,
			wantTrigger: true,
		},
		{
			name:        "match by namespace/name",
			allow:       []string{"wow-look-at-my/buildhost"},
			body:        `{"action":"published","package":{"name":"buildhost","namespace":"wow-look-at-my"}}`,
			wantTrigger: true,
		},
		{
			name:        "match is case-insensitive",
			allow:       []string{"BuildHost"},
			body:        `{"action":"published","package":{"name":"buildhost"}}`,
			wantTrigger: true,
		},
		{
			name:        "non-matching package is ignored",
			allow:       []string{"buildhost"},
			body:        `{"action":"published","package":{"name":"some-other-app","namespace":"wow-look-at-my"}}`,
			wantTrigger: false,
		},
		{
			name:        "unparseable name fails open and triggers",
			allow:       []string{"buildhost"},
			body:        `not json`,
			wantTrigger: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, trigger := newTestWebhook(string(secret), tt.allow...)
			body := []byte(tt.body)

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "package")
			req.Header.Set("X-Hub-Signature-256", sign(secret, body))
			rec := httptest.NewRecorder()
			s.handleWebhook(rec, req)

			if tt.wantTrigger {
				assert.Equal(t, http.StatusAccepted, rec.Code)
				assert.Equal(t, 1, len(trigger))
			} else {
				assert.Equal(t, http.StatusOK, rec.Code)
				assert.Equal(t, 0, len(trigger))
			}
		})
	}
}
