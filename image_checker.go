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
