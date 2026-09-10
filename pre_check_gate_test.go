package main

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
)

// A pre-check URL that names the container by hostname resolves only from a
// network docker-updater shares with it, so the gate is never asked from
// outside that network.
func TestPreCheckGateUnreachableHostnameRefuses(t *testing.T) {
	reason := preCheckRefuses(context.Background(), nil, ContainerInfo{
		Name:            "webhook-runner",
		State:           "running",
		PreCheckURL:     "http://no-such-host.invalid:9001/restart-ready",
		PreCheckTimeout: time.Second,
	})

	assert.Contains(t, reason, "pre-check HTTP request failed")
}

// A container that is not running has nothing to drain. Asking its gate kept
// webhook-runner crash-looping on a broken image for hours after the fixed one
// was published: the gate lives in the container that cannot start.
func TestPreCheckGateSkippedForAContainerThatIsNotRunning(t *testing.T) {
	for _, state := range []string{"restarting", "exited", "dead", "created"} {
		reason := preCheckRefuses(context.Background(), nil, ContainerInfo{
			Name:            "webhook-runner",
			State:           state,
			PreCheckURL:     "http://no-such-host.invalid:9001/restart-ready",
			PreCheckTimeout: time.Second,
		})
		assert.Empty(t, reason, "state %s must not be gated", state)
	}
}

// The network a container sits on is known whether or not it currently holds
// an IP, so docker-updater joins it before the container is back up and the
// hostname resolves the moment it is.
func TestContainerEndpointNamesTheNetworkWithoutAnIP(t *testing.T) {
	inspect := types.ContainerJSON{NetworkSettings: &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{
		"go-s3-server_default": {NetworkID: "net-s3"},
	}}}

	address, networkID := containerEndpoint(inspect)

	assert.Empty(t, address)
	assert.Equal(t, "net-s3", networkID)
}
