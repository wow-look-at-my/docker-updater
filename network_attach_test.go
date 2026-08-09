package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfOn builds an inspect for docker-updater's own container sitting on the
// given network IDs.
func selfOn(networkIDs ...string) types.ContainerJSON {
	nets := map[string]*network.EndpointSettings{}
	for i, id := range networkIDs {
		nets[string(rune('a'+i))] = &network.EndpointSettings{NetworkID: id, IPAddress: "172.20.0.2"}
	}
	return types.ContainerJSON{NetworkSettings: &types.NetworkSettings{Networks: nets}}
}

// attacherFor returns an attacher for a docker-updater on the given networks,
// plus the (networkID, containerID) pairs it connects.
func attacherFor(t *testing.T, self types.ContainerJSON, connectErr error) (*selfAttacher, *[][2]string) {
	t.Helper()
	var connects [][2]string
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return self, nil
		},
		networkConnectFn: func(_ context.Context, netID, containerID string, _ *network.EndpointSettings) error {
			if connectErr != nil {
				return connectErr
			}
			connects = append(connects, [2]string{netID, containerID})
			return nil
		},
	}
	return newSelfAttacher(context.Background(), cli, "self123456789"), &connects
}

func TestSelfAttacherJoinsTheContainersNetwork(t *testing.T) {
	a, connects := attacherFor(t, selfOn("net-updater"), nil)

	warning := a.ensure(context.Background(), ContainerInfo{Name: "app", AddressNetwork: "net-app"})

	assert.Empty(t, warning)
	require.Len(t, *connects, 1)
	assert.Equal(t, [2]string{"net-app", "self123456789"}, (*connects)[0])
}

// The whole point is that an operator never wires this up: a container on a
// network docker-updater is already on must not produce a redundant connect
// (Docker rejects it, which would read as a failure).
func TestSelfAttacherSkipsNetworkItIsAlreadyOn(t *testing.T) {
	a, connects := attacherFor(t, selfOn("net-shared"), nil)

	warning := a.ensure(context.Background(), ContainerInfo{Name: "app", AddressNetwork: "net-shared"})

	assert.Empty(t, warning)
	assert.Empty(t, *connects)
}

func TestSelfAttacherJoinsEachNetworkOnce(t *testing.T) {
	a, connects := attacherFor(t, selfOn("net-updater"), nil)

	for _, name := range []string{"app", "worker", "cache"} {
		assert.Empty(t, a.ensure(context.Background(), ContainerInfo{Name: name, AddressNetwork: "net-app"}))
	}

	assert.Len(t, *connects, 1, "three containers on one network is one join")
}

// A container with no IP of its own (host/none/container: network mode) has no
// network to join -- attaching to nothing must not be attempted or warned about,
// because the address warning already names that case.
func TestSelfAttacherIgnoresContainerWithoutNetwork(t *testing.T) {
	a, connects := attacherFor(t, selfOn("net-updater"), nil)

	assert.Empty(t, a.ensure(context.Background(), ContainerInfo{Name: "hostnet"}))
	assert.Empty(t, *connects)
}

// A failed join is what makes the probe that follows unreachable, so it must
// name itself rather than leaving the operator to explain a refused connection.
func TestSelfAttacherWarnsWhenJoinFails(t *testing.T) {
	a, _ := attacherFor(t, selfOn("net-updater"), errors.New("network net-app not found"))

	warning := a.ensure(context.Background(), ContainerInfo{Name: "app", AddressNetwork: "net-app"})

	assert.Contains(t, warning, "cannot attach docker-updater")
	assert.Contains(t, warning, "network net-app not found")
}

// A retry next cycle is the correct behavior for a transient failure: the
// network must not be recorded as joined when the join did not happen.
func TestSelfAttacherRetriesAfterAFailedJoin(t *testing.T) {
	a, _ := attacherFor(t, selfOn("net-updater"), errors.New("boom"))

	info := ContainerInfo{Name: "app", AddressNetwork: "net-app"}
	assert.NotEmpty(t, a.ensure(context.Background(), info))
	assert.NotEmpty(t, a.ensure(context.Background(), info), "a failed join is not remembered as joined")
}

func TestSelfAttacherDisabledWithoutOwnContainerID(t *testing.T) {
	cli := &mockDocker{
		networkConnectFn: func(_ context.Context, _, _ string, _ *network.EndpointSettings) error {
			t.Fatal("must not connect anything without knowing which container it is")
			return nil
		},
	}

	a := newSelfAttacher(context.Background(), cli, "")

	assert.Nil(t, a)
	assert.Empty(t, a.ensure(context.Background(), ContainerInfo{Name: "app", AddressNetwork: "net-app"}))
}

// An unusable DOCKER_UPDATER_CONTAINER_ID must disable attaching rather than
// fire a connect for a container that is not us.
func TestSelfAttacherDisabledWhenSelfInspectFails(t *testing.T) {
	cli := &mockDocker{
		containerInspectFn: func(_ context.Context, _ string) (types.ContainerJSON, error) {
			return types.ContainerJSON{}, errors.New("no such container")
		},
		networkConnectFn: func(_ context.Context, _, _ string, _ *network.EndpointSettings) error {
			t.Fatal("must not connect when its own networks are unknown")
			return nil
		},
	}

	assert.Nil(t, newSelfAttacher(context.Background(), cli, "bogus"))
}
