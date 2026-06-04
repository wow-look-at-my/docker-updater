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
	addr   string
	secret []byte
	// allow is an optional set of package identifiers (lowercased "name" and/or
	// "namespace/name"). When non-empty, only matching packages trigger a check
	// -- the knob for an org-level webhook that fires for every package. Empty
	// means any package event triggers.
	allow   map[string]struct{}
	trigger chan<- struct{}
}

func newGitHubWebhookServer(addr, secret string, packages []string, trigger chan<- struct{}) *githubWebhookServer {
	allow := make(map[string]struct{}, len(packages))
	for _, p := range packages {
		allow[strings.ToLower(p)] = struct{}{}
	}
	return &githubWebhookServer{
		addr:    addr,
		secret:  []byte(secret),
		allow:   allow,
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
		pkg := parsePackage(body)
		if !s.allowed(pkg) {
			log.Printf("github webhook: package %q not in allowlist, ignoring", pkg.fullName())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ignored (package not in allowlist)"))
			return
		}
		if pkg.name != "" {
			log.Printf("github webhook: package %q (%s) %s, triggering update check", pkg.fullName(), pkg.pkgType, pkg.action)
		} else {
			log.Print("github webhook: package event received, triggering update check")
		}
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

// packageInfo is the subset of a GitHub package/registry_package payload the
// updater cares about.
type packageInfo struct {
	action    string
	name      string
	namespace string
	pkgType   string
}

// fullName returns "namespace/name" when both are present, else just the name.
func (p packageInfo) fullName() string {
	if p.namespace != "" && p.name != "" {
		return p.namespace + "/" + p.name
	}
	return p.name
}

// parsePackage best-effort extracts package identity from a webhook body. It
// reads both the modern "package" event and the deprecated "registry_package"
// event. A parse failure yields a zero value rather than an error: the signature
// already proved the delivery genuine, so callers fail open and still trigger.
func parsePackage(body []byte) packageInfo {
	var raw struct {
		Action  string         `json:"action"`
		Package *packageFields `json:"package"`
		// Deprecated event shape.
		RegistryPackage *packageFields `json:"registry_package"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return packageInfo{}
	}
	fields := raw.Package
	if fields == nil {
		fields = raw.RegistryPackage
	}
	if fields == nil {
		return packageInfo{action: raw.Action}
	}
	return packageInfo{
		action:    raw.Action,
		name:      fields.Name,
		namespace: fields.Namespace,
		pkgType:   fields.PackageType,
	}
}

type packageFields struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	PackageType string `json:"package_type"`
}

// allowed reports whether a package event should trigger a check. With no
// allowlist configured every package qualifies. With an allowlist, the package
// must match by name or "namespace/name" (case-insensitive). An unparseable
// name fails open: an authenticated delivery we can't classify still triggers,
// since a missed trigger only delays the update to the next interval.
func (s *githubWebhookServer) allowed(p packageInfo) bool {
	if len(s.allow) == 0 || p.name == "" {
		return true
	}
	if _, ok := s.allow[strings.ToLower(p.name)]; ok {
		return true
	}
	if full := p.fullName(); full != p.name {
		if _, ok := s.allow[strings.ToLower(full)]; ok {
			return true
		}
	}
	return false
}
