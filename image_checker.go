package main

import (
	"context"
	"fmt"
	"strings"
)

// checkImageUpdate pulls the latest image and compares digests. Returns the new
// digest if an update is available (empty string if already up-to-date) and
// whether the pull actually fetched new content (as opposed to confirming the
// local image was already current).
func checkImageUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, resolveAuth AuthResolver) (newDigest string, fetched bool, err error) {
	latestDigest, fetched, err := pullImage(ctx, cli, info.Image, resolveAuth)
	if err != nil {
		return "", false, pullFailure(info.Image, err)
	}

	if latestDigest != info.ImageDigest {
		return latestDigest, fetched, nil
	}

	return "", fetched, nil
}

// pulledCommit reads the commit the image now under imageName was built from.
// It runs after a pull, so the name resolves to the fetched image. An image
// with no such labels, or one the daemon cannot inspect, yields nothing: the
// digest still names the update.
func pulledCommit(ctx context.Context, cli DockerClient, imageName string) (sha, url string) {
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil || inspect.Config == nil {
		return "", ""
	}
	return commitOf(inspect.Config.Labels)
}

// pullFailure reports a failed pull with the daemon's own reason. Only when the
// registry says the repository or manifest does not exist is build mode a
// remedy: a local compose `build:` tag is not in any registry. Every other
// failure (no such host, TLS, 401, 5xx) names a reachable registry that needs a
// different fix.
func pullFailure(ref string, err error) error {
	msg := err.Error()
	for _, absent := range []string{"repository does not exist", "manifest unknown", "not found"} {
		if strings.Contains(msg, absent) {
			return fmt.Errorf("image %s is not in its registry: %w; if it is built locally, set docker-updater.mode=build", ref, err)
		}
	}
	return fmt.Errorf("checking image update for %s: %w", ref, err)
}
