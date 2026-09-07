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

	// The entrypoint always comes from the new image. An image owns the binary
	// it starts, and that binary changes with the image: buildhost moved from a
	// bare executable to a shell launcher, and every container still carrying
	// the old spelling could not exec at all. Diffing against the previous image
	// does not catch that, because a value inherited from an older ancestor
	// matches no later image and is therefore pinned for good.
	config.Entrypoint = nil

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

// resetToImageDefaults strips the process fields from config so the new image's
// own apply, and reports whether it had anything to strip.
//
// clearInheritedImageDefaults is the first line and it is not always enough. It
// clears a field only when the container's value EQUALS the old image's, so a
// value it cannot attribute survives: the old image is gone from the daemon, or
// an operator once typed the path by hand, or the container predates the image
// pair entirely. A surviving path that the new image does not carry is an exec
// failure on every start, and a rolling updater clones it onto the container
// after this one, so the deployment never moves again. One buildhost deployment
// spent three weeks on that loop.
//
// The retry that calls this gives up the operator's own overrides for the
// image's. That is the lesser loss: a container that starts on the image's
// defaults is running the version it was asked to run, and one that cannot exec
// its entrypoint is running the previous version forever.
func resetToImageDefaults(config *container.Config) bool {
	if config == nil {
		return false
	}
	changed := len(config.Entrypoint) > 0 || len(config.Cmd) > 0 ||
		config.User != "" || config.WorkingDir != "" || config.Healthcheck != nil
	config.Entrypoint = nil
	config.Cmd = nil
	config.User = ""
	config.WorkingDir = ""
	config.Healthcheck = nil
	return changed
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
