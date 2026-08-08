package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wellKnownServer starts a server answering the standard endpoints with the
// given statuses (0 = do not register the route, i.e. 404), and returns a
// ContainerInfo addressed at it.
func wellKnownServer(t *testing.T, healthStatus, preUpdateStatus int) (ContainerInfo, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	if healthStatus != 0 {
		mux.HandleFunc(wellKnownHealth, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(healthStatus) })
	}
	if preUpdateStatus != 0 {
		mux.HandleFunc(wellKnownPreUpdate, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(preUpdateStatus) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	return ContainerInfo{
		ID:           "c1",
		Name:         "app",
		Labels:       map[string]string{},
		Address:      host,
		ExposedPorts: []int{port},
	}, srv
}

// A container serving the standard endpoints needs no configuration at all:
// discovery addresses it from its own network settings and exposed port.
func TestWellKnownDiscoversStandardEndpoints(t *testing.T) {
	info, srv := wellKnownServer(t, http.StatusOK, http.StatusOK)

	state := resolveWellKnown(context.Background(), info)

	assert.Equal(t, srv.URL+wellKnownHealth, state.HealthURL)
	assert.Equal(t, srv.URL+wellKnownPreUpdate, state.PreUpdateURL)
	assert.Empty(t, state.Warnings, "a conforming container must not be warned about")
}

// An endpoint that answers non-2xx still IMPLEMENTS the contract: an unhealthy
// container is a health problem, not a configuration one.
func TestWellKnownTreatsUnhealthyAsConfigured(t *testing.T) {
	info, _ := wellKnownServer(t, http.StatusServiceUnavailable, http.StatusOK)

	state := resolveWellKnown(context.Background(), info)

	assert.NotEmpty(t, state.HealthURL)
	assert.Empty(t, state.Warnings)
}

// The warning every unconfigured container gets: what is missing, what was
// probed, what happens instead, and how to silence it.
func TestWellKnownWarnsWhenEndpointMissing(t *testing.T) {
	info, srv := wellKnownServer(t, 0, 0) // no routes: 404 for both

	state := resolveWellKnown(context.Background(), info)

	assert.Empty(t, state.HealthURL)
	require.Len(t, state.Warnings, 1)
	assert.Contains(t, state.Warnings[0], wellKnownHealth)
	assert.Contains(t, state.Warnings[0], srv.URL)
	assert.Contains(t, state.Warnings[0], "Docker HEALTHCHECK")
	assert.Contains(t, state.Warnings[0], wellKnownEnableLabel+"=false")
}

// A container that never answers is a different fault from one that answers
// 404, and must not get the same sentence: "implement the endpoint" sends the
// operator to write code that may already be there, while the actual break is
// the route to the container or the port being probed.
func TestWellKnownWarnsSeparatelyWhenUnreachable(t *testing.T) {
	info, srv := wellKnownServer(t, http.StatusOK, http.StatusOK)
	srv.Close() // the address is still addressed, but nothing listens on it now

	state := resolveWellKnown(context.Background(), info)

	assert.Empty(t, state.HealthURL)
	require.Len(t, state.Warnings, 1)
	assert.Contains(t, state.Warnings[0], "cannot reach")
	assert.Contains(t, state.Warnings[0], "--network host")
	assert.Contains(t, state.Warnings[0], wellKnownPortLabel)
	assert.NotContains(t, state.Warnings[0], "Implement the endpoint",
		"nothing about an unanswered probe shows the endpoint is missing")
}

// Compatibility with the original opt-in setup: the check labels still win,
// and are reported as nonstandard rather than silently honored.
func TestWellKnownLabelOverridesAreNonstandard(t *testing.T) {
	info, _ := wellKnownServer(t, http.StatusOK, http.StatusOK)
	info.Labels["docker-updater.health-check.url"] = "http://10.0.0.5:9000/healthz"
	info.HealthCheckURL = "http://10.0.0.5:9000/healthz"

	state := resolveWellKnown(context.Background(), info)

	assert.Empty(t, state.HealthURL, "an override must not be replaced by discovery")
	require.Len(t, state.Warnings, 1)
	assert.Contains(t, state.Warnings[0], "nonstandard")
	assert.Contains(t, state.Warnings[0], "docker-updater.health-check.url")
	assert.Contains(t, state.Warnings[0], wellKnownPrefix)

	// And the override survives applyWellKnown untouched.
	applied, warnings := applyWellKnown(context.Background(), info)
	assert.Equal(t, "http://10.0.0.5:9000/healthz", applied.HealthCheckURL)
	assert.False(t, applied.PreCheckStandard)
	assert.Equal(t, state.Warnings, warnings)
}

// Containers that serve no HTTP at all (a database) can opt out entirely
// instead of warning on every cycle.
func TestWellKnownOptOutSilencesTheWarning(t *testing.T) {
	info, _ := wellKnownServer(t, 0, 0)
	info.Labels[wellKnownEnableLabel] = "false"

	state := resolveWellKnown(context.Background(), info)

	assert.Empty(t, state.Warnings)
	assert.Empty(t, state.HealthURL)
}

// Port selection: one exposed port is unambiguous, several are not, and the
// label settles it either way.
func TestWellKnownPortSelection(t *testing.T) {
	base := ContainerInfo{Labels: map[string]string{}, Address: "10.0.0.7"}

	one := base
	one.ExposedPorts = []int{8080}
	url, err := wellKnownBaseURL(one)
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.7:8080", url)

	many := base
	many.ExposedPorts = []int{80, 443, 9000}
	_, err = wellKnownBaseURL(many)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "80, 443, 9000")
	assert.Contains(t, err.Error(), wellKnownPortLabel)

	picked := many
	picked.Labels = map[string]string{wellKnownPortLabel: "443"}
	url, err = wellKnownBaseURL(picked)
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.7:443", url)

	none := base
	_, err = wellKnownBaseURL(none)
	require.Error(t, err)
	assert.Contains(t, err.Error(), wellKnownPortLabel)

	bad := one
	bad.Labels = map[string]string{wellKnownPortLabel: "http"}
	_, err = wellKnownBaseURL(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid port")
}

// A container with no address (never started, no network) is a warning, not a
// crash or a bogus URL.
func TestWellKnownWarnsWithoutAddress(t *testing.T) {
	state := resolveWellKnown(context.Background(), ContainerInfo{Name: "x", Labels: map[string]string{}, ExposedPorts: []int{80}})

	require.Len(t, state.Warnings, 1)
	assert.Contains(t, state.Warnings[0], "no reachable address")
}

// Discovery fills in BOTH checks for a conforming container, and marks the
// pre-update gate as the discovered (optional) one.
func TestApplyWellKnownFillsBothChecks(t *testing.T) {
	info, srv := wellKnownServer(t, http.StatusOK, http.StatusOK)

	applied, warnings := applyWellKnown(context.Background(), info)

	assert.Empty(t, warnings)
	assert.Equal(t, srv.URL+wellKnownHealth, applied.HealthCheckURL)
	assert.Equal(t, defaultHealthCheckTimeout, applied.HealthCheckTimeout)
	assert.Equal(t, srv.URL+wellKnownPreUpdate, applied.PreCheckURL)
	assert.Equal(t, defaultPreCheckTimeout, applied.PreCheckTimeout)
	assert.True(t, applied.PreCheckStandard)
}

// The pre-update endpoint is optional. A container serving /health but not
// /pre-update must still update -- the discovered gate has no opinion.
func TestStandardPreUpdateAbsenceDoesNotBlock(t *testing.T) {
	info, _ := wellKnownServer(t, http.StatusOK, 0)
	applied, _ := applyWellKnown(context.Background(), info)
	require.True(t, applied.PreCheckStandard)

	assert.NoError(t, runPreCheck(context.Background(), nil, applied),
		"a 404 on the optional pre-update endpoint must not hold the update back")

	// Neither may an unreachable container: nothing to ask is not "hold forever".
	gone := applied
	gone.PreCheckURL = "http://127.0.0.1:1" + wellKnownPreUpdate
	assert.NoError(t, runPreCheck(context.Background(), nil, gone))
}

// ...but a container that answers the gate with a refusal still holds.
func TestStandardPreUpdateRefusalBlocks(t *testing.T) {
	info, _ := wellKnownServer(t, http.StatusOK, http.StatusServiceUnavailable)
	applied, _ := applyWellKnown(context.Background(), info)

	err := runPreCheck(context.Background(), nil, applied)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

// A label-configured pre-check is a declared gate: its 404 is a real failure,
// unlike the discovered endpoint's.
func TestLabelPreCheckStillFailsClosed(t *testing.T) {
	info, srv := wellKnownServer(t, http.StatusOK, 0)
	info.PreCheckURL = srv.URL + "/my-precheck"
	info.PreCheckTimeout = defaultPreCheckTimeout

	err := runPreCheck(context.Background(), nil, info)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestContainerAddressPrefersNetworkIPThenHost(t *testing.T) {
	bridged := types.ContainerJSON{NetworkSettings: &types.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{"bridge": {IPAddress: "172.17.0.4"}},
	}}
	assert.Equal(t, "172.17.0.4", containerAddress(bridged))

	hostNet := types.ContainerJSON{
		NetworkSettings:   &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{"host": {}}},
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{NetworkMode: "host"}},
	}
	assert.Equal(t, "127.0.0.1", containerAddress(hostNet))

	assert.Empty(t, containerAddress(types.ContainerJSON{}))
}
