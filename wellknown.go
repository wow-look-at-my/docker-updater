package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The standardized update-liveness contract a monitored container serves on its
// own HTTP port. Both endpoints answer GET and carry meaning in the status code
// alone; see docs/well-known-endpoints.md for the full contract.
const (
	wellKnownPrefix    = "/.well-known/docker-updater/"
	wellKnownHealth    = wellKnownPrefix + "health"
	wellKnownPreUpdate = wellKnownPrefix + "pre-update"
)

// Labels that steer discovery. The port override matters for a container
// exposing several ports (there is no way to guess which one speaks the
// contract); the opt-out silences the warning for containers that legitimately
// serve no HTTP at all (a database, a queue worker).
const (
	wellKnownPortLabel   = "docker-updater.well-known.port"
	wellKnownEnableLabel = "docker-updater.well-known"
)

// wellKnownProbeTimeout bounds the discovery GET. It targets a container on the
// local host, so this is generous. Var so tests can shrink it.
var wellKnownProbeTimeout = 3 * time.Second

// Default budgets for the update checks, applied to both the label-configured
// and the standard /.well-known/docker-updater/ endpoints.
const (
	defaultPreCheckTimeout    = 30 * time.Second
	defaultHealthCheckTimeout = 60 * time.Second
)

// containerAddress returns the address docker-updater reaches a container on:
// its first network IP, or 127.0.0.1 when it shares the host's namespace (host
// networking has no per-container IP, and docker-updater is meant to run with
// --network host itself).
func containerAddress(inspect types.ContainerJSON) string {
	if inspect.NetworkSettings == nil {
		return ""
	}
	for _, net := range inspect.NetworkSettings.Networks {
		if net.IPAddress != "" {
			return net.IPAddress
		}
	}
	if inspect.HostConfig != nil && inspect.HostConfig.NetworkMode.IsHost() {
		return "127.0.0.1"
	}
	return ""
}

// exposedTCPPorts returns the container's declared TCP ports, ascending. These
// are the ports the image/compose file declares, not only published ones: with
// host networking or a shared bridge, docker-updater reaches an unpublished
// port fine.
func exposedTCPPorts(inspect types.ContainerJSON) []int {
	if inspect.Config == nil {
		return nil
	}
	var ports []int
	for p := range inspect.Config.ExposedPorts {
		if p.Proto() != "tcp" {
			continue
		}
		if n := p.Int(); n > 0 {
			ports = append(ports, n)
		}
	}
	sort.Ints(ports)
	return ports
}

// wellKnownState is the resolved outcome of discovery for one container: the
// endpoint URLs to use (empty when unavailable) and any warnings to surface on
// the dashboard and in the log.
type wellKnownState struct {
	HealthURL    string
	PreUpdateURL string
	Warnings     []string
}

// overrideLabels lists the per-container check labels that displace the
// standard endpoints. Their presence is what makes a container "nonstandard".
func overrideLabels(info ContainerInfo) []string {
	var set []string
	for _, l := range []string{
		"docker-updater.pre-check.url",
		"docker-updater.pre-check.command",
		"docker-updater.health-check.url",
		"docker-updater.health-check.command",
	} {
		if info.Labels[l] != "" {
			set = append(set, l)
		}
	}
	return set
}

// resolveWellKnown decides how a container's update checks are addressed this
// cycle. Explicit check labels always win — that is the compatibility contract
// with the original opt-in setup — but they are reported as nonstandard so a
// fleet can be migrated deliberately rather than by accident.
func resolveWellKnown(ctx context.Context, info ContainerInfo) wellKnownState {
	if set := overrideLabels(info); len(set) > 0 {
		return wellKnownState{Warnings: []string{
			"nonstandard update checks: " + strings.Join(set, ", ") + " override the standard " +
				wellKnownPrefix + " endpoints",
		}}
	}
	if info.Labels[wellKnownEnableLabel] == "false" {
		return wellKnownState{}
	}

	base, err := wellKnownBaseURL(info)
	if err != nil {
		return wellKnownState{Warnings: []string{"no standard update endpoints: " + err.Error()}}
	}
	implemented, reachErr := probeWellKnown(ctx, base+wellKnownHealth)
	switch {
	case reachErr != nil:
		// No answer at all: the break is the route or the port, not the endpoint.
		return wellKnownState{Warnings: []string{
			"cannot reach " + base + wellKnownHealth + " (" + reachErr.Error() + "); post-update liveness falls " +
				"back to Docker HEALTHCHECK. docker-updater must share a network with the container (it is meant " +
				"to run with --network host), and " + wellKnownPortLabel + " must name a port it serves on",
		}}
	case !implemented:
		return wellKnownState{Warnings: []string{
			"does not serve " + wellKnownHealth + " (probed " + base + "); post-update liveness falls back to " +
				"Docker HEALTHCHECK. Implement the endpoint, or set " + wellKnownEnableLabel + "=false to silence this",
		}}
	}
	// A container answering /health is assumed to speak the contract. The
	// pre-update endpoint is optional and never probed separately: gating
	// treats a 404 there as "not implemented, go ahead", so a second discovery
	// request would buy nothing.
	return wellKnownState{HealthURL: base + wellKnownHealth, PreUpdateURL: base + wellKnownPreUpdate}
}

