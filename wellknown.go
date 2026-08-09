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

// containerEndpoint returns the container's own IP and the ID of the network it
// belongs to -- the address docker-updater dials, and the network it has to
// join to get there.
//
// Both are empty when the container has no IP of its own (host, none, or
// container: network mode), and there is no substitute to fall back on:
// 127.0.0.1 is docker-updater's OWN loopback, so probing it reports on the
// wrong process entirely and can pass a post-update health gate against
// docker-updater itself. Such a container needs an absolute
// docker-updater.health-check.url instead.
//
// Networks are considered in name order because Go randomizes map iteration: a
// multi-network container must resolve to the SAME endpoint every cycle, or the
// updater joins a second network for nothing and the address the health gate
// re-resolves after an update belongs to a different network than the one it
// probed before it.
func containerEndpoint(inspect types.ContainerJSON) (address, networkID string) {
	if inspect.NetworkSettings == nil {
		return "", ""
	}
	names := make([]string, 0, len(inspect.NetworkSettings.Networks))
	for name := range inspect.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if net := inspect.NetworkSettings.Networks[name]; net.IPAddress != "" {
			return net.IPAddress, net.NetworkID
		}
	}
	return "", ""
}

// hasDockerHealthcheck reports whether the container has an EFFECTIVE Docker
// HEALTHCHECK. It reads State.Health, which is exactly what waitHealthy
// branches on -- so a warning built from this can never promise a fallback the
// gate will not actually take. Reading the image's Config.Healthcheck instead
// would over-report: `--no-healthcheck` and a `["NONE"]` test both leave the
// config populated while the runtime has no health at all.
// State is promoted through the embedded *ContainerJSONBase, so the base has
// to be checked before it -- reaching straight for .State panics on a
// zero-value inspect.
func hasDockerHealthcheck(inspect types.ContainerJSON) bool {
	if inspect.ContainerJSONBase == nil || inspect.State == nil {
		return false
	}
	return inspect.State.Health != nil
}

// exposedTCPPorts returns the container's declared TCP ports, ascending. These
// are the ports the image/compose file declares, not only published ones:
// docker-updater dials the container directly on a network they share, so an
// unpublished port is reachable.
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
		// No HTTP answer at all. Saying "implement the endpoint" here sends the
		// operator to write code that may already exist: what is actually
		// broken is the route to the container, or the port being probed.
		return wellKnownState{Warnings: []string{
			"cannot reach " + base + wellKnownHealth + " (" + reachErr.Error() + "); " + fallbackPhrase(info) +
				". docker-updater must be attached to a network the container is on, and " +
				wellKnownPortLabel + " must name a port it serves on",
		}}
	case !implemented:
		return wellKnownState{Warnings: []string{
			"does not serve " + wellKnownHealth + " (probed " + base + "); " + fallbackPhrase(info) +
				". Implement the endpoint, or set " + wellKnownEnableLabel + "=false to silence this",
		}}
	}
	// A container answering /health is assumed to speak the contract. The
	// pre-update endpoint is optional and never probed separately: gating
	// treats a 404 there as "not implemented, go ahead", so a second discovery
	// request would buy nothing.
	return wellKnownState{HealthURL: base + wellKnownHealth, PreUpdateURL: base + wellKnownPreUpdate}
}

// fallbackPhrase names the post-update gate this container will ACTUALLY get
// once the standard endpoint is out of the picture, which is not the same
// sentence for every container: waitPostUpdateHealthy falls through to
// waitHealthy, and waitHealthy only waits for a health STATUS when the
// container has one. Without a HEALTHCHECK the gate degrades to "it stayed
// running", and an operator told they still have HEALTHCHECK cover would
// believe an update was verified when nothing verified it.
func fallbackPhrase(info ContainerInfo) string {
	if info.DockerHealthcheck {
		return "post-update liveness falls back to Docker HEALTHCHECK"
	}
	return "post-update liveness is UNVERIFIED beyond the container staying up briefly " +
		"(this container has no Docker HEALTHCHECK to fall back to)"
}

// wellKnownBaseURL builds http://<address>:<port> for a container, or explains
// why it cannot. Address is the container's own IP; the port is the label
// override, else the container's single exposed TCP port.
func wellKnownBaseURL(info ContainerInfo) (string, error) {
	if info.Address == "" {
		return "", errors.New("container has no IP of its own (host, none, or container: network mode); " +
			"give it an absolute docker-updater.health-check.url instead")
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

// probeWellKnown reports whether the endpoint exists, and separately whether
// the container answered at all. Any HTTP answer other than 404/501 counts as
// implemented: a container that is merely unhealthy right now still speaks the
// contract, and reporting it as unconfigured would send the operator to fix the
// wrong thing.
//
// A returned error means no answer arrived — refused, timed out, unroutable.
// That is a different fault with a different fix, and collapsing it into "not
// implemented" makes a network problem read as missing code.
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
// updated info and the warnings for this cycle; the caller logs them together
// with everything else it collected for the container.
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

	return info, state.Warnings
}

// warnedOnce keeps a container's warning out of the log on every cycle: it is
// logged when first seen and again only if it changes (the dashboard shows the
// live state regardless).
var warnedOnce sync.Map

func logContainerWarnings(name string, warnings []string) {
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
