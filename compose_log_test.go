package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureComposeLog redirects the compose child-output stream to a buffer for
// the duration of a test, restoring the real writer afterwards.
func captureComposeLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	old := composeLogWriter
	composeLogWriter = &buf
	t.Cleanup(func() { composeLogWriter = old })
	return &buf
}

func TestRunLoggedCommandFailureErrorCarriesOutputTail(t *testing.T) {
	t.Serial()
	logged := captureComposeLog(t)

	err := runLoggedCommand(context.Background(), "sh",
		[]string{"-c", "echo building step 1; echo 'mkdir /tmp/dockerfile123: no such file or directory' >&2; exit 1"})

	require.Error(t, err)
	// The error names the command, the exit status, AND the actual cause from
	// the child's output -- never a bare "exit status 1".
	assert.Contains(t, err.Error(), "sh -c")
	assert.Contains(t, err.Error(), "exit status 1")
	assert.Contains(t, err.Error(), "mkdir /tmp/dockerfile123: no such file or directory")
	assert.Contains(t, err.Error(), "building step 1", "stdout and stderr are both captured")

	// The full output also streamed to the updater's own log.
	assert.Contains(t, logged.String(), "building step 1")
	assert.Contains(t, logged.String(), "no such file or directory")
}

func TestRunLoggedCommandSuccessStreamsOutputWithoutError(t *testing.T) {
	t.Serial()
	logged := captureComposeLog(t)

	err := runLoggedCommand(context.Background(), "sh", []string{"-c", "echo pulling base; echo built ok"})

	require.NoError(t, err)
	assert.Contains(t, logged.String(), "pulling base")
	assert.Contains(t, logged.String(), "built ok")
}

func TestRunLoggedCommandFailureWithNoOutputSaysSo(t *testing.T) {
	t.Serial()
	captureComposeLog(t)

	err := runLoggedCommand(context.Background(), "sh", []string{"-c", "exit 3"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 3")
	assert.Contains(t, err.Error(), "(no output)")
}

func TestRunLoggedCommandTailTrimmedToLastLines(t *testing.T) {
	t.Serial()
	captureComposeLog(t)

	// 60 numbered lines, then fail: only the last composeErrTailLines lines may
	// appear in the error.
	err := runLoggedCommand(context.Background(), "sh",
		[]string{"-c", "i=1; while [ $i -le 60 ]; do echo line-$i; i=$((i+1)); done; exit 1"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "line-60", "the final lines are kept")
	assert.Contains(t, err.Error(), "line-41", "exactly the last 20 lines are kept")
	assert.NotContains(t, err.Error(), "line-40\n", "older lines are trimmed")
	assert.NotContains(t, err.Error(), "line-1\n", "the head of the output is trimmed")
}

func TestTailBufferCapsRetainedBytes(t *testing.T) {
	t.Serial()
	tb := &tailBuffer{max: 16}
	_, err := tb.Write([]byte("0123456789"))
	require.NoError(t, err)
	_, err = tb.Write([]byte("abcdefghijklmnop"))
	require.NoError(t, err)

	assert.Equal(t, "abcdefghijklmnop", tb.tail(composeErrTailLines),
		"only the final max bytes are retained")
}

func TestRunDockerComposeErrorMentionsDockerCommand(t *testing.T) {
	t.Serial()
	captureComposeLog(t)

	// No real docker CLI needed: whether the binary is missing or the args are
	// rejected, the error must identify the full docker command line.
	err := runDockerCompose(context.Background(), composeArgs(composeTarget{ConfigFiles: []string{"/srv/demo/docker-compose.yml"}, WorkingDir: "/srv/demo", Project: "demo"}, "build", "--pull", "opencode"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "docker compose -f /srv/demo/docker-compose.yml --project-directory /srv/demo -p demo build --pull opencode")
}

// A compose file the updater cannot see is the containerized-updater footgun:
// the label carries a host path, and without a matching bind mount the docker
// CLI fails with a bare "no such file or directory". Fail before exec'ing with
// an error that names the actual fix.
func TestComposeRunnerRejectsInvisibleComposeFile(t *testing.T) {
	t.Serial()
	captureComposeLog(t)
	missing := "/mnt/ssdpool/appdata/compose.manager/claude-host/docker-compose.yml"

	err := execComposeRunner{}.UpNoDeps(context.Background(), composeTarget{
		ConfigFiles: []string{missing},
		WorkingDir:  "/mnt/ssdpool/appdata/compose.manager/claude-host",
		Project:     "claude-host",
		Service:     "dind",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
	assert.Contains(t, err.Error(), "not visible to docker-updater")
	assert.Contains(t, err.Error(), "-v /mnt/ssdpool/appdata/compose.manager/claude-host:/mnt/ssdpool/appdata/compose.manager/claude-host:ro")
	assert.NotContains(t, err.Error(), "exit status", "the docker CLI must never be reached")
}

func TestCheckComposeFilesVisiblePassesForReadableFiles(t *testing.T) {
	t.Serial()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(path, []byte("services: {}\n"), 0o644))

	assert.NoError(t, checkComposeFilesVisible([]string{path}))
	assert.NoError(t, checkComposeFilesVisible(nil))
}
