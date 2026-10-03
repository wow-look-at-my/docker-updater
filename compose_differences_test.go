package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compose created the fixture container. createCapture recorded both requests, before and after the file edit.
func captureFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/compose-capture/" + name)
	require.NoError(t, err)
	return data
}

func capturedContainer(t *testing.T) types.ContainerJSON {
	t.Helper()
	var c types.ContainerJSON
	require.NoError(t, json.Unmarshal(captureFixture(t, "container.json"), &c))
	return c
}

func capturedImage(t *testing.T) *container.Config {
	t.Helper()
	var img container.Config
	require.NoError(t, json.Unmarshal(captureFixture(t, "image.json"), &img))
	return &img
}

// editRequest decodes the in-sync request, lets edit change it, and encodes it again.
func editRequest(t *testing.T, edit func(req map[string]any)) []byte {
	t.Helper()
	req, err := decodeObject(captureFixture(t, "create-insync.json"))
	require.NoError(t, err)
	edit(req)
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return data
}

func TestDifferencesInSyncContainerHasNone(t *testing.T) {
	diffs, err := composeDifferences(captureFixture(t, "create-insync.json"), capturedContainer(t), capturedImage(t))
	require.NoError(t, err)
	assert.Empty(t, diffs, "daemon defaults, image env and labels, and compose's own labels are not differences")
}

// The case that listed no difference before: every changed field is one
// nothing named, and all of them appear without a rule of their own.
func TestDifferencesEditedFileListsEveryChangedField(t *testing.T) {
	diffs, err := composeDifferences(captureFixture(t, "create-edited.json"), capturedContainer(t), capturedImage(t))
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "HostConfig.MaskedPaths", File: "[]", Container: `["/proc/acpi","/proc/asound","/proc/interrupts","/proc/kcore","/proc/keys","/proc/latency_stats","/proc/sched_debug","/proc/scsi","/proc/timer_list","/proc/timer_stats","/sys/devices/virtual/powercap","/sys/firmware"]`},
		{Field: "HostConfig.Memory", File: "25769803776", Container: none},
		{Field: "HostConfig.MemorySwap", File: "25769803776", Container: none},
		{Field: "HostConfig.PidsLimit", File: "8192", Container: none},
		{Field: "HostConfig.ReadonlyPaths", File: "[]", Container: `["/proc/bus","/proc/fs","/proc/irq","/proc/sys","/proc/sysrq-trigger"]`},
		{Field: "HostConfig.SecurityOpt", File: `["seccomp={\"defaultAction\":\"SCMP_ACT_ALLOW\",\"syscalls\":[]}"]`, Container: none},
	}, diffs)
}

// A key the file drops still shows: the request sends zero, and the container
// keeps the value.
func TestDifferencesRemovedKey(t *testing.T) {
	c := capturedContainer(t)
	c.HostConfig.Memory = 1 << 30
	c.HostConfig.CapAdd = []string{"NET_ADMIN"}

	diffs, err := composeDifferences(captureFixture(t, "create-insync.json"), c, capturedImage(t))
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "HostConfig.CapAdd", File: none, Container: `["NET_ADMIN"]`},
		{Field: "HostConfig.Memory", File: none, Container: "1073741824"},
	}, diffs)
}

func TestDifferencesEnvLabelsAndNetworks(t *testing.T) {
	c := capturedContainer(t)
	c.Config.Env = []string{"MODE=dev", "PATH=/bin", "TOKEN=s3cret", "EMPTY="}
	delete(c.Config.Labels, "team")
	c.Config.Labels["old"] = "x"
	c.NetworkSettings.Networks["legacy"] = &network.EndpointSettings{}
	c.NetworkSettings.Networks["e2e_default"].Aliases = []string{"runner"}
	c.NetworkSettings.Networks["e2e_default"].IPAddress = "172.18.0.2"

	diffs, err := composeDifferences(captureFixture(t, "create-insync.json"), c, capturedImage(t))
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "Config.Env EMPTY", File: none, Container: `""`},
		{Field: "Config.Env MODE", File: `"prod"`, Container: `"dev"`},
		{Field: "Config.Env TOKEN", File: none, Container: `"s3cret"`},
		{Field: "Config.Labels old", File: none, Container: "x"},
		{Field: "Config.Labels team", File: "core", Container: none},
		{Field: "network e2e_default Aliases", File: `["e2e-runner-1","runner"]`, Container: `["runner"]`},
		{Field: "network legacy", File: none, Container: "joined"},
	}, diffs)
}

// A field no rule names is compared too, so a new compose or Docker field needs no code.
func TestDifferencesUnnamedField(t *testing.T) {
	req := editRequest(t, func(req map[string]any) {
		req["HostConfig"].(map[string]any)["OomScoreAdj"] = json.Number("500")
		req["Tty"] = true
	})
	diffs, err := composeDifferences(req, capturedContainer(t), capturedImage(t))
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "Config.Tty", File: "true", Container: none},
		{Field: "HostConfig.OomScoreAdj", File: "500", Container: none},
	}, diffs)
}

// The image supplies a command the request leaves empty, but not past an entrypoint the request names.
func TestDifferencesImageCommand(t *testing.T) {
	img := capturedImage(t)
	img.Cmd = []string{"/bin/run", "--serve"}
	req := editRequest(t, func(req map[string]any) { req["Cmd"] = nil })
	diffs, err := composeDifferences(req, capturedContainer(t), img)
	require.NoError(t, err)
	assert.Empty(t, diffs)

	req = editRequest(t, func(req map[string]any) {
		req["Cmd"] = nil
		req["Entrypoint"] = []any{"/bin/run"}
	})
	diffs, err = composeDifferences(req, capturedContainer(t), img)
	require.NoError(t, err)
	assert.Equal(t, []ComposeDifference{
		{Field: "Config.Cmd", File: none, Container: `["/bin/run","--serve"]`},
		{Field: "Config.Entrypoint", File: `["/bin/run"]`, Container: none},
	}, diffs)
}

// A seccomp profile is kilobytes, and it shows in full.
func TestDifferencesLongValueShowsInFull(t *testing.T) {
	profile := "seccomp={" + strings.Repeat(`"x":1,`, 40) + `"y":2}`
	req := editRequest(t, func(req map[string]any) {
		req["HostConfig"].(map[string]any)["SecurityOpt"] = []any{profile}
	})
	c := capturedContainer(t)
	c.HostConfig.SecurityOpt = []string{strings.Replace(profile, `"y":2`, `"y":3`, 1)}

	diffs, err := composeDifferences(req, c, capturedImage(t))
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "HostConfig.SecurityOpt", diffs[0].Field)
	assert.Equal(t, canonical([]any{profile}), diffs[0].File)
	assert.Equal(t, canonical([]any{c.HostConfig.SecurityOpt[0]}), diffs[0].Container)
}

// A file with no restart key sends an empty policy, and the daemon stores "no".
func TestDifferencesDefaultRestartPolicy(t *testing.T) {
	req := editRequest(t, func(req map[string]any) {
		req["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "", "MaximumRetryCount": json.Number("0")}
	})
	c := capturedContainer(t)
	c.HostConfig.RestartPolicy = container.RestartPolicy{Name: "no"}

	diffs, err := composeDifferences(req, c, capturedImage(t))
	require.NoError(t, err)
	assert.Empty(t, diffs)
}

func TestDifferencesRejectsABadRequest(t *testing.T) {
	_, err := composeDifferences([]byte("not json"), capturedContainer(t), capturedImage(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the create request")
}
