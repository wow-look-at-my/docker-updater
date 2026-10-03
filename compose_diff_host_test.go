package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runnerConfigJSON is what `docker compose config --format json runner` printed
// for a service with these keys. Byte counts come out as quoted integers.
const runnerConfigJSON = `{
  "name": "webhooks",
  "networks": {"default": {"name": "webhooks_default", "ipam": {}}},
  "services": {
    "runner": {
      "command": null,
      "entrypoint": null,
      "image": "runner:latest",
      "networks": {"default": null},
      "security_opt": ["seccomp=./bwrap-seccomp.json", "systempaths=unconfined", "label:disable"],
      "mem_limit": "25769803776",
      "memswap_limit": "25769803776",
      "pids_limit": 8192,
      "shm_size": "268435456",
      "cpus": 1.5,
      "init": true,
      "ulimits": {"nofile": {"soft": 1024, "hard": 2048}, "nproc": 512},
      "sysctls": {"net.core.somaxconn": "1024"},
      "devices": [{"source": "/dev/fuse", "target": "/dev/fuse", "permissions": "rwm"}],
      "extra_hosts": ["host.docker.internal=host-gateway"],
      "stop_grace_period": "1m0s",
      "logging": {"driver": "json-file", "options": {"max-size": "10m"}}
    }
  }
}`

// The profile on disk carries whitespace. Compose compacts it before it hands it to the daemon.
const runnerSeccompFile = "{\"defaultAction\": \"SCMP_ACT_ALLOW\",\n  \"syscalls\": []}\n"

func runnerProject(t *testing.T) (composeProject, string) {
	t.Helper()
	var p composeProject
	require.NoError(t, json.Unmarshal([]byte(runnerConfigJSON), &p))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bwrap-seccomp.json"), []byte(runnerSeccompFile), 0o644))
	return p, dir
}

// runnerContainer is what compose creates from runnerConfigJSON.
func runnerContainer() types.ContainerJSON {
	pids, init, grace := int64(8192), true, 60
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "no"},
				SecurityOpt:   []string{`seccomp={"defaultAction":"SCMP_ACT_ALLOW","syscalls":[]}`, "label:disable"},
				MaskedPaths:   []string{},
				ReadonlyPaths: []string{},
				ShmSize:       268435456,
				Init:          &init,
				Sysctls:       map[string]string{"net.core.somaxconn": "1024"},
				ExtraHosts:    []string{"host.docker.internal:host-gateway"},
				LogConfig:     container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m"}},
				Resources: container.Resources{
					Memory:     25769803776,
					MemorySwap: 25769803776,
					NanoCPUs:   1500000000,
					PidsLimit:  &pids,
					Ulimits:    []*container.Ulimit{{Name: "nproc", Soft: 512, Hard: 512}, {Name: "nofile", Soft: 1024, Hard: 2048}},
					Devices:    []container.DeviceMapping{{PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse", CgroupPermissions: "rwm"}},
				},
			},
		},
		Config: &container.Config{
			Image:       "runner:latest",
			Hostname:    "0123456789ab",
			StopTimeout: &grace,
		},
		NetworkSettings: &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{"webhooks_default": {}}},
	}
}

func TestHostDifferencesMatchingContainerHasNone(t *testing.T) {
	p, dir := runnerProject(t)
	diffs, err := composeDifferences(p, "runner", dir, runnerContainer(), &container.Config{})
	require.NoError(t, err)
	assert.Empty(t, diffs, "an inlined seccomp profile, empty system path masks and daemon defaults are not differences")
}

// A container created before the file gained its security and resource keys
// drifts. Before these fields, the list came back empty for it.
func TestHostDifferencesContainerFromBeforeTheEdit(t *testing.T) {
	p, dir := runnerProject(t)
	c := runnerContainer()
	c.HostConfig.SecurityOpt = []string{"label:disable"}
	c.HostConfig.MaskedPaths = []string{"/proc/kcore"}
	c.HostConfig.ReadonlyPaths = []string{"/proc/sys"}
	c.HostConfig.Memory, c.HostConfig.MemorySwap, c.HostConfig.PidsLimit = 0, 0, nil

	diffs, err := composeDifferences(p, "runner", dir, c, &container.Config{})
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "security_opt", File: "label:disable, seccomp profile f09d9622987b, systempaths=unconfined", Container: "label:disable"},
		{Field: "mem_limit", File: "24GiB", Container: none},
		{Field: "memswap_limit", File: "24GiB", Container: none},
		{Field: "pids_limit", File: "8192", Container: none},
	}, diffs)
}

func TestHostDifferencesSeccompProfileChanged(t *testing.T) {
	p, dir := runnerProject(t)
	c := runnerContainer()
	c.HostConfig.SecurityOpt[0] = `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`

	diffs, err := composeDifferences(p, "runner", dir, c, &container.Config{})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "security_opt", diffs[0].Field)
	assert.NotEqual(t, diffs[0].File, diffs[0].Container, "a different profile body must read as a difference")
}

func TestHostDifferencesUnreadableSeccompProfile(t *testing.T) {
	p, _ := runnerProject(t)
	diffs, err := composeDifferences(p, "runner", t.TempDir(), runnerContainer(), &container.Config{})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Contains(t, diffs[0].File, "seccomp profile ./bwrap-seccomp.json (cannot read:")
}

// deploy.resources.limits wins over the top-level keys, as it does in compose.
func TestHostDifferencesDeployLimits(t *testing.T) {
	p, dir := runnerProject(t)
	svc := p.Services["runner"]
	require.NoError(t, json.Unmarshal([]byte(`{"resources": {"limits": {"cpus": 0.5, "memory": "536870912", "pids": 100}, "reservations": {"memory": "134217728"}}}`), &svc.Deploy))
	p.Services["runner"] = svc

	diffs, err := composeDifferences(p, "runner", dir, runnerContainer(), &container.Config{})
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "mem_limit", File: "512MiB", Container: "24GiB"},
		{Field: "mem_reservation", File: "128MiB", Container: none},
		{Field: "cpus", File: "0.5", Container: "1.5"},
		{Field: "pids_limit", File: "100", Container: "8192"},
	}, diffs)
}

// The daemon stores a default /dev/shm size and twice the memory as swap when the file sets neither.
func TestHostDifferencesDaemonDefaults(t *testing.T) {
	p, dir := runnerProject(t)
	svc := p.Services["runner"]
	svc.ShmSize, svc.MemswapLimit = 0, 0
	p.Services["runner"] = svc
	c := runnerContainer()
	c.HostConfig.ShmSize = dockerDefaultShmSize
	c.HostConfig.MemorySwap = 2 * c.HostConfig.Memory

	diffs, err := composeDifferences(p, "runner", dir, c, &container.Config{})
	require.NoError(t, err)
	assert.Empty(t, diffs)
}
