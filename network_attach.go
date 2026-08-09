package main

import (
	"context"
	"log"
)

// selfAttacher puts docker-updater on a network of every container it monitors.
// The update-check endpoints are dialed at the container's own IP, which only
// works from a network they share -- and the updater already holds the Docker
// socket, so it arranges that itself rather than making an operator run
// `docker network connect` per stack.
//
// It never disconnects: a network it joined for a container that has since gone
// away costs an unused endpoint, while disconnecting a network something else
// put it on would break reachability it did not create.
type selfAttacher struct {
	cli    DockerClient
	selfID string
	joined map[string]bool // network IDs docker-updater is on
}

// newSelfAttacher reads the networks docker-updater is currently on. Built once
// per check cycle, so a network joined (or lost) since the last cycle is
// reflected without any state to go stale.
//
// A nil attacher never attaches, and is what a docker-updater that cannot
// identify its own container gets -- main says so at startup, since without an
// ID there is nothing per-container to add.
func newSelfAttacher(ctx context.Context, cli DockerClient, selfID string) *selfAttacher {
	if selfID == "" {
		return nil
	}
	inspect, err := cli.ContainerInspect(ctx, selfID)
	if err != nil {
		logWarn("cannot attach to monitored containers' networks: inspecting own container %s failed: %v",
			shortID(selfID), err)
		return nil
	}
	a := &selfAttacher{cli: cli, selfID: selfID, joined: map[string]bool{}}
	if inspect.NetworkSettings != nil {
		for _, net := range inspect.NetworkSettings.Networks {
			if net.NetworkID != "" {
				a.joined[net.NetworkID] = true
			}
		}
	}
	return a
}

// ensure joins the container's network when docker-updater is not on it yet,
// and returns a warning when it cannot. A failed join never stops the cycle --
// the update itself does not need the network -- but it is never silent either:
// the probe that follows will fail to reach the container, and this names why.
func (a *selfAttacher) ensure(ctx context.Context, info ContainerInfo) string {
	if a == nil || info.AddressNetwork == "" || a.joined[info.AddressNetwork] {
		return ""
	}
	if err := a.cli.NetworkConnect(ctx, info.AddressNetwork, a.selfID, nil); err != nil {
		return "cannot attach docker-updater to this container's network " +
			shortID(info.AddressNetwork) + ": " + err.Error()
	}
	a.joined[info.AddressNetwork] = true
	log.Printf("attached to network %s to reach container %s", shortID(info.AddressNetwork), info.Name)
	return ""
}
