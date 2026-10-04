package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// daemonLog is what a fake daemon received.
type daemonLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *daemonLog) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

// fakeDaemon serves a Docker API on a unix socket and points DOCKER_HOST at it.
func fakeDaemon(t *testing.T) *daemonLog {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	log := &daemonLog{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.seen = append(log.seen, r.Method+" "+r.URL.Path)
		log.mu.Unlock()
		io.WriteString(w, `{"from":"daemon"}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	return log
}

// fakeCompose stands in for `docker compose`. It reads through the socket it is given, as the real one does, then sends the requests in its script.
// It returns the compose file and the directory where the fake writes its arguments.
func fakeCompose(t *testing.T, requests string) (string, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "compose.yml")
	require.NoError(t, os.WriteFile(file, []byte("services: {}\n"), 0o644))
	dir := fakeDockerOnPath(t, `s="${DOCKER_HOST#unix://}"
curl -sf --unix-socket "$s" http://d/v1.47/containers/web/json > /dev/null || { echo "read failed" >&2; exit 1; }
`+requests)
	return file, dir
}

func TestCreateRequestCapturesTheCreateBody(t *testing.T) {
	daemon := fakeDaemon(t)
	file, _ := fakeCompose(t, `curl -s --unix-socket "$s" -X POST -d '{"Image":"web:2"}' http://d/v1.47/containers/create?name=x
echo "Error response from daemon: captured" >&2; exit 1`)

	body, err := execComposeRunner{}.CreateRequest(context.Background(), composeTarget{ConfigFiles: []string{file}, Service: "web"})

	require.NoError(t, err)
	assert.JSONEq(t, `{"Image":"web:2"}`, string(body))
	assert.Equal(t, []string{"GET /v1.47/containers/web/json"}, daemon.requests(), "reads reach the daemon, and the create never does")
}

// A write before the create never reaches the daemon, and the capture says which write compose wanted.
func TestCreateRequestRefusesOtherWrites(t *testing.T) {
	daemon := fakeDaemon(t)
	file, _ := fakeCompose(t, `curl -s --unix-socket "$s" -X POST http://d/v1.47/containers/dep/stop
echo "Error response from daemon: refused" >&2; exit 1`)

	_, err := execComposeRunner{}.CreateRequest(context.Background(), composeTarget{ConfigFiles: []string{file}, Service: "web"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compose wanted to POST /v1.47/containers/dep/stop before it created the container")
	assert.Equal(t, []string{"GET /v1.47/containers/web/json"}, daemon.requests())
}

func TestCreateRequestReportsComposeFailure(t *testing.T) {
	fakeDaemon(t)
	file, _ := fakeCompose(t, `echo "no such service: web" >&2; exit 1`)

	_, err := execComposeRunner{}.CreateRequest(context.Background(), composeTarget{ConfigFiles: []string{file}, Service: "web"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such service: web")
}

func TestCreateRequestRunsTheRecreate(t *testing.T) {
	fakeDaemon(t)
	file, dir := fakeCompose(t, `curl -s --unix-socket "$s" -X POST -d '{}' http://d/containers/create; exit 1`)

	_, err := execComposeRunner{}.CreateRequest(context.Background(), composeTarget{ConfigFiles: []string{file}, WorkingDir: "/srv/w", Project: "p", Service: "web"})

	require.NoError(t, err)
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	require.NoError(t, err)
	assert.Equal(t, "compose -f "+file+" --project-directory /srv/w -p p up --no-deps --no-build --no-start --pull never --force-recreate web\n", string(args))
}

func TestCreateRequestRefusesTLSDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://docker:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	file, _ := fakeCompose(t, "exit 1")

	_, err := execComposeRunner{}.CreateRequest(context.Background(), composeTarget{ConfigFiles: []string{file}, Service: "web"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs TLS")
}
