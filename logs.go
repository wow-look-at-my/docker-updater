package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// A container that fails to start says why in its own logs, and nowhere else.
// The dashboard reported "Restarting (126)" and stopped there, so the reason
// lived only on the host: an operator had to reach a shell to read a line the
// updater already had an API for. One buildhost deployment restarted on exit 126
// for hours with that line one docker logs away and nobody able to run it.
//
// Read-only, and the tail is bounded. This surface never starts, stops or
// removes anything.

// logsMaxTail bounds a request. A caller asking for more gets this, because an
// unbounded tail turns one click into a whole container's history over the wire.
const logsMaxTail = 2000

// logsDefaultTail is what a request with no tail parameter gets: enough to hold
// a panic and the lines that led to it.
const logsDefaultTail = 200

// handleAPILogs serves GET /api/logs?container=<name>&tail=<n> as plain text.
//
// The dashboard addresses a container by NAME, not id: a name survives the
// recreate an update performs, and an id does not. Docker accepts either.
func (s *dashboardServer) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("container")
	if name == "" {
		http.Error(w, "container parameter required", http.StatusBadRequest)
		return
	}
	tail := logsDefaultTail
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "tail must be a positive number", http.StatusBadRequest)
			return
		}
		tail = min(n, logsMaxTail)
	}

	body, err := containerLogTail(r.Context(), s.cli, name, tail)
	if err != nil {
		http.Error(w, "failed to read logs: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if len(body) == 0 {
		// An empty body renders as an empty panel, which reads as a failure of
		// the dashboard rather than an answer about the container.
		body = []byte("(this container has written nothing to stdout or stderr)\n")
	}
	_, _ = w.Write(body)
}

// containerLogTail returns the last tail lines of a container's stdout and
// stderr, with Docker's stream framing removed.
func containerLogTail(ctx context.Context, cli DockerClient, name string, tail int) ([]byte, error) {
	rc, err := cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// Bounded by the tail count and a generous per-line allowance, so a
	// container logging one enormous line cannot pull the dashboard over.
	raw, err := io.ReadAll(io.LimitReader(rc, int64(tail)*8192))
	if err != nil {
		return nil, err
	}
	return demuxDockerStream(raw), nil
}

// demuxDockerStream strips the 8-byte frame header Docker puts in front of each
// chunk when the container has no TTY.
//
// The header is [stream, 0, 0, 0, len32be]. A TTY container's logs carry no
// headers at all, so a stream that does not parse as framed is returned as it
// came: guessing wrong would eat the first eight characters of every line.
func demuxDockerStream(raw []byte) []byte {
	if !looksFramed(raw) {
		return raw
	}
	var out bytes.Buffer
	for off := 0; off+8 <= len(raw); {
		size := int(binary.BigEndian.Uint32(raw[off+4 : off+8]))
		off += 8
		end := min(off+size, len(raw))
		out.Write(raw[off:end])
		off = end
	}
	return out.Bytes()
}

// looksFramed reports whether raw begins with a plausible Docker frame header.
// Byte 0 is the stream (0 stdin, 1 stdout, 2 stderr) and bytes 1-3 are padding,
// a shape ordinary log text does not have.
func looksFramed(raw []byte) bool {
	if len(raw) < 8 {
		return false
	}
	if raw[0] > 2 || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
		return false
	}
	size := int(binary.BigEndian.Uint32(raw[4:8]))
	return size > 0 && size <= len(raw)-8
}

// logExcerptTail is how far back the row's reason line looks. A process that
// dies at exec writes one line, and a few is enough to find it past whatever
// the runtime printed after.
const logExcerptTail = 20

// isFailing reports whether a container is in a state whose logs are worth
// reading on every poll: it is restarting, or it exited non-zero.
func isFailing(state, status string) bool {
	if state == "restarting" || state == "dead" {
		return true
	}
	// "Exited (0) 3 hours ago" is a job that finished, not a failure.
	return state == "exited" && !strings.HasPrefix(status, "Exited (0)")
}

// logExcerpt is the short reason line the dashboard shows beside a failing
// container, from the tail of its logs.
//
// It is the last non-blank line, minus the RFC3339 timestamp Docker prefixes.
// The last line is where a process that died says why, and it is the one an
// operator would read first after running docker logs by hand.
func logExcerpt(logs []byte) string {
	lines := strings.Split(strings.TrimRight(string(logs), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(stripLogTimestamp(lines[i]))
		if line != "" {
			return line
		}
	}
	return ""
}

// stripLogTimestamp removes the leading RFC3339Nano stamp Timestamps adds. It
// leaves a line alone unless the first field really parses as one, because a
// container's own first word is not a timestamp to be eaten.
func stripLogTimestamp(line string) string {
	first, rest, found := strings.Cut(line, " ")
	if !found || len(first) < 20 || first[4] != '-' || first[7] != '-' || first[10] != 'T' {
		return line
	}
	var y, mo, d, h, mi int
	if _, err := fmt.Sscanf(first[:16], "%4d-%2d-%2dT%2d:%2d", &y, &mo, &d, &h, &mi); err != nil {
		return line
	}
	return rest
}
