package main

import (
	"context"
	"fmt"
)

// checkImageUpdate pulls the latest image and compares digests. Returns the
// new digest if an update is available, or empty string if already up-to-date.
func checkImageUpdate(ctx context.Context, cli *dockerClient, info ContainerInfo) (newDigest string, err error) {
	if err := cli.pullImage(ctx, info.Image); err != nil {
		return "", fmt.Errorf("checking image update for %s: %w", info.Image, err)
	}

	inspect, err := cli.inspectImage(ctx, info.Image)
	if err != nil {
		return "", fmt.Errorf("inspecting image %s: %w", info.Image, err)
	}

	if inspect.ID != info.ImageDigest {
		return inspect.ID, nil
	}

	return "", nil
}
