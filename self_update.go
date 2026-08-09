package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// docker-updater cannot update its own container the way it updates any other
// one: recreateContainer stops and removes the target before creating the
// replacement, and stopping its *own* container kills this process mid-swap --
// nothing is left to create and start the new container, so the updater would
// vanish (and, with a restart policy, the removed container can't be revived
// either). This is why a published fix never reaches a running updater.
//
// The fix is a hand-off: when the container to update is our own, we spawn a
// short-lived, detached helper container from the freshly-pulled new image that
// runs the hidden `finish-self-update` subcommand. The helper -- a separate
// container with its own lifecycle -- recreates our container on the new image
// and then exits. Because it reuses recreateContainer, the rollback and health
// gate guarantees apply to the updater itself, so a broken new image rolls the
// updater back instead of leaving it down.

// finishSelfUpdateCommand is the hidden argv[1] the helper container runs to
// complete a self-update. Invoked as `/docker-updater finish-self-update ...`.
const finishSelfUpdateCommand = "finish-self-update"

// selfUpdateHelperLabel marks the helper container so it is never itself picked
// up as a monitored target (it carries no enable label anyway, but this makes
// the intent explicit and greppable).
const selfUpdateHelperLabel = "docker-updater.self-update-helper"

// containerIDRe matches a 64-hex Docker container ID embedded in a host path.
var containerIDRe = regexp.MustCompile(`[0-9a-f]{64}`)

// detectOwnContainerID returns the ID of the container this process runs in, or
// "" if it cannot be determined (e.g. running outside Docker). It reads
// /proc/self/mountinfo first: Docker bind-mounts /etc/hostname, /etc/hosts and
// /etc/resolv.conf from /var/lib/docker/containers/<id>/, so the ID is present
// there regardless of network mode -- unlike the hostname==short-ID convention,
// which any container carrying an explicit or inherited hostname defeats. Falls
// back to /proc/self/cgroup for cgroup v1 hosts.
func detectOwnContainerID() string {
	if id := containerIDFromMountinfo(readFileString("/proc/self/mountinfo")); id != "" {
		return id
	}
	return containerIDFromCgroup(readFileString("/proc/self/cgroup"))
}

func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// containerIDFromMountinfo extracts the container ID from the
// /var/lib/docker/containers/<id>/ host paths that appear in mountinfo lines.
func containerIDFromMountinfo(mountinfo string) string {
	for _, line := range strings.Split(mountinfo, "\n") {
		idx := strings.Index(line, "/containers/")
		if idx < 0 {
			continue
		}
		if id := containerIDRe.FindString(line[idx:]); id != "" {
			return id
		}
	}
	return ""
}

// containerIDFromCgroup extracts the container ID from cgroup v1 paths such as
// "12:devices:/docker/<id>" or ".../docker-<id>.scope".
func containerIDFromCgroup(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		if id := containerIDRe.FindString(line); id != "" {
			return id
		}
	}
	return ""
}

