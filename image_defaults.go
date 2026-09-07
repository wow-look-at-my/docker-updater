package main

import (
	"context"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/wow-look-at-my/go-containers/set"
)

// clearInheritedImageDefaults removes from a container's config every value the
// image it was created from supplied.
//
// ContainerInspect reports a container's RESOLVED config. The API records the
// entrypoint, command, environment, labels, healthcheck, ports and volumes the
// container runs with, and records nothing about where each one came from. An
// operator's own --entrypoint and the image's ENTRYPOINT read identically.
//
// So handing that config back to ContainerCreate with only Image changed pins
// the OLD image's defaults onto the NEW one, and a default the new image
// changed never takes effect. A field cleared here is nil or empty, which is
// how ContainerCreate is told to take the image's own value.
//
// The one thing this cannot preserve is an operator who typed a value that
// happens to equal the old image's. That is indistinguishable from inheritance
// through this API, and inheriting is the safer of the two readings: it tracks
// the image the operator asked to run.
func clearInheritedImageDefaults(config, img *container.Config) {
	if config == nil || img == nil {
		return
	}

	if equalStrings(config.Entrypoint, img.Entrypoint) {
		config.Entrypoint = nil
	}
	if equalStrings(config.Cmd, img.Cmd) {
		config.Cmd = nil
	}
	if config.User == img.User {
		config.User = ""
	}
	if config.WorkingDir == img.WorkingDir {
		config.WorkingDir = ""
	}
	if config.StopSignal == img.StopSignal {
		config.StopSignal = ""
	}
	if equalHealthcheck(config.Healthcheck, img.Healthcheck) {
		config.Healthcheck = nil
	}

	// The remaining fields are unions rather than replacements: the daemon adds
	// the image's entries to the ones the operator gave. Each drops only the
	// entries that match the old image, so an operator's own survive.
	config.Env = dropMatching(config.Env, img.Env)
	for k, v := range img.Labels {
		if config.Labels[k] == v {
			delete(config.Labels, k)
		}
	}
	for p := range img.ExposedPorts {
		delete(config.ExposedPorts, p)
	}
	for v := range img.Volumes {
		delete(config.Volumes, v)
	}
}

// clearInheritedDefaultsFor looks up the image a container was created from and
// clears what that image supplied.
//
// A missing old image (pruned, or a daemon that will not answer) leaves the
// config as inspected, which is the behavior that predates this function: the
// update still runs, and at worst carries a default forward. Saying so at error
// level is the point -- an update that quietly pins an old image's entrypoint is
// a container that will not start, and the log line is the only place that shows.
func clearInheritedDefaultsFor(ctx context.Context, cli DockerClient, config *container.Config, imageID, name string) {
	if config == nil || imageID == "" {
		return
	}
	img, _, err := cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		log.Printf("ERROR container %s: cannot inspect its current image %s, so its config keeps that image's entrypoint, command and environment; the new image's own will not apply: %v", name, shortID(imageID), err)
		return
	}
	clearInheritedImageDefaults(config, img.Config)
}

// exitNotExecutable is what the kernel reports when it cannot exec the
// entrypoint. A cosmopolitan binary reaches it whenever the entrypoint names the
// binary rather than the shell launcher that starts it.
const exitNotExecutable = 126

// lastExitCode reports how a container died, or a negative value when that
// cannot be read. The caller uses it to tell a dead entrypoint apart from an
// application that started and failed its health check.
func lastExitCode(ctx context.Context, cli DockerClient, id string) int {
	inspect, err := cli.ContainerInspect(ctx, id)
	if err != nil || inspect.State == nil {
		return -1
	}
	return inspect.State.ExitCode
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalHealthcheck(a, b *container.HealthConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalStrings(a.Test, b.Test) &&
		a.Interval == b.Interval &&
		a.Timeout == b.Timeout &&
		a.StartPeriod == b.StartPeriod &&
		a.StartInterval == b.StartInterval &&
		a.Retries == b.Retries
}

// dropMatching returns entries of a that b does not also carry, order preserved.
func dropMatching(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return a
	}
	remove := set.New[string]()
	for _, e := range b {
		remove.Add(e)
	}
	kept := make([]string, 0, len(a))
	for _, e := range a {
		if remove.Contains(e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}
