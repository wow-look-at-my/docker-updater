package main

import (
	"context"
	"log"
	"slices"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// securityOptLabel names the security options an image needs, one --security-opt value per whitespace-separated word.
const securityOptLabel = "docker-updater.security-opt"

// withImageSecurityOpts returns hostConfig plus the options newImage asks for
// in its securityOptLabel. It never changes hostConfig itself, because the
// rollback recreates the version with the original.
//
// Only a container that binds the Docker socket gets them. Such a container
// is already root on the host, so a lifted seccomp filter or /proc mask adds
// nothing. For any other container the label would be an escalation, so it is
// refused at error level and the update goes on without it.
func withImageSecurityOpts(ctx context.Context, cli DockerClient, inspect types.ContainerJSON, newImage, name string) *container.HostConfig {
	hostConfig := inspect.HostConfig
	if hostConfig == nil {
		return hostConfig
	}
	img, _, err := cli.ImageInspectWithRaw(ctx, newImage)
	if err != nil {
		log.Printf("ERROR container %s: cannot inspect new image %s, so any %s it declares does not apply: %v", name, newImage, securityOptLabel, err)
		return hostConfig
	}
	if img.Config == nil {
		return hostConfig
	}
	wanted := strings.Fields(img.Config.Labels[securityOptLabel])
	var missing []string
	for _, opt := range wanted {
		if !slices.Contains(hostConfig.SecurityOpt, opt) {
			missing = append(missing, opt)
		}
	}
	if len(missing) == 0 {
		return hostConfig
	}
	if len(dockerSocketBinds(inspect)) == 0 {
		log.Printf("ERROR container %s: image %s asks for --security-opt %s, refused: only a container that binds the Docker socket may lift its own confinement", name, newImage, strings.Join(missing, " "))
		return hostConfig
	}
	updated := *hostConfig
	updated.SecurityOpt = append(slices.Clone(hostConfig.SecurityOpt), missing...)
	log.Printf("container %s: adding --security-opt %s, from the %s label of %s", name, strings.Join(missing, " "), securityOptLabel, newImage)
	return &updated
}