// wellKnownBaseURL builds http://<address>:<port> for a container, or explains
// why it cannot. Address comes from the container's own network settings
// (127.0.0.1 under host networking); the port is the label override, else the
// container's single exposed TCP port.
func wellKnownBaseURL(info ContainerInfo) (string, error) {
	if info.Address == "" {
		return "", fmt.Errorf("container has no reachable address")
	}
	port, err := wellKnownPort(info)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(info.Address, strconv.Itoa(port)), nil
}

func wellKnownPort(info ContainerInfo) (int, error) {
	if raw := info.Labels[wellKnownPortLabel]; raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return 0, fmt.Errorf("%s=%q is not a valid port", wellKnownPortLabel, raw)
		}
		return port, nil
	}
	switch len(info.ExposedPorts) {
	case 0:
		return 0, fmt.Errorf("container exposes no TCP port; set %s", wellKnownPortLabel)
	case 1:
		return info.ExposedPorts[0], nil
	default:
		return 0, fmt.Errorf("container exposes %d TCP ports (%s); set %s to pick one",
			len(info.ExposedPorts), joinPorts(info.ExposedPorts), wellKnownPortLabel)
	}
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

// probeWellKnown separates "answered 404/501" (not implemented) from "did not answer at all" (reachErr): a merely unhealthy container still speaks the contract, and a network fault reported as missing code sends the operator to write what is already there.
func probeWellKnown(ctx context.Context, url string) (implemented bool, reachErr error) {
	ctx, cancel := context.WithTimeout(ctx, wellKnownProbeTimeout)
	defer cancel()

	resp, err := wellKnownGet(ctx, url)
	if err != nil {
		// *url.Error repeats the URL the caller already has; report the cause.
		var uerr *neturl.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			err = uerr.Err
		}
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusNotImplemented, nil
}

func wellKnownGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "docker-updater")
	return http.DefaultClient.Do(req)
}

// applyWellKnown folds discovery into a container's check configuration: the
// standard endpoints fill in wherever no explicit label set one. Returns the
// updated info and the warnings for this cycle.
func applyWellKnown(ctx context.Context, info ContainerInfo) (ContainerInfo, []string) {
	state := resolveWellKnown(ctx, info)

	if state.HealthURL != "" && info.HealthCheckURL == "" && info.HealthCheckCommand == "" {
		info.HealthCheckURL = state.HealthURL
		info.HealthCheckTimeout = defaultHealthCheckTimeout
		// Built from this container's address, which the update replaces --
		// see waitPostUpdateHealthy.
		info.HealthCheckURLFromContainer = true
	}
	if state.PreUpdateURL != "" && info.PreCheckURL == "" && info.PreCheckCommand == "" {
		info.PreCheckURL = state.PreUpdateURL
		info.PreCheckTimeout = defaultPreCheckTimeout
		// The standard pre-update endpoint is optional, so its absence must
		// never block an update -- see runPreCheck.
		info.PreCheckStandard = true
	}

	logWellKnownWarnings(info.Name, state.Warnings)
	return info, state.Warnings
}

// warnedOnce keeps a container's warning out of the log on every cycle: it is
// logged when first seen and again only if it changes (the dashboard shows the
// live state regardless).
var warnedOnce sync.Map

func logWellKnownWarnings(name string, warnings []string) {
	key := name
	joined := strings.Join(warnings, "; ")
	if prev, ok := warnedOnce.Load(key); ok && prev == joined {
		return
	}
	warnedOnce.Store(key, joined)
	for _, w := range warnings {
		logWarn("container %s: %s", name, w)
	}
}

// logWarn marks a line as operator-actionable in an otherwise chatty log.
func logWarn(format string, a ...any) {
	log.Printf("WARNING: "+format, a...)
}
