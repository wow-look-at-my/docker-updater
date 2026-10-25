// createPath matches the Docker API call that creates a container, with or without a version prefix.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// createPath matches the Docker API call that creates a container, with or without a version prefix.
var createPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)

// errCreateCaptured is what compose reads back for its create call. It aborts compose before it touches the container.
const errCreateCaptured = "docker-updater captured this create request to compare it; nothing was created"

// createCapture is a Docker API socket for one compose run.
type createCapture struct {
	upstream http.Handler
	mu       sync.Mutex
	body     []byte
	refused  []string
}

func (c *createCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		c.upstream.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost && createPath.MatchString(r.URL.Path) {
		body, err := io.ReadAll(r.Body)
		c.mu.Lock()
		if err == nil && c.body == nil {
			c.body = body
		}
		c.mu.Unlock()
		writeDockerError(w, http.StatusInternalServerError, errCreateCaptured)
		return
	}
	c.mu.Lock()
	c.refused = append(c.refused, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	writeDockerError(w, http.StatusForbidden, "docker-updater refuses "+r.Method+" "+r.URL.Path+" while it reads the compose config")
}

func writeDockerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// dockerUpstream is a reverse proxy to the daemon that DOCKER_HOST names, or to the default socket.
func dockerUpstream() (http.Handler, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST %q: %w", host, err)
	}
	if os.Getenv("DOCKER_TLS_VERIFY") != "" {
		return nil, fmt.Errorf("DOCKER_HOST %s needs TLS, which the compose config capture does not carry", host)
	}
	target := &url.URL{Scheme: "http", Host: u.Host}
	transport := &http.Transport{}
	switch u.Scheme {
	case "unix":
		path := u.Path
		target.Host = "docker"
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
	default:
		return nil, fmt.Errorf("DOCKER_HOST %s: the compose config capture supports unix and tcp only", host)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.FlushInterval = -1
	return proxy, nil
}

// CreateRequest runs the recreate `docker compose up` would do for the
// service against a createCapture socket, and returns the body of the
// container create request compose sent. That body is the container compose
// would create from the file now, in the Docker API's own form.
func (execComposeRunner) CreateRequest(ctx context.Context, t composeTarget) ([]byte, error) {
	if err := checkComposeFilesVisible(t.ConfigFiles); err != nil {
		return nil, err
	}
	upstream, err := dockerUpstream()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "docker-updater-capture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("opening the capture socket: %w", err)
	}
	capture := &createCapture{upstream: upstream}
	srv := &http.Server{Handler: capture}
	go srv.Serve(ln)
	defer srv.Close()

	args := composeArgs(t, "up", "--no-deps", "--no-build", "--no-start", "--pull", "never", "--force-recreate", t.Service)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(captureEnv(os.Environ()), "DOCKER_HOST=unix://"+sock)
	stderr := &tailBuffer{max: composeErrTailBytes}
	cmd.Stderr = stderr
	runErr := cmd.Run()

	capture.mu.Lock()
	body, refused := capture.body, capture.refused
	capture.mu.Unlock()
	if body != nil {
		return body, nil
	}
	tail := stderr.tail(composeErrTailLines)
	if len(refused) > 0 {
		return nil, fmt.Errorf("compose wanted to %s before it created the container, so its create request is unknown: %s", strings.Join(refused, ", "), tail)
	}
	if runErr == nil {
		runErr = errors.New("compose exited without a create request")
	}
	return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), runErr, tail)
}

// captureEnv drops the variables that would point compose at a daemon other than the capture socket.
func captureEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
			continue
		}
		out = append(out, kv)
	}
	return out
}
