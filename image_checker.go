package main

import (
	"context"
	"fmt"
)

// checkImageUpdate pulls the latest image and compares digests. Returns the
// new digest if an update is available, or empty string if already up-to-date.
func checkImageUpdate(ctx context.Context, cli DockerClient, info ContainerInfo, resolveAuth AuthResolver) (newDigest string, err error) {
	latestDigest, err := pullImage(ctx, cli, info.Image, resolveAuth)
	if err != nil {
		return "", fmt.Errorf("checking image update for %s: %w", info.Image, err)
	}

	if latestDigest != info.ImageDigest {
		return latestDigest, nil
	}

	return "", nil
}
