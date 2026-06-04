package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// maxWebhookBody caps how much of a webhook request body we read. GitHub
// package payloads are a few KB; bounding the read protects the public endpoint
// from memory-exhaustion attempts.
const maxWebhookBody = 1 << 20 // 1 MiB

// githubWebhookServer is an authenticated, publicly-reachable HTTP endpoint that
// lets GitHub notify the updater that a package (a ghcr image) was published or
// updated. A valid delivery triggers an immediate update check, so a freshly
// pushed image rolls out without waiting for the next interval tick.
//
// It runs as its own listener, separate from the dashboard, so the public
// webhook surface stays isolated from the dashboard's container inventory: the
// operator can expose this port to the internet while keeping the dashboard
// internal.
type githubWebhookServer struct {
	addr    string
	secret  []byte
	trigger chan<- struct{}
}

func newGitHubWebhookServer(addr, secret string, trigger chan<- struct{}) *githubWebhookServer {
	return &githubWebhookServer{
		addr:    addr,
		secret:  []byte(secret),
		trigger: trigger,
	}
}

// run starts the webhook listener and blocks until the context is cancelled. A
// bind failure is logged but never fatal: the update loop keeps running on its
// interval even if the webhook cannot listen.
func (s *githubWebhookServer) run(ctx context.Context) {
	mux := http.NewServeMux()
	// Mounted at the root so the operator is free to choose any path in the
	// GitHub webhook URL (e.g. behind a reverse proxy); every path lands here.
	mux.HandleFunc("/", s.handleWebhook)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("github webhook listening on %s", s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("github webhook server error on %s: %v", s.addr, err)
	}
}

func (s *githubWebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Authenticate every request against the shared secret using GitHub's
	// HMAC-SHA256 scheme before trusting anything it carries. This is the lock
	// on the public endpoint: without a valid signature the request is rejected.
	if !validSignature(s.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		// GitHub sends a ping when the webhook is created. Ack it so the
		// delivery is marked healthy, but don't trigger a check.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	case "package", "registry_package":
		logPackageEvent(body)
		s.fire()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("update check triggered"))
	default:
		// Authenticated, but not an event we act on. Ack without triggering.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ignored"))
	}
}

// fire requests an update check without blocking. The trigger channel is
// buffered with depth 1, so a burst of deliveries (e.g. a multi-arch push that
// emits several package events) coalesces into at most one pending check rather
// than queueing a storm.
func (s *githubWebhookServer) fire() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// validSignature reports whether sigHeader is a valid GitHub "sha256=<hex>"
// HMAC-SHA256 signature of body under secret, compared in constant time.
func validSignature(secret, body []byte, sigHeader string) bool {
	const prefix = "sha256="
	if len(secret) == 0 || !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want, err := hex.DecodeString(sigHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// logPackageEvent best-effort logs which package triggered the check. A parse
// failure is non-fatal: the signature already proved the delivery is genuine,
// so the check still fires.
func logPackageEvent(body []byte) {
	var p struct {
		Action  string `json:"action"`
		Package struct {
			Name        string `json:"name"`
			PackageType string `json:"package_type"`
		} `json:"package"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Package.Name == "" {
		log.Print("github webhook: package event received, triggering update check")
		return
	}
	log.Printf("github webhook: package %q (%s) %s, triggering update check", p.Package.Name, p.Package.PackageType, p.Action)
}
