package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mirrorConfigJSON is what `docker compose -p mirror --project-directory
// /srv/mirror config --format json web` printed for a real file.
const mirrorConfigJSON = `{
  "name": "mirror",
  "networks": {"front": {"name": "mirror_front", "ipam": {}}},
  "services": {
    "web": {
      "cap_add": ["NET_ADMIN"],
      "command": ["nginx", "-g", "daemon off;"],
      "entrypoint": null,
      "environment": {"MODE": "prod", "TOKEN": "abc", "PASS": null},
      "image": "nginx:1.27",
      "labels": {"team": "core"},
      "networks": {"front": null},
      "ports": [
        {"mode": "ingress", "target": 80, "published": "8080", "protocol": "tcp"},
        {"mode": "ingress", "host_ip": "127.0.0.1", "target": 9090, "published": "9090", "protocol": "udp"}
      ],
      "restart": "unless-stopped",
      "tmpfs": ["/tmp:size=64m"],
      "user": "1000",
      "volumes": [
        {"type": "bind", "source": "/srv/mirror/data", "target": "/data", "read_only": true, "bind": {}},
        {"type": "volume", "source": "cache", "target": "/cache", "volume": {}},
        {"type": "volume", "target": "/anon", "volume": {}},
        {"type": "tmpfs", "target": "/run/scratch"}
      ]
    }
  },
  "volumes": {"cache": {"name": "mirror_cache"}}
}`

func mirrorProject(t *testing.T) composeProject {
	t.Helper()
	var p composeProject
	require.NoError(t, json.Unmarshal([]byte(mirrorConfigJSON), &p))
	return p
}

// nginxImage is the image config: it supplies the entrypoint, PATH, a label and a VOLUME.
func nginxImage() *container.Config {
	return &container.Config{
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
		Env:        []string{"PATH=/usr/local/sbin:/usr/bin", "NGINX_VERSION=1.27.0"},
		Labels:     map[string]string{"maintainer": "NGINX"},
		Volumes:    map[string]struct{}{"/var/cache/nginx": {}},
	}
}

// matchingContainer is what compose creates from mirrorConfigJSON.
func matchingContainer() types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
				CapAdd:        []string{"CAP_NET_ADMIN"},
				Tmpfs:         map[string]string{"/tmp": "size=64m"},
				Mounts:        []mount.Mount{{Type: mount.TypeTmpfs, Target: "/run/scratch"}},
				PortBindings: nat.PortMap{
					"80/tcp":   {{HostIP: "", HostPort: "8080"}},
					"9090/udp": {{HostIP: "127.0.0.1", HostPort: "9090"}},
				},
			},
		},
		Mounts: []types.MountPoint{
			{Type: mount.TypeBind, Source: "/srv/mirror/data", Destination: "/data", RW: false},
			{Type: mount.TypeVolume, Name: "mirror_cache", Destination: "/cache", RW: true},
			{Type: mount.TypeVolume, Name: strings.Repeat("ab", 32), Destination: "/anon", RW: true},
			{Type: mount.TypeVolume, Name: strings.Repeat("cd", 32), Destination: "/var/cache/nginx", RW: true},
			{Type: mount.TypeTmpfs, Destination: "/run/scratch", RW: true},
		},
		Config: &container.Config{
			Image:      "nginx:1.27",
			Entrypoint: []string{"/docker-entrypoint.sh"},
			Cmd:        []string{"nginx", "-g", "daemon off;"},
			User:       "1000",
			Env:        []string{"PATH=/usr/local/sbin:/usr/bin", "NGINX_VERSION=1.27.0", "MODE=prod", "TOKEN=abc", "PASS=from-shell"},
			Labels: map[string]string{
				"maintainer":                 "NGINX",
				"team":                       "core",
				"com.docker.compose.project": "mirror",
			},
		},
		NetworkSettings: &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{"mirror_front": {}}},
	}
}

func TestComposeDifferencesMatchingContainerHasNone(t *testing.T) {
	diffs, err := composeDifferences(mirrorProject(t), "web", "/srv/mirror",matchingContainer(), nginxImage())
	require.NoError(t, err)
	assert.Empty(t, diffs, "image defaults, compose labels, anonymous image volumes and pass-through env are not differences")
}

func TestComposeDifferencesNamesEachField(t *testing.T) {
	c := matchingContainer()
	c.Config.Image = "nginx:1.26"
	c.Config.Env = append(c.Config.Env[:3], "TOKEN=s3cret", "OLD_FLAG=1")
	c.Config.Labels["old"] = "x"
	c.Mounts[0].RW = true
	c.Mounts = append(c.Mounts, types.MountPoint{Type: mount.TypeBind, Source: "/srv/legacy", Destination: "/legacy", RW: true})
	delete(c.HostConfig.PortBindings, "9090/udp")
	c.NetworkSettings.Networks = map[string]*network.EndpointSettings{"mirror_default": {}}
	c.HostConfig.RestartPolicy = container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}

	diffs, err := composeDifferences(mirrorProject(t), "web", "/srv/mirror",c, nginxImage())
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "image", File: "nginx:1.27", Container: "nginx:1.26"},
		{Field: "environment TOKEN", File: "set (value hidden)", Container: "a different value"},
		{Field: "environment OLD_FLAG", File: none, Container: "set"},
		{Field: "label old", File: none, Container: "x"},
		{Field: "mount /data", File: "bind /srv/mirror/data (read-only)", Container: "bind /srv/mirror/data"},
		{Field: "mount /legacy", File: none, Container: "bind /srv/legacy"},
		{Field: "ports", File: "127.0.0.1:9090->9090/udp, 8080->80/tcp", Container: "8080->80/tcp"},
		{Field: "networks", File: "mirror_front", Container: "mirror_default"},
		{Field: "restart", File: "unless-stopped", Container: "on-failure:3"},
	}, diffs)

	for _, d := range diffs {
		assert.NotContains(t, d.File+d.Container, "s3cret", "an environment value must never reach the dashboard")
		assert.NotContains(t, d.File+d.Container, "abc")
	}
}

func TestComposeDifferencesUnknownService(t *testing.T) {
	_, err := composeDifferences(mirrorProject(t), "db", "/srv/mirror",matchingContainer(), nginxImage())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no service db")
}

// A service with no networks key joins the project's default network.
func TestComposeDifferencesDefaultNetwork(t *testing.T) {
	p := mirrorProject(t)
	svc := p.Services["web"]
	svc.Networks = nil
	p.Services["web"] = svc
	c := matchingContainer()
	c.NetworkSettings.Networks = map[string]*network.EndpointSettings{"mirror_default": {}}

	diffs, err := composeDifferences(p, "web", "/srv/mirror", c, nginxImage())
	require.NoError(t, err)
	assert.Empty(t, diffs)
}