// sameContainer reports whether a and b refer to the same container, tolerating
// one being a short-ID prefix of the other (Docker's list/inspect return full
// IDs, but env-supplied overrides may be short).
func sameContainer(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// selfUpdate replaces docker-updater's own container by handing the swap to a
// detached helper container built from newImage. It must not stop or remove its
// own container inline -- that would kill this process before the replacement
// exists. It returns once the helper is running; the helper then recreates us.
func selfUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, newImage string) error {
	log.Printf("container %s: update targets docker-updater itself; handing the swap to a detached helper", info.Name)

	self, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting own container %s: %w", info.Name, err)
	}

	binds := dockerSocketBinds(self)
	if len(binds) == 0 {
		return fmt.Errorf("cannot self-update %s: no Docker socket mount found on own container (the docker.sock bind the updater already uses must be present to give the helper Docker access)", info.Name)
	}

	helperName := info.Name + "-self-update"
	// Clear any helper stranded by a previously aborted attempt so the create
	// below cannot fail on a name conflict.
	_ = cli.ContainerRemove(ctx, helperName, container.RemoveOptions{Force: true})

	helperCfg := &container.Config{
		Image:  newImage,
		Cmd:    []string{finishSelfUpdateCommand, "--target", info.ID, "--name", info.Name, "--image", newImage},
		Labels: map[string]string{selfUpdateHelperLabel: "true"},
	}
	if dh := dockerHostEnv(self); dh != "" {
		helperCfg.Env = []string{"DOCKER_HOST=" + dh}
	}

	// A fresh HostConfig (not the parent's): the helper only needs the Docker
	// socket and the parent's network reachability to it. AutoRemove cleans the
	// one-shot up on exit; an empty RestartPolicy means "no", so the helper is
	// never revived after it finishes.
	helperHost := &container.HostConfig{
		Binds:      binds,
		AutoRemove: true,
	}
	if self.HostConfig != nil {
		helperHost.NetworkMode = self.HostConfig.NetworkMode
	}

	created, err := cli.ContainerCreate(ctx, helperCfg, helperHost, nil, nil, helperName)
	if err != nil {
		return fmt.Errorf("creating self-update helper for %s: %w", info.Name, err)
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting self-update helper for %s: %w", info.Name, err)
	}

	log.Printf("container %s: self-update helper %s started; it will recreate me on %s", info.Name, shortID(created.ID), newImage)
	return nil
}

// dockerSocketBinds returns bind specs referencing the Docker socket from the
// container's host config (or, failing that, its resolved mounts), so the helper
// can be given the same socket the updater already uses.
func dockerSocketBinds(inspect types.ContainerJSON) []string {
	var binds []string
	if inspect.HostConfig != nil {
		for _, b := range inspect.HostConfig.Binds {
			if strings.Contains(b, "docker.sock") {
				binds = append(binds, b)
			}
		}
	}
	if len(binds) > 0 {
		return binds
	}
	// Fall back to the resolved mount table for setups that record the socket
	// as a mount rather than a raw bind string.
	for _, m := range inspect.Mounts {
		if strings.Contains(m.Destination, "docker.sock") || strings.Contains(m.Source, "docker.sock") {
			bind := m.Source + ":" + m.Destination
			if !m.RW {
				bind += ":ro"
			}
			binds = append(binds, bind)
		}
	}
	return binds
}

// dockerHostEnv returns the container's DOCKER_HOST value, or "" if unset (the
// helper then defaults to /var/run/docker.sock).
func dockerHostEnv(inspect types.ContainerJSON) string {
	if inspect.Config == nil {
		return ""
	}
	for _, e := range inspect.Config.Env {
		if v, ok := strings.CutPrefix(e, "DOCKER_HOST="); ok {
			return v
		}
	}
	return ""
}

// finishSelfUpdate is the helper container's entrypoint. It recreates the
// original docker-updater container (--target) on the new image (--image),
// reusing recreateContainer so the same rollback and health-gate guarantees
// apply. It runs to completion then exits; the helper container is --rm'd.
func finishSelfUpdate(args []string) {
	fs := flag.NewFlagSet(finishSelfUpdateCommand, flag.ExitOnError)
	target := fs.String("target", "", "ID of the docker-updater container to replace")
	name := fs.String("name", "", "name of the docker-updater container")
	newImage := fs.String("image", "", "new image to run")
	_ = fs.Parse(args)

	if *target == "" || *newImage == "" {
		log.Fatal("finish-self-update: --target and --image are required")
	}

	cli, err := newDockerClient()
	if err != nil {
		log.Fatalf("finish-self-update: failed to create Docker client: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	info := ContainerInfo{ID: *target, Name: *name, Image: *newImage}
	if info.Name == "" {
		if inspect, err := cli.ContainerInspect(ctx, *target); err == nil {
			info.Name = strings.TrimPrefix(inspect.Name, "/")
		}
	}

	log.Printf("finish-self-update: recreating %s (%s) on %s", info.Name, shortID(*target), *newImage)
	if err := recreateContainer(ctx, cli, info, *newImage); err != nil {
		log.Fatalf("finish-self-update: %v", err)
	}
	log.Printf("finish-self-update: %s is now running %s", info.Name, *newImage)
}
