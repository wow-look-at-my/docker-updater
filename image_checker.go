package main

import (
	"context"
	"fmt"
)

// checkImageUpdate pulls the latest image and compares digests. Returns the new
// digest if an update is available (empty string if already up-to-date) and
// whether the pull actually fetched new content (as opposed to confirming the
// local image was already current).
func checkImageUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, resolveAuth AuthResolver) (newDigest string, fetched bool, err error) {
	latestDigest, fetched, err := pullImage(ctx, cli, info.Image, resolveAuth)
	if err != nil {
		return "", false, fmt.Errorf("checking image update for %s: %w", info.Image, err)
	}

	if latestDigest != info.ImageDigest {
		return latestDigest, fetched, nil
	}

	return "", fetched, nil
}
